package tokens_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

func TestInstallationRepos(t *testing.T) {
	e := newEnv(t)
	e.srv.AddAccountRepos(org, "docs", "mock-docs", "mock-api", "workflows")
	e.srv.SetPageSize(3)
	ctx := context.Background()
	got, err := e.m.InstallationRepos(ctx, "author", org)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{org + "/docs", org + "/mock-api", org + "/mock-docs", org + "/workflows"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("repos = %v, want %v", got, want)
	}
	reqs := e.srv.TokenRequests()
	if len(reqs) != 1 || reqs[0].HasRepositories || string(reqs[0].Body) != `{"permissions":{"metadata":"read"}}` {
		t.Fatalf("token requests = %+v", reqs)
	}
	if n := e.srv.Count(githubtest.RouteInstallationRepos); n != 2 {
		t.Errorf("%d pages read, want 2", n)
	}
	// Cached: a second listing mints nothing.
	if _, err := e.m.InstallationRepos(ctx, "author", strings.ToUpper(org)); err != nil || len(e.srv.TokenRequests()) != 1 {
		t.Errorf("second listing: %v, %d token requests", err, len(e.srv.TokenRequests()))
	}
	// A selected installation lists its repos only.
	if got, err := e.m.InstallationRepos(ctx, "reviewer", org); err != nil || !reflect.DeepEqual(got, []string{org + "/docs", org + "/mock-docs"}) {
		t.Errorf("reviewer repos = %v, %v", got, err)
	}
	if strings.Contains(e.logs.String(), "ghs_") {
		t.Error("logs carry a token")
	}
}

func TestInstallationReposUserAccount(t *testing.T) {
	e := newEnv(t)
	e.srv.AddInstallation(githubtest.Installation{ID: 9004, AppID: reviewerApp, Owner: "octo-user", User: true, Permissions: map[string]string{"metadata": "read"}})
	e.srv.AddAccountRepos("octo-user", "mock-app")
	got, err := e.m.InstallationRepos(context.Background(), "reviewer", "octo-user")
	if err != nil || !reflect.DeepEqual(got, []string{"octo-user/mock-app"}) {
		t.Fatalf("repos = %v, %v", got, err)
	}
	if e.srv.Count(githubtest.RouteOrgInstallation) != 1 || e.srv.Count(githubtest.RouteUserInstallation) != 1 {
		t.Errorf("lookups: org %d, user %d", e.srv.Count(githubtest.RouteOrgInstallation), e.srv.Count(githubtest.RouteUserInstallation))
	}
}

func TestInstallationReposRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"unknown identity": func() error { _, err := e.m.InstallationRepos(ctx, "nobody", org); return err },
		"no owner":         func() error { _, err := e.m.InstallationRepos(ctx, "author", ""); return err },
		"owner with slash": func() error { _, err := e.m.InstallationRepos(ctx, "author", docs); return err },
	} {
		if err := call(); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.m.InstallationRepos(ctx, "author", "not-installed"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("no installation: %v, want ErrNotFound", err)
	}
	if len(e.srv.TokenRequests()) != 0 {
		t.Error("a refused listing minted a token")
	}
}

func TestGraphQLURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":          "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:8080":           "http://127.0.0.1:8080/graphql",
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := tokens.GraphQLURL(u).String(); got != want {
			t.Errorf("GraphQLURL(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestGraphQLAndDo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cred := e.mint(t, req("author", docs, map[string]string{"issues": "write"}, tokens.MaxTTL))
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 7, Labels: []string{"bug"}})
	vars := map[string]any{"owner": org, "name": "docs", "number": 7}
	var data struct {
		Repository struct {
			Object struct {
				Number int `json:"number"`
			} `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	if err := e.m.GraphQL(ctx, cred.Token, "Observe", "query Observe($owner: String!) { … }", vars, &data); err != nil || data.Repository.Object.Number != 7 {
		t.Fatalf("GraphQL = %+v, %v", data, err)
	}
	vars["number"] = 8
	err := e.m.GraphQL(ctx, cred.Token, "Observe", "query Observe { … }", vars, &data)
	var gqlErr *tokens.GraphQLError
	if !errors.Is(err, provider.ErrNotFound) || !errors.As(err, &gqlErr) || gqlErr.Type != "NOT_FOUND" {
		t.Errorf("missing object: %v, want NOT_FOUND", err)
	}
	e.srv.SetGraphQL("Limited", nil, []byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
	if err := e.m.GraphQL(ctx, cred.Token, "Limited", "query Limited { x }", nil, &data); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("rate limited: %v, want ErrRetryable", err)
	}
	e.srv.SetGraphQL("Denied", nil, []byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`))
	if err := e.m.GraphQL(ctx, cred.Token, "Denied", "query Denied { x }", nil, &data); !errors.Is(err, tokens.ErrForbidden) || !errors.Is(err, provider.ErrForbidden) {
		t.Errorf("forbidden: %v, want ErrForbidden and provider.ErrForbidden", err)
	}
	e.srv.FailNext(githubtest.RouteGraphQL, githubtest.Failure{Status: http.StatusBadGateway, Message: "Server Error"})
	if err := e.m.GraphQL(ctx, cred.Token, "Observe", "query Observe { … }", vars, &data); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("502: %v, want ErrRetryable", err)
	}
	if err := e.m.GraphQL(ctx, "", "Observe", "query Observe { … }", vars, &data); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("no token: %v, want ErrInvalid", err)
	}

	// Do: a write with a JSON body, no decoding, the error mapping.
	if _, err := e.m.Do(ctx, cred.Token, http.MethodPost, "repos/"+docs+"/issues/7/labels", nil, map[string]any{"labels": []string{"factory:x"}}, http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	if o, _ := e.srv.Object(docs, 7); !reflect.DeepEqual(o.Labels, []string{"bug", "factory:x"}) {
		t.Errorf("labels = %v", o.Labels)
	}
	if _, err := e.m.Do(ctx, cred.Token, http.MethodDelete, "repos/"+docs+"/issues/7/labels/nope", nil, nil, http.StatusOK, nil); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("remove a missing label: %v, want ErrNotFound", err)
	}
	if _, err := e.m.Do(ctx, "", http.MethodGet, "x", nil, nil, http.StatusOK, nil); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("no token: %v, want ErrInvalid", err)
	}
	if strings.Contains(e.logs.String(), cred.Token) {
		t.Error("logs carry the token")
	}
}

package tokens_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github/githubtest"
	"github.com/compliance-framework/factory/internal/provider/tokens"
)

var membersRead = map[string]string{"members": "read"}

func TestOrgToken(t *testing.T) {
	e := newEnv(t)
	e.srv.SetInstallationPermissions(authorInst, map[string]string{"contents": "write", "members": "read"})
	ctx := context.Background()
	tok, exp, err := e.m.OrgToken(ctx, "author", org, membersRead)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(t0.Add(githubtest.TokenTTL)) {
		t.Errorf("expiry = %v", exp)
	}
	reqs := e.srv.TokenRequests()
	if len(reqs) != 1 || reqs[0].HasRepositories || string(reqs[0].Body) != `{"permissions":{"members":"read"}}` || reqs[0].InstallationID != authorInst {
		t.Fatalf("token requests = %+v", reqs)
	}
	if n := e.srv.Count(githubtest.RouteOrgInstallation); n != 1 {
		t.Errorf("%d org installation lookups, want 1", n)
	}

	// Cached, and usable for org reads only.
	again, _, err := e.m.OrgToken(ctx, "author", strings.ToUpper(org), membersRead)
	if err != nil || again != tok || len(e.srv.TokenRequests()) != 1 {
		t.Errorf("second OrgToken = %q, %v; %d requests", again, err, len(e.srv.TokenRequests()))
	}
	e.srv.AddOrgMember(org, "admin", "admin")
	var members []struct {
		Login string `json:"login"`
	}
	if _, err := e.m.Get(ctx, tok, "orgs/"+org+"/members", url.Values{"role": {"admin"}}, &members); err != nil || len(members) != 1 {
		t.Errorf("Get members = %v, %v", members, err)
	}
	var rels []any
	if _, err := e.m.Get(ctx, tok, "repos/"+docs+"/releases", nil, &rels); !errors.Is(err, tokens.ErrForbidden) {
		t.Errorf("Get releases with an org token = %v, want ErrForbidden", err)
	}
	if strings.Contains(e.logs.String(), tok) {
		t.Error("logs carry the token")
	}

	// Expiry: a new token once the cached one is within RefreshBefore of GitHub's expiry.
	e.clk.Advance(githubtest.TokenTTL - tokens.RefreshBefore)
	if next, _, err := e.m.OrgToken(ctx, "author", org, membersRead); err != nil || next == tok {
		t.Errorf("after expiry: %q, %v", next, err)
	}
}

func TestOrgTokenRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"unknown identity": func() error { _, _, err := e.m.OrgToken(ctx, "nobody", org, membersRead); return err },
		"no org":           func() error { _, _, err := e.m.OrgToken(ctx, "author", "", membersRead); return err },
		"a repo":           func() error { _, _, err := e.m.OrgToken(ctx, "author", docs, membersRead); return err },
		"no permissions":   func() error { _, _, err := e.m.OrgToken(ctx, "author", org, nil); return err },
		"repo permission": func() error {
			_, _, err := e.m.OrgToken(ctx, "author", org, map[string]string{"contents": "read"})
			return err
		},
		"write": func() error {
			_, _, err := e.m.OrgToken(ctx, "author", org, map[string]string{"members": "write"})
			return err
		},
		// authorPerms has no members permission.
		"not granted": func() error { _, _, err := e.m.OrgToken(ctx, "author", org, membersRead); return err },
	} {
		if err := call(); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if n := len(e.srv.TokenRequests()); n != 0 {
		t.Errorf("%d token requests, want none", n)
	}
	if _, _, err := e.m.OrgToken(ctx, "author", "nowhere", membersRead); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("org without the App: %v, want ErrNotFound", err)
	}
}

func TestAppAndAppGet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.srv.SetSlug(authorApp, "ccf-factory")
	app, err := e.m.App(ctx, "author")
	if err != nil || app.ID != authorApp || app.Slug != "ccf-factory" || app.BotLogin() != "ccf-factory[bot]" {
		t.Errorf("App = %+v, %v", app, err)
	}
	if _, err := e.m.App(ctx, "nobody"); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("App(unknown) = %v", err)
	}
	e.srv.AddDelivery(authorApp, githubtest.Delivery{ID: 7, GUID: "g", DeliveredAt: t0, Event: "ping"})
	var ds []struct {
		GUID string `json:"guid"`
	}
	h, err := e.m.AppGet(ctx, "author", "app/hook/deliveries", url.Values{"per_page": {"1"}}, &ds)
	if err != nil || len(ds) != 1 || ds[0].GUID != "g" || h == nil {
		t.Errorf("AppGet = %v, %v", ds, err)
	}
	if _, err := e.m.AppGet(ctx, "reviewer", "app/hook/deliveries", nil, &ds); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("AppGet without a webhook = %v, want ErrNotFound", err)
	}
	if _, err := e.m.Get(ctx, "", "orgs/x/members", nil, &ds); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("Get without a token = %v", err)
	}
	if _, err := e.m.Get(ctx, "ghs_unknown", "orgs/x/members", nil, &ds); !errors.Is(err, tokens.ErrUnauthorized) {
		t.Errorf("Get with an unknown token = %v", err)
	}
	e.srv.FailNext(githubtest.RouteDeliveries, githubtest.Failure{Status: 429, Header: map[string][]string{"Retry-After": {"7"}}})
	_, err = e.m.AppGet(ctx, "author", "app/hook/deliveries", nil, &ds)
	var re *provider.RetryableError
	if !errors.As(err, &re) || re.RetryAfter != 7*time.Second {
		t.Errorf("rate-limited AppGet = %v", err)
	}
}

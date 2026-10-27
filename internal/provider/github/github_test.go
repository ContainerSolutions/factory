package github_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

const (
	authorApp   = 101
	reviewerApp = 202
	org         = "compliance-framework"
	docs        = org + "/docs"
	prov        = "github-public"
	// hookSecret signs the test webhooks.
	hookSecret = "test-webhook-secret"
	// uiBase is the factory's public URL the markers link to.
	uiBase = "https://factory.example"
)

var t0 = time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)

type env struct {
	srv     *githubtest.Server
	clk     *clock.Fake
	adapter *github.Adapter
	secrets map[string][]byte
	p       v1alpha1.Provider
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	srv := githubtest.NewServer(t, clk)
	authorKey, authorRSA := githubtest.AppKey(t)
	reviewerKey, reviewerRSA := githubtest.NewAppKey(t)
	srv.AddApp(authorApp, &authorRSA.PublicKey)
	srv.AddApp(reviewerApp, &reviewerRSA.PublicKey)
	srv.AddInstallation(githubtest.Installation{ID: 1, AppID: authorApp, Owner: org, Permissions: map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read", "members": "read",
		"checks": "read", "actions": "read", "deployments": "read",
	}})
	srv.AddInstallation(githubtest.Installation{ID: 2, AppID: reviewerApp, Owner: org, Permissions: map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "read", "metadata": "read",
	}})
	srv.SetSlug(authorApp, "ccf-factory")
	srv.SetSlug(reviewerApp, "ccf-factory-reviewer")
	return &env{
		srv: srv,
		clk: clk,
		adapter: github.NewAdapter(github.Options{
			HTTPClient: srv.Client(), Clock: clk, Groups: provider.StaticGroups(testGroups), UIBaseURL: uiBase,
		}),
		secrets: map[string][]byte{
			"ccf-factory/app_private_key":          authorKey,
			"ccf-factory-reviewer/app_private_key": reviewerKey,
			"github-public/webhook_secret":         []byte(hookSecret),
		},
		p: v1alpha1.Provider{
			TypeMeta: v1alpha1.TypeMeta{Kind: "Provider"},
			Metadata: v1alpha1.ObjectMeta{Name: "github-public"},
			Spec: v1alpha1.ProviderSpec{
				Type: v1alpha1.ProviderTypeGitHub,
				GitHub: &v1alpha1.GitHubProviderSpec{
					APIURL:  srv.URL,
					Webhook: v1alpha1.Webhook{Path: "/hooks/github-public", SecretRef: v1alpha1.SecretRef{Name: "github-public", Key: "webhook_secret"}},
					Identities: map[string]v1alpha1.AppIdentity{
						"author":   {AppID: authorApp, PrivateKeyRef: v1alpha1.SecretRef{Name: "ccf-factory", Key: "app_private_key"}},
						"reviewer": {AppID: reviewerApp, PrivateKeyRef: v1alpha1.SecretRef{Name: "ccf-factory-reviewer", Key: "app_private_key"}},
					},
				},
			},
		},
	}
}

// testGroups are the catalog Groups trust rules name in the tests.
var testGroups = map[string]v1alpha1.GroupSpec{
	"maintainers": {Members: []v1alpha1.GroupSource{
		{Provider: "slack-ccf", UserGroup: "@maintainers"}, // another provider's source: skipped
		{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: org, Role: "admin"}},
	}},
	"docs-team": {Members: []v1alpha1.GroupSource{
		{Provider: prov, Team: org + "/no-such-team"}, // unresolvable: not a member
		{Provider: prov, Team: org + "/docs"},
	}, Cache: &v1alpha1.GroupCache{TTL: "1m"}},
}

func (e *env) resolver() provider.SecretResolver {
	return provider.SecretResolverFunc(func(_ context.Context, ref v1alpha1.SecretRef) ([]byte, error) {
		v, ok := e.secrets[ref.Name+"/"+ref.Key]
		if !ok {
			return nil, fmt.Errorf("secret %s/%s not set", ref.Name, ref.Key)
		}
		return v, nil
	})
}

func (e *env) connect(t *testing.T) *github.Conn {
	t.Helper()
	c, err := e.adapter.Connect(context.Background(), e.p, e.resolver())
	if err != nil {
		t.Fatal(err)
	}
	return c.(*github.Conn)
}

func TestConnectViaRegistry(t *testing.T) {
	e := newEnv(t)
	reg, err := provider.NewRegistry(e.adapter)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Types(); len(got) != 1 || got[0] != "github" {
		t.Fatalf("Types = %v", got)
	}
	if _, err := reg.Connect(context.Background(), e.p, e.resolver()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Errorf("Connect made %d GitHub calls, want none", n)
	}
}

func TestConnectRefused(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*env)
		want   error
	}{
		{"no name", func(e *env) { e.p.Metadata.Name = "" }, provider.ErrInvalid},
		{"wrong type", func(e *env) { e.p.Spec.Type = v1alpha1.ProviderTypeSlack }, provider.ErrInvalid},
		{"no github spec", func(e *env) { e.p.Spec.GitHub = nil }, provider.ErrInvalid},
		{"no identities", func(e *env) { e.p.Spec.GitHub.Identities = nil }, provider.ErrInvalid},
		{"incomplete secret ref", func(e *env) {
			e.p.Spec.GitHub.Identities["author"] = v1alpha1.AppIdentity{AppID: authorApp, PrivateKeyRef: v1alpha1.SecretRef{Name: "ccf-factory"}}
		}, provider.ErrInvalid},
		{"no app id", func(e *env) {
			id := e.p.Spec.GitHub.Identities["reviewer"]
			id.AppID = 0
			e.p.Spec.GitHub.Identities["reviewer"] = id
		}, provider.ErrInvalid},
		{"bad key", func(e *env) { e.secrets["ccf-factory/app_private_key"] = []byte("s3cr3t but not PEM") }, provider.ErrInvalid},
		{"bad API URL", func(e *env) { e.p.Spec.GitHub.APIURL = "file:///etc" }, provider.ErrInvalid},
		{"missing secret", func(e *env) { delete(e.secrets, "ccf-factory-reviewer/app_private_key") }, nil},
		{"missing webhook secret", func(e *env) { delete(e.secrets, "github-public/webhook_secret") }, nil},
		{"empty webhook secret", func(e *env) { e.secrets["github-public/webhook_secret"] = nil }, provider.ErrInvalid},
		{"incomplete webhook secret ref", func(e *env) { e.p.Spec.GitHub.Webhook.SecretRef.Key = "" }, provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.p.Spec.GitHub.Identities = maps.Clone(e.p.Spec.GitHub.Identities)
			tt.mutate(e)
			_, err := e.adapter.Connect(context.Background(), e.p, e.resolver())
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("Connect = %v, want %v", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "-----BEGIN") {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
	e := newEnv(t)
	if _, err := e.adapter.Connect(context.Background(), e.p, nil); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("nil resolver: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.adapter.Connect(ctx, e.p, e.resolver()); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestMintGitCredential(t *testing.T) {
	e := newEnv(t)
	c := e.connect(t)
	cred, err := c.MintGitCredential(context.Background(), provider.GitCredentialRequest{
		Identity: "author", Repo: docs, Permissions: map[string]string{"contents": "write", "pull_requests": "write"}, TTL: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Provider != "github-public" || cred.Host != "127.0.0.1:"+strings.Split(e.srv.URL, ":")[2] ||
		cred.Username != "x-access-token" || cred.Token != e.srv.Tokens()[0] || cred.Repo != docs ||
		!cred.ExpiresAt.Equal(t0.Add(30*time.Minute)) || cred.Permissions["contents"] != "write" {
		t.Errorf("credential = %+v", cred)
	}
	if strings.Contains(fmt.Sprint(cred), cred.Token) {
		t.Error("credential prints its token")
	}
	body := string(e.srv.TokenRequests()[0].Body)
	if body != `{"repositories":["docs"],"permissions":{"contents":"write","pull_requests":"write"}}` {
		t.Errorf("body = %s", body)
	}

	ro, err := c.MintGitCredential(context.Background(), provider.ReadOnlyRequest("reviewer", org+"/notes", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ro.Permissions["contents"] != "read" || len(ro.Permissions) != 1 || ro.Repo != org+"/notes" {
		t.Errorf("read-only credential = %+v", ro)
	}
	if body := string(e.srv.TokenRequests()[1].Body); body != `{"repositories":["notes"],"permissions":{"contents":"read"}}` {
		t.Errorf("read-only body = %s", body)
	}

	for _, r := range []provider.GitCredentialRequest{
		provider.ReadOnlyRequest("author", "", time.Hour),
		{Identity: "author", Repo: docs, ReadRepos: []string{org + "/notes"}, Permissions: provider.ReadOnlyPermissions(), TTL: time.Hour},
		{Identity: "reviewer", Repo: docs, Permissions: map[string]string{"issues": "write"}, TTL: time.Hour},
	} {
		if _, err := c.MintGitCredential(context.Background(), r); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("MintGitCredential(%+v) = %v, want ErrInvalid", r, err)
		}
	}
	if n := len(e.srv.TokenRequests()); n != 2 {
		t.Errorf("%d token requests, want 2", n)
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	c := e.connect(t)
	if h := c.Health(); h.Status != provider.HealthUnknown {
		t.Errorf("before any check: %+v", h)
	}
	if h := c.CheckHealth(context.Background()); h.Status != provider.HealthOK || !h.CheckedAt.Equal(t0) {
		t.Errorf("CheckHealth = %+v, want ok", h)
	}
	if n := e.srv.Count(githubtest.RouteApp); n != 2 {
		t.Errorf("%d GET /app, want one per identity", n)
	}

	// A refused request says nothing about the identity.
	_, _ = c.MintGitCredential(context.Background(), provider.ReadOnlyRequest("author", "", time.Hour))
	e.srv.FailNext(githubtest.RouteAccessToken, githubtest.Failure{Status: 404})
	_, _ = c.MintGitCredential(context.Background(), provider.ReadOnlyRequest("author", docs, time.Hour))
	if h := c.Health(); h.Status != provider.HealthOK {
		t.Errorf("after request errors: %+v, want ok", h)
	}

	// GitHub rejecting one identity's JWT: degraded; both: down.
	e.clk.Advance(time.Minute)
	e.srv.FailNext(githubtest.RouteInstallation, githubtest.Failure{Status: 401, Message: "Bad credentials"})
	_, err := c.MintGitCredential(context.Background(), provider.ReadOnlyRequest("author", org+"/other", time.Hour))
	if !errors.Is(err, tokens.ErrUnauthorized) {
		t.Fatalf("mint = %v, want ErrUnauthorized", err)
	}
	h := c.Health()
	if h.Status != provider.HealthDegraded || !strings.Contains(h.Message, "author") || !h.CheckedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("one identity failing: %+v, want degraded", h)
	}
	e.srv.FailNext(githubtest.RouteApp, githubtest.Failure{Status: 502})
	e.srv.FailNext(githubtest.RouteApp, githubtest.Failure{Status: 502})
	if h := c.CheckHealth(context.Background()); h.Status != provider.HealthDown || !strings.Contains(h.Message, "reviewer") {
		t.Errorf("both failing: %+v, want down", h)
	}

	// Recovery: a successful mint marks the identity ok again.
	if _, err := c.MintGitCredential(context.Background(), provider.ReadOnlyRequest("author", docs, time.Hour)); err != nil {
		t.Fatal(err)
	}
	if h := c.Health(); h.Status != provider.HealthDegraded {
		t.Errorf("author recovered: %+v, want degraded", h)
	}
	if h := c.CheckHealth(context.Background()); h.Status != provider.HealthOK || h.Message != "" {
		t.Errorf("all recovered: %+v, want ok", h)
	}

	// A check we abandoned records nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h := c.CheckHealth(ctx); h.Status != provider.HealthOK {
		t.Errorf("cancelled check: %+v, want ok unchanged", h)
	}
}

package tokens_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

const (
	authorApp     = 101
	reviewerApp   = 202
	authorInst    = 9001
	reviewerInst  = 9002
	otherInst     = 9003
	org           = "compliance-framework"
	providerName  = "github-public"
	docs          = org + "/docs"
	notes         = org + "/notes"
	elsewhereRepo = "someone-else/tool"
)

var t0 = time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)

// The Apps' installation permissions, as in rollout.md §GitHub Apps.
var (
	authorPerms = map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"checks": "read", "actions": "read", "deployments": "read",
	}
	reviewerPerms = map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "read", "metadata": "read", "checks": "read",
	}
	write = map[string]string{"contents": "write", "pull_requests": "write"}
)

type env struct {
	srv  *githubtest.Server
	clk  *clock.Fake
	m    *tokens.Minter
	logs *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newEnv(t *testing.T, mutate ...func(*tokens.Options)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	srv := githubtest.NewServer(t, clk)
	authorKey, authorRSA := githubtest.AppKey(t)
	reviewerKey, reviewerRSA := githubtest.NewAppKey(t)
	srv.AddApp(authorApp, &authorRSA.PublicKey)
	srv.AddApp(reviewerApp, &reviewerRSA.PublicKey)
	srv.AddInstallation(githubtest.Installation{ID: authorInst, AppID: authorApp, Owner: org, Permissions: authorPerms})
	srv.AddInstallation(githubtest.Installation{ID: reviewerInst, AppID: reviewerApp, Owner: org, Repos: []string{"docs", "mock-docs"}, Permissions: reviewerPerms})
	srv.AddInstallation(githubtest.Installation{ID: otherInst, AppID: authorApp, Owner: "someone-else", Permissions: map[string]string{"contents": "read", "metadata": "read"}})
	logs := &lockedBuffer{}
	opts := tokens.Options{
		APIURL:     srv.URL,
		HTTPClient: srv.Client(),
		Clock:      clk,
		Logger:     slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Provider:   providerName,
		Host:       "github.com",
	}
	for _, f := range mutate {
		f(&opts)
	}
	m, err := tokens.New(map[string]tokens.Identity{
		"author":   {AppID: authorApp, PrivateKey: authorKey},
		"reviewer": {AppID: reviewerApp, PrivateKey: reviewerKey},
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, clk: clk, m: m, logs: logs}
}

func req(identity, repo string, perms map[string]string, ttl time.Duration) provider.GitCredentialRequest {
	return provider.GitCredentialRequest{Identity: identity, Repo: repo, Permissions: maps.Clone(perms), TTL: ttl}
}

func (e *env) mint(t *testing.T, r provider.GitCredentialRequest) provider.Credential {
	t.Helper()
	c, err := e.m.Token(context.Background(), r)
	if err != nil {
		t.Fatalf("Token(%v) = %v", r, err)
	}
	return c
}

// TestTokenBody: the access-token body always names exactly one repo and exactly the
// requested permissions, and the credential carries the scope.
func TestTokenBody(t *testing.T) {
	tests := []struct {
		name     string
		req      provider.GitCredentialRequest
		wantInst int64
		wantBody string
	}{
		{
			name:     "task token",
			req:      req("author", docs, write, time.Hour),
			wantInst: authorInst,
			wantBody: `{"repositories":["docs"],"permissions":{"contents":"write","pull_requests":"write"}}`,
		},
		{
			name:     "read-only token",
			req:      provider.ReadOnlyRequest("author", notes, 30*time.Minute),
			wantInst: authorInst,
			wantBody: `{"repositories":["notes"],"permissions":{"contents":"read"}}`,
		},
		{
			name:     "reviewer token",
			req:      req("reviewer", docs, map[string]string{"pull_requests": "write", "issues": "read"}, time.Hour),
			wantInst: reviewerInst,
			wantBody: `{"repositories":["docs"],"permissions":{"issues":"read","pull_requests":"write"}}`,
		},
		{
			name:     "another account's installation",
			req:      provider.ReadOnlyRequest("author", elsewhereRepo, time.Hour),
			wantInst: otherInst,
			wantBody: `{"repositories":["tool"],"permissions":{"contents":"read"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			cred := e.mint(t, tt.req)
			trs := e.srv.TokenRequests()
			if len(trs) != 1 {
				t.Fatalf("%d token requests, want 1", len(trs))
			}
			tr := trs[0]
			if got := string(tr.Body); got != tt.wantBody {
				t.Errorf("body = %s\nwant   %s", got, tt.wantBody)
			}
			if !tr.HasRepositories || len(tr.Repositories) != 1 {
				t.Errorf("repositories = %v (present %v), want exactly one", tr.Repositories, tr.HasRepositories)
			}
			if tr.InstallationID != tt.wantInst {
				t.Errorf("installation = %d, want %d", tr.InstallationID, tt.wantInst)
			}
			want := provider.Credential{
				Provider:    providerName,
				Host:        "github.com",
				Username:    "x-access-token",
				Token:       e.srv.Tokens()[0],
				ExpiresAt:   t0.Add(min(tt.req.TTL, time.Hour)),
				Repo:        tt.req.Repo,
				Permissions: tt.req.Permissions,
			}
			if cred.Token != want.Token || cred.Provider != want.Provider || cred.Host != want.Host ||
				cred.Username != want.Username || !cred.ExpiresAt.Equal(want.ExpiresAt) || cred.Repo != want.Repo ||
				!maps.Equal(cred.Permissions, want.Permissions) || len(cred.ReadRepos) != 0 {
				t.Errorf("credential = %+v (token match %v)\nwant %+v", cred, cred.Token == want.Token, want)
			}
		})
	}
}

// TestTokenRefused: what can't be minted never reaches GitHub's token endpoint.
func TestTokenRefused(t *testing.T) {
	tests := []struct {
		name string
		req  provider.GitCredentialRequest
	}{
		{"unscoped", req("author", "", write, time.Hour)},
		{"unscoped read-only", provider.ReadOnlyRequest("author", "", time.Hour)},
		{"read repos on a read-only token", provider.GitCredentialRequest{
			Identity: "author", Repo: docs, ReadRepos: []string{notes}, Permissions: provider.ReadOnlyPermissions(), TTL: time.Hour,
		}},
		{"read repos on a write token", provider.GitCredentialRequest{
			Identity: "author", Repo: docs, ReadRepos: []string{notes}, Permissions: write, TTL: time.Hour,
		}},
		{"no permissions", req("author", docs, nil, time.Hour)},
		{"no identity", req("", docs, write, time.Hour)},
		{"unknown identity", req("admin", docs, write, time.Hour)},
		{"no ttl", req("author", docs, write, 0)},
		{"repo without owner", req("author", "docs", write, time.Hour)},
		{"repo with a path", req("author", docs+"/extra", write, time.Hour)},
		{"empty owner", req("author", "/docs", write, time.Hour)},
		{"unknown level", req("author", docs, map[string]string{"contents": "owner"}, time.Hour)},
		{"bad permission name", req("author", docs, map[string]string{"Contents": "read"}, time.Hour)},
		{"superset: a level above the grant", req("reviewer", docs, map[string]string{"issues": "write"}, time.Hour)},
		{"superset: a permission not granted", req("reviewer", docs, map[string]string{"contents": "read", "administration": "write"}, time.Hour)},
		{"superset: admin", req("author", docs, map[string]string{"contents": "admin"}, time.Hour)},
		{"superset on another account", req("author", elsewhereRepo, write, time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			cred, err := e.m.Token(context.Background(), tt.req)
			if !errors.Is(err, provider.ErrInvalid) {
				t.Fatalf("Token = %v, %v; want ErrInvalid", cred, err)
			}
			if cred.Token != "" {
				t.Errorf("a refused request returned a token")
			}
			if n := e.srv.Count(githubtest.RouteAccessToken); n != 0 {
				t.Errorf("%d token requests reached GitHub", n)
			}
		})
	}
}

// TestTokenCache: reuse per (identity, repo, permissions) until 5 minutes before expiry.
func TestTokenCache(t *testing.T) {
	e := newEnv(t)
	first := e.mint(t, req("author", docs, write, time.Hour))

	// Same key, permissions in another map order, repo in another case: a hit.
	again := e.mint(t, req("author", "Compliance-Framework/Docs", map[string]string{"pull_requests": "write", "contents": "write"}, time.Hour))
	if again.Token != first.Token {
		t.Error("same (identity, repo, permissions) minted a new token")
	}
	if n := e.srv.Count(githubtest.RouteAccessToken); n != 1 {
		t.Fatalf("%d token requests, want 1", n)
	}

	// A different key: a new token each.
	for _, r := range []provider.GitCredentialRequest{
		req("author", docs, map[string]string{"contents": "write"}, time.Hour),
		req("author", notes, write, time.Hour),
		req("reviewer", docs, write, time.Hour),
		provider.ReadOnlyRequest("author", docs, time.Hour),
	} {
		if c := e.mint(t, r); c.Token == first.Token {
			t.Errorf("%v reused the token of another scope", r)
		}
	}
	if n := e.srv.Count(githubtest.RouteAccessToken); n != 5 {
		t.Fatalf("%d token requests, want 5", n)
	}

	// Still reused just before the 5-minute margin, with GitHub's expiry as the bound.
	e.clk.Set(t0.Add(55*time.Minute - time.Second))
	hit := e.mint(t, req("author", docs, write, time.Hour))
	if hit.Token != first.Token {
		t.Fatal("token not reused 5m01s before expiry")
	}
	if !hit.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("reused ExpiresAt = %s, want GitHub's %s", hit.ExpiresAt, t0.Add(time.Hour))
	}

	// 5 minutes before expiry: a fresh token.
	e.clk.Set(t0.Add(55 * time.Minute))
	fresh := e.mint(t, req("author", docs, write, time.Hour))
	if fresh.Token == first.Token {
		t.Fatal("token reused 5 minutes before expiry")
	}
	if !fresh.ExpiresAt.Equal(t0.Add(55*time.Minute + time.Hour)) {
		t.Errorf("fresh ExpiresAt = %s", fresh.ExpiresAt)
	}
	if n := e.srv.Count(githubtest.RouteAccessToken); n != 6 {
		t.Fatalf("%d token requests, want 6", n)
	}
}

// TestTokenTTL: ExpiresAt is at most min(TTL, 1h) from now and never after GitHub's expiry.
func TestTokenTTL(t *testing.T) {
	e := newEnv(t)
	if c := e.mint(t, req("author", docs, write, 2*time.Hour)); !c.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("2h TTL: ExpiresAt = %s, want capped at 1h", c.ExpiresAt)
	}
	e.clk.Advance(10 * time.Minute)
	if c := e.mint(t, req("author", docs, write, 20*time.Minute)); !c.ExpiresAt.Equal(t0.Add(30 * time.Minute)) {
		t.Errorf("20m TTL: ExpiresAt = %s, want now+20m", c.ExpiresAt)
	}
	if c := e.mint(t, req("author", docs, write, time.Hour)); !c.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("1h TTL on a token with 50m left: ExpiresAt = %s, want GitHub's expiry", c.ExpiresAt)
	}
	if n := e.srv.Count(githubtest.RouteAccessToken); n != 1 {
		t.Errorf("%d token requests, want 1", n)
	}
}

// TestConcurrentMintsDedupe: callers asking for the same scope at once share one GitHub call.
func TestConcurrentMintsDedupe(t *testing.T) {
	e := newEnv(t)
	release := e.srv.Gate()
	defer release()
	const n = 8
	r := req("author", docs, write, time.Hour)
	creds := make([]provider.Credential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { creds[i], errs[i] = e.m.Token(context.Background(), r) })
	}
	waitFor(t, func() bool { return e.srv.InFlight() == 1 && e.m.Waiters("author", docs, write) == n-1 })
	release()
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if creds[i].Token != creds[0].Token {
			t.Errorf("caller %d got another token", i)
		}
	}
	if got := e.srv.Count(githubtest.RouteAccessToken); got != 1 {
		t.Errorf("%d token requests, want 1", got)
	}
	if got := e.srv.Count(githubtest.RouteInstallation); got != 1 {
		t.Errorf("%d installation lookups, want 1", got)
	}
}

// TestConcurrentMintsMixed: many scopes at once, under -race.
func TestConcurrentMintsMixed(t *testing.T) {
	e := newEnv(t)
	reqs := []provider.GitCredentialRequest{
		req("author", docs, write, time.Hour),
		provider.ReadOnlyRequest("author", notes, time.Hour),
		req("reviewer", docs, write, time.Hour),
		provider.ReadOnlyRequest("author", elsewhereRepo, time.Hour),
	}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			if _, err := e.m.Token(context.Background(), reqs[i%len(reqs)]); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := e.srv.Count(githubtest.RouteAccessToken); got > 40 || got < len(reqs) {
		t.Errorf("%d token requests", got)
	}
}

// A follower whose leader gives up (its context ends) mints on its own.
func TestLeaderCancelled(t *testing.T) {
	e := newEnv(t)
	release := e.srv.Gate()
	r := req("author", docs, write, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	var leaderErr, followerErr error
	var follower provider.Credential
	var wg sync.WaitGroup
	wg.Go(func() { _, leaderErr = e.m.Token(ctx, r) })
	waitFor(t, func() bool { return e.srv.InFlight() == 1 })
	wg.Go(func() { follower, followerErr = e.m.Token(context.Background(), r) })
	waitFor(t, func() bool { return e.m.Waiters("author", docs, write) == 1 })
	cancel()
	// The follower now becomes the leader of a second request, held by the gate too.
	waitFor(t, func() bool { return e.srv.Count(githubtest.RouteAccessToken) == 2 })
	release()
	wg.Wait()
	if !errors.Is(leaderErr, context.Canceled) {
		t.Errorf("leader: %v, want context.Canceled", leaderErr)
	}
	if followerErr != nil || follower.Token == "" {
		t.Errorf("follower: %v, %v", follower, followerErr)
	}
}

// TestInstallationCache: one lookup per (identity, account), reused across repos, refreshed
// after InstallationTTL and after GitHub says it changed.
func TestInstallationCache(t *testing.T) {
	e := newEnv(t, func(o *tokens.Options) { o.InstallationTTL = 30 * time.Minute })
	e.mint(t, req("author", docs, write, time.Hour))
	e.mint(t, provider.ReadOnlyRequest("author", notes, time.Hour))
	e.mint(t, provider.ReadOnlyRequest("author", org+"/mock-docs", time.Hour))
	if n := e.srv.Count(githubtest.RouteInstallation); n != 1 {
		t.Fatalf("%d lookups for one account, want 1", n)
	}
	e.mint(t, req("reviewer", docs, write, time.Hour))
	e.mint(t, provider.ReadOnlyRequest("author", elsewhereRepo, time.Hour))
	if n := e.srv.Count(githubtest.RouteInstallation); n != 3 {
		t.Fatalf("%d lookups, want 3 (one per identity and account)", n)
	}

	e.clk.Advance(30 * time.Minute)
	e.mint(t, req("author", docs, map[string]string{"issues": "write"}, time.Hour))
	if n := e.srv.Count(githubtest.RouteInstallation); n != 4 {
		t.Fatalf("%d lookups after the installation TTL, want 4", n)
	}

	// A 404 on the token endpoint (App uninstalled and reinstalled): look up again next time.
	e.srv.FailNext(githubtest.RouteAccessToken, githubtest.Failure{Status: http.StatusNotFound, Message: "Not Found"})
	if _, err := e.m.Token(context.Background(), req("author", docs, map[string]string{"checks": "read"}, time.Hour)); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Token = %v, want ErrNotFound", err)
	}
	e.mint(t, req("author", docs, map[string]string{"checks": "read"}, time.Hour))
	if n := e.srv.Count(githubtest.RouteInstallation); n != 5 {
		t.Fatalf("%d lookups after a 404, want 5", n)
	}
}

// A permission granted after the installation was cached is seen without waiting for the TTL.
func TestInstallationPermissionsChange(t *testing.T) {
	e := newEnv(t)
	e.mint(t, req("reviewer", docs, write, time.Hour))
	grown := maps.Clone(reviewerPerms)
	grown["issues"] = "write"
	e.srv.SetInstallationPermissions(reviewerInst, grown)
	e.mint(t, req("reviewer", docs, map[string]string{"issues": "write"}, time.Hour))
	if n := e.srv.Count(githubtest.RouteInstallation); n != 2 {
		t.Errorf("%d lookups, want 2 (the refusal looked again)", n)
	}
	// Still refused once the fresh lookup says no.
	if _, err := e.m.Token(context.Background(), req("reviewer", docs, map[string]string{"actions": "read"}, time.Hour)); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("Token = %v, want ErrInvalid", err)
	}
}

// TestJWT: every App call carries a valid JWT for its App, valid for minutes, not hours.
func TestJWT(t *testing.T) {
	e := newEnv(t)
	e.mint(t, req("author", docs, write, time.Hour))
	e.mint(t, req("reviewer", docs, write, time.Hour))
	if err := e.m.CheckApp(context.Background(), "reviewer"); err != nil {
		t.Fatal(err)
	}
	reqs := e.srv.Requests()
	if len(reqs) != 5 {
		t.Fatalf("%d requests, want 5", len(reqs))
	}
	now := time.Now()
	for _, r := range reqs {
		c := r.Claims
		if c.Issuer != "101" && c.Issuer != "202" {
			t.Errorf("%s %s: issuer %q (the JWT didn't verify)", r.Method, r.Path, c.Issuer)
			continue
		}
		if life := c.ExpiresAt.Sub(c.IssuedAt); life <= 0 || life > 10*time.Minute {
			t.Errorf("%s %s: JWT valid for %s", r.Method, r.Path, life)
		}
		if !c.ExpiresAt.After(now.Add(-time.Minute)) || c.ExpiresAt.After(now.Add(10*time.Minute)) {
			t.Errorf("%s %s: JWT expires at %s (now %s)", r.Method, r.Path, c.ExpiresAt, now)
		}
	}
	if reqs[2].Claims.Issuer != "202" {
		t.Errorf("reviewer call signed as %q", reqs[2].Claims.Issuer)
	}
}

// TestWrongKey: GitHub rejecting the JWT is ErrUnauthorized.
func TestWrongKey(t *testing.T) {
	clk := clock.NewFake(t0)
	srv := githubtest.NewServer(t, clk)
	_, registered := githubtest.AppKey(t)
	other, _ := githubtest.NewAppKey(t)
	srv.AddApp(authorApp, &registered.PublicKey)
	m, err := tokens.New(map[string]tokens.Identity{"author": {AppID: authorApp, PrivateKey: other}},
		tokens.Options{APIURL: srv.URL, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckApp(context.Background(), "author"); !errors.Is(err, tokens.ErrUnauthorized) {
		t.Errorf("CheckApp = %v, want ErrUnauthorized", err)
	}
	if _, err := m.Token(context.Background(), req("author", docs, write, time.Hour)); !errors.Is(err, tokens.ErrUnauthorized) {
		t.Errorf("Token = %v, want ErrUnauthorized", err)
	}
}

// TestErrors: GitHub's statuses map to typed errors; transient ones are retryable.
func TestErrors(t *testing.T) {
	reset := t0.Add(90 * time.Second)
	tests := []struct {
		name       string
		route      string
		failure    githubtest.Failure
		want       error
		retryable  bool
		retryAfter time.Duration
	}{
		{name: "401", route: githubtest.RouteAccessToken, failure: githubtest.Failure{Status: 401, Message: "Bad credentials"}, want: tokens.ErrUnauthorized},
		{name: "403", route: githubtest.RouteAccessToken, failure: githubtest.Failure{Status: 403, Message: "Resource not accessible by integration"}, want: tokens.ErrForbidden},
		{name: "404", route: githubtest.RouteAccessToken, failure: githubtest.Failure{Status: 404, Message: "Not Found"}, want: provider.ErrNotFound},
		{name: "422", route: githubtest.RouteAccessToken, failure: githubtest.Failure{Status: 422, Message: "The permissions requested are not granted to this installation."}, want: provider.ErrInvalid},
		{name: "installation 404", route: githubtest.RouteInstallation, failure: githubtest.Failure{Status: 404, Message: "Not Found"}, want: provider.ErrNotFound},
		{name: "500", route: githubtest.RouteAccessToken, failure: githubtest.Failure{Status: 500}, want: provider.ErrRetryable, retryable: true},
		{name: "502 on lookup", route: githubtest.RouteInstallation, failure: githubtest.Failure{Status: 502}, want: provider.ErrRetryable, retryable: true},
		{name: "429 retry-after", route: githubtest.RouteAccessToken, want: provider.ErrRetryable, retryable: true, retryAfter: 30 * time.Second,
			failure: githubtest.Failure{Status: 429, Header: http.Header{"Retry-After": {"30"}}}},
		{name: "403 primary limit", route: githubtest.RouteAccessToken, want: provider.ErrRetryable, retryable: true, retryAfter: 90 * time.Second,
			failure: githubtest.Failure{Status: 403, Message: "API rate limit exceeded", Header: http.Header{
				"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)},
			}}},
		{name: "403 secondary limit", route: githubtest.RouteAccessToken, want: provider.ErrRetryable, retryable: true,
			failure: githubtest.Failure{Status: 403, Message: "You have exceeded a secondary rate limit."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.FailNext(tt.route, tt.failure)
			_, err := e.m.Token(context.Background(), req("author", docs, write, time.Hour))
			if !errors.Is(err, tt.want) {
				t.Fatalf("Token = %v, want %v", err, tt.want)
			}
			// Only a 403 that isn't a rate limit is forbidden, for the provider too.
			if got, want := errors.Is(err, provider.ErrForbidden), tt.name == "403"; got != want || errors.Is(err, tokens.ErrForbidden) != want {
				t.Errorf("forbidden = %v, want %v (%v)", got, want, err)
			}
			if got := errors.Is(err, provider.ErrRetryable); got != tt.retryable {
				t.Errorf("retryable = %v, want %v (%v)", got, tt.retryable, err)
			}
			var re *provider.RetryableError
			if errors.As(err, &re) && re.RetryAfter != tt.retryAfter {
				t.Errorf("RetryAfter = %s, want %s", re.RetryAfter, tt.retryAfter)
			}
			var apiErr *tokens.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.failure.Status || apiErr.RequestID == "" {
				t.Errorf("APIError = %+v", apiErr)
			}
			// The failure isn't cached: the next call mints.
			e.mint(t, req("author", docs, write, time.Hour))
		})
	}
}

func TestTransportErrors(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.m.Token(ctx, req("author", docs, write, time.Hour))
	if !errors.Is(err, context.Canceled) || errors.Is(err, provider.ErrRetryable) {
		t.Errorf("cancelled: %v, want context.Canceled, not retryable", err)
	}
	e.srv.Close()
	_, err = e.m.Token(context.Background(), req("author", docs, write, time.Hour))
	if !errors.Is(err, provider.ErrRetryable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("server down: %v, want ErrRetryable", err)
	}
}

// TestScopeMismatch: a token GitHub scoped differently from the request is never handed out.
func TestScopeMismatch(t *testing.T) {
	keyPEM, _ := githubtest.AppKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 1, "permissions": {"contents": "write"}}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token": "ghs_wide", "expires_at": "2026-10-16T12:00:00Z",
				"repositories": [{"name": "docs"}, {"name": "api"}]}`))
		}
	}))
	defer srv.Close()
	m, err := tokens.New(map[string]tokens.Identity{"author": {AppID: 1, PrivateKey: keyPEM}},
		tokens.Options{APIURL: srv.URL, Clock: clock.NewFake(t0)})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := m.Token(context.Background(), req("author", docs, map[string]string{"contents": "write"}, time.Hour))
	if err == nil || cred.Token != "" {
		t.Fatalf("Token = %v, %v; want an error and no token", cred, err)
	}
	if strings.Contains(err.Error(), "ghs_wide") {
		t.Errorf("error leaks the token: %v", err)
	}
}

// TestNoRedirects: the JWT never follows a redirect off the API host.
func TestNoRedirects(t *testing.T) {
	keyPEM, _ := githubtest.AppKey(t)
	var leaked sync.Map
	evil := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store("auth", r.Header.Get("Authorization"))
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	m, err := tokens.New(map[string]tokens.Identity{"author": {AppID: 1, PrivateKey: keyPEM}},
		tokens.Options{APIURL: srv.URL, Clock: clock.NewFake(t0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckApp(context.Background(), "author"); err == nil {
		t.Error("CheckApp followed a redirect")
	}
	if _, ok := leaked.Load("auth"); ok {
		t.Error("the redirect target got a request")
	}
}

// TestGHE: an API base with a path (GHE's /api/v3) and the git host derived from it.
func TestGHE(t *testing.T) {
	clk := clock.NewFake(t0)
	gh := githubtest.NewServer(t, clk)
	keyPEM, key := githubtest.AppKey(t)
	gh.AddApp(authorApp, &key.PublicKey)
	gh.AddInstallation(githubtest.Installation{ID: authorInst, AppID: authorApp, Owner: org, Permissions: authorPerms})
	ghe := httptest.NewServer(http.StripPrefix("/api/v3", gh.Config.Handler))
	defer ghe.Close()
	m, err := tokens.New(map[string]tokens.Identity{"author": {AppID: authorApp, PrivateKey: keyPEM}},
		tokens.Options{APIURL: ghe.URL + "/api/v3/", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := m.Token(context.Background(), provider.ReadOnlyRequest("author", docs, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(ghe.URL)
	if cred.Host != u.Host {
		t.Errorf("Host = %q, want %q", cred.Host, u.Host)
	}
	for _, tt := range []struct{ api, host string }{
		{"https://api.github.com", "github.com"},
		{"https://API.github.com/", "github.com"},
		{"https://ghe.example.com/api/v3", "ghe.example.com"},
		{"https://ghe.example.com:8443/api/v3", "ghe.example.com:8443"},
	} {
		u, _ := url.Parse(tt.api)
		if got := tokens.HostForAPI(u); got != tt.host {
			t.Errorf("HostForAPI(%s) = %q, want %q", tt.api, got, tt.host)
		}
	}
}

func TestNew(t *testing.T) {
	keyPEM, _ := githubtest.AppKey(t)
	tests := []struct {
		name   string
		idents map[string]tokens.Identity
		api    string
	}{
		{name: "no identities"},
		{name: "no app id", idents: map[string]tokens.Identity{"author": {PrivateKey: keyPEM}}},
		{name: "no key", idents: map[string]tokens.Identity{"author": {AppID: 1}}},
		{name: "not PEM", idents: map[string]tokens.Identity{"author": {AppID: 1, PrivateKey: []byte("secret-not-a-key")}}},
		{name: "not RSA", idents: map[string]tokens.Identity{"author": {AppID: 1, PrivateKey: []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")}}},
		{name: "bad API URL", idents: map[string]tokens.Identity{"author": {AppID: 1, PrivateKey: keyPEM}}, api: "ftp://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tokens.New(tt.idents, tokens.Options{APIURL: tt.api})
			if !errors.Is(err, provider.ErrInvalid) {
				t.Fatalf("New = %v, want ErrInvalid", err)
			}
			if strings.Contains(err.Error(), "secret-not-a-key") || strings.Contains(err.Error(), "-----BEGIN") {
				t.Errorf("error leaks key material: %v", err)
			}
		})
	}
	m, err := tokens.New(map[string]tokens.Identity{"reviewer": {AppID: 2, PrivateKey: keyPEM}, "author": {AppID: 1, PrivateKey: keyPEM}}, tokens.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Identities(); strings.Join(got, ",") != "author,reviewer" {
		t.Errorf("Identities = %v", got)
	}
}

func TestCheckApp(t *testing.T) {
	e := newEnv(t)
	if err := e.m.CheckApp(context.Background(), "author"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.CheckApp(context.Background(), "nobody"); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("unknown identity: %v", err)
	}
	e.srv.FailNext(githubtest.RouteApp, githubtest.Failure{Status: 503})
	if err := e.m.CheckApp(context.Background(), "author"); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("503: %v, want ErrRetryable", err)
	}
	if n := e.srv.Count(githubtest.RouteAccessToken); n != 0 {
		t.Errorf("CheckApp minted %d tokens", n)
	}
}

// TestNoTokenInLogs: debug logs of mints, hits and failures never carry a token or a JWT.
func TestNoTokenInLogs(t *testing.T) {
	e := newEnv(t)
	cred := e.mint(t, req("author", docs, write, time.Hour))
	e.mint(t, req("author", docs, write, time.Hour))
	e.mint(t, provider.ReadOnlyRequest("author", notes, time.Hour))
	e.srv.FailNext(githubtest.RouteAccessToken, githubtest.Failure{Status: 500})
	_, err := e.m.Token(context.Background(), provider.ReadOnlyRequest("reviewer", docs, time.Hour))
	if err == nil {
		t.Fatal("want an error")
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "github token minted") || !strings.Contains(logs, "github token cache hit") {
		t.Fatalf("expected mint and hit lines, got:\n%s", logs)
	}
	for _, tok := range e.srv.Tokens() {
		if strings.Contains(logs, tok) {
			t.Errorf("token %s in logs:\n%s", tok, logs)
		}
		if strings.Contains(err.Error(), tok) {
			t.Errorf("token in error: %v", err)
		}
	}
	for _, s := range []string{"Bearer", "eyJ"} {
		if strings.Contains(logs, s) {
			t.Errorf("JWT in logs:\n%s", logs)
		}
	}
	// The credential itself redacts.
	logger := slog.New(slog.NewTextHandler(e.logs, nil))
	logger.Info("lease", "cred", cred)
	if strings.Contains(e.logs.String(), cred.Token) {
		t.Error("credential logged with its token")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 10s")
		}
		time.Sleep(time.Millisecond)
	}
}

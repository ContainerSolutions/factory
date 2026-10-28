package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

func TestSafeReturnTo(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"", "/", true},
		{"/", "/", true},
		{"/board?view=all#top", "/board?view=all#top", true},
		{"/workitems/wi_1", "/workitems/wi_1", true},
		{"//evil.example", "", false},
		{"//evil.example/path", "", false},
		{"/\\evil.example", "", false},
		{"\\\\evil.example", "", false},
		{"https://evil.example", "", false},
		{"javascript:alert(1)", "", false},
		{"evil.example", "", false},
		{"/ok\r\nLocation: https://evil.example", "", false},
		{"/tab\there", "", false},
		{"/\x00", "", false},
		{"http:/evil.example", "", false},
		{"/" + strings.Repeat("a", MaxReturnTo-1), "/" + strings.Repeat("a", MaxReturnTo-1), true},
		{"/" + strings.Repeat("a", MaxReturnTo), "", false},
	}
	for _, tt := range tests {
		got, err := SafeReturnTo(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("SafeReturnTo(%q) = %q, %v; want %q, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrBadReturnTo) {
			t.Errorf("SafeReturnTo(%q): %v is not ErrBadReturnTo", tt.in, err)
		}
	}
}

type agents map[string]string

func (a agents) Authenticate(_ context.Context, tok string) (string, error) {
	if id, ok := a[tok]; ok {
		return id, nil
	}
	return "", scheduler.ErrUnauthorized
}

func (a agents) AuthenticateRun(context.Context, string) (scheduler.RunAuth, error) {
	return scheduler.RunAuth{}, scheduler.ErrUnauthorized
}

type staticCatalog struct{ c *catalog.Catalog }

func (s staticCatalog) Current() *catalog.Catalog { return s.c }

func newAuth(t *testing.T, clk *clock.Fake) (*Authenticator, *fake.Conn) {
	t.Helper()
	p := v1alpha1.Provider{Metadata: v1alpha1.ObjectMeta{Name: "gh"}, Spec: v1alpha1.ProviderSpec{Type: v1alpha1.ProviderTypeGitHub}}
	gh := fake.NewConn(p, fake.Options{Clock: clk})
	gh.SetOrgRole("org", "admin", "Gus")
	gh.SetTeam("org/docs", "rita")
	admin := v1alpha1.OrgRole{Org: "org", Role: "admin"}
	cat := &catalog.Catalog{Groups: map[string]*v1alpha1.Group{
		"maintainers": {Spec: v1alpha1.GroupSpec{Members: []v1alpha1.GroupSource{{Provider: "gh", OrgRole: &admin}}}},
		"readers":     {Spec: v1alpha1.GroupSpec{Members: []v1alpha1.GroupSource{{Provider: "gh", Team: "org/docs"}, {Provider: "gh", Team: "org/gone"}}}},
	}}
	a, err := New(Config{CLIToken: "cli", Provider: "gh", Clock: clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, Deps{
		Store:   memory.New(store.Options{Clock: clk}),
		Catalog: staticCatalog{cat},
		Conns:   controllers.StaticConns{"gh": gh},
		Agents:  agents{"agt_gus-mac1.secret": "agt_gus-mac1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, gh
}

func TestGroups(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, gh := newAuth(t, clk)
	ctx := context.Background()
	for _, c := range []struct {
		login          string
		reader, admins bool
	}{{"gus", true, true}, {"rita", true, false}, {"eve", false, false}} {
		u := &User{Provider: "gh", Login: c.login}
		r, err := a.IsReader(ctx, u)
		if err != nil || r != c.reader {
			t.Errorf("%s reader = %v, %v", c.login, r, err)
		}
		ad, err := a.IsAdmin(ctx, u)
		if err != nil || ad != c.admins {
			t.Errorf("%s admin = %v, %v", c.login, ad, err)
		}
	}
	// Another provider's login of the same name is not a member.
	if ok, _ := a.IsAdmin(ctx, &User{Provider: "other", Login: "gus"}); ok {
		t.Error("a login on another provider is an admin")
	}
	// Resolutions are cached for 10 minutes.
	gh.SetOrgRole("org", "admin")
	if ok, _ := a.IsAdmin(ctx, &User{Provider: "gh", Login: "gus"}); !ok {
		t.Error("the cache was not used")
	}
	clk.Advance(DefaultGroupCacheTTL)
	if ok, _ := a.IsAdmin(ctx, &User{Provider: "gh", Login: "gus"}); ok {
		t.Error("the cache outlived its TTL")
	}
}

func TestCredentials(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	ctx := context.Background()
	req := func(h, v string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set(h, v)
		}
		return r
	}
	if p, ok := a.CLI(req("Authorization", "Bearer cli")); !ok || p.Kind != KindCLI || p.ActorID() != CLIActor {
		t.Errorf("CLI token: %+v %v", p, ok)
	}
	for _, v := range []string{"Bearer cl", "Bearer clix", "Basic cli", "cli", ""} {
		if _, ok := a.CLI(req("Authorization", v)); ok {
			t.Errorf("%q accepted as the CLI token", v)
		}
	}
	if p, err := a.Agent(ctx, req("Authorization", "bearer agt_gus-mac1.secret")); err != nil || p.AgentID != "agt_gus-mac1" {
		t.Errorf("agent token: %+v %v", p, err)
	}
	for _, v := range []string{"Bearer agt_gus-mac1.wrong", "Bearer cli", ""} {
		if _, err := a.Agent(ctx, req("Authorization", v)); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: %v, want ErrUnauthenticated", v, err)
		}
	}

	// Sessions: signed cookies, stored by hash, 12 h.
	c, err := a.newSession(ctx, User{Provider: "gh", Login: "gus"})
	if err != nil {
		t.Fatal(err)
	}
	r := req("", "")
	r.AddCookie(c)
	if u, err := a.Session(ctx, r); err != nil || u.Login != "gus" {
		t.Fatalf("session: %+v %v", u, err)
	}
	var stored []Session
	if _, err := a.d.Store.List(ctx, KindSession, store.ListOptions{}, &stored); err != nil || len(stored) != 1 {
		t.Fatal(err)
	}
	id, _, _ := strings.Cut(c.Value, ".")
	if strings.Contains(stored[0].Metadata.Name, id) {
		t.Error("the session id is stored in clear")
	}
	forged := req("", "")
	forged.AddCookie(&http.Cookie{Name: SessionCookie, Value: id + ".forged"})
	if _, err := a.Session(ctx, forged); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("forged signature: %v", err)
	}
	clk.Advance(DefaultSessionTTL)
	if _, err := a.Session(ctx, r); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired session: %v", err)
	}
	if _, err := a.d.Store.List(ctx, KindSession, store.ListOptions{}, &stored); err != nil || len(stored) != 0 {
		t.Errorf("the expired session is still stored (%d)", len(stored))
	}
}

func TestBeginLoginBounds(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	if _, _, err := a.BeginLogin(context.Background(), "/"); !errors.Is(err, ErrOAuth) {
		t.Errorf("no client id: %v", err)
	}
	a.cfg.OAuth.ClientID = "c"
	// No process holds the logins in progress, so nothing bounds them: each state is
	// self-contained and its own.
	seen := map[string]bool{}
	for range 3 {
		_, c, err := a.BeginLogin(context.Background(), "/")
		if err != nil || c.Path != "/api/v1/auth" || !c.HttpOnly || !c.Secure {
			t.Fatalf("login: %+v %v", c, err)
		}
		if seen[c.Value] {
			t.Fatalf("two logins share the state %q", c.Value)
		}
		seen[c.Value] = true
	}
}

// TestLoginState: the OAuth state is signed under its own label with the session key (or the
// previous one), carries its return URL and expiry, and is used once through UsedLoginState.
func TestLoginState(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	a.cfg.OAuth.ClientID = "c"
	_, c, err := a.BeginLogin(ctx, "/board?x=1")
	if err != nil {
		t.Fatal(err)
	}
	st, ok := a.verifyState(c.Value)
	if !ok || st.ReturnTo != "/board?x=1" || time.Unix(st.Expires, 0).Sub(clk.Now()) != DefaultStateTTL {
		t.Fatalf("verifyState = %+v, %v", st, ok)
	}

	// Another dispatcher with the same key verifies it; one with another key doesn't.
	other, _ := newAuth(t, clk)
	other.key = a.key
	if _, ok := other.verifyState(c.Value); !ok {
		t.Error("a dispatcher with the same session key refused the state")
	}
	stranger, _ := newAuth(t, clk)
	if _, ok := stranger.verifyState(c.Value); ok {
		t.Error("a dispatcher with another session key accepted the state")
	}
	// Rotation: the old key, now the previous one, still verifies the state.
	rotated, _ := newAuth(t, clk)
	rotated.cfg.PreviousSessionKey = a.key
	if _, ok := rotated.verifyState(c.Value); !ok {
		t.Error("the previous session key didn't verify a state signed before the rotation")
	}
	// The labels keep a state and a session cookie apart under one key.
	if _, ok := a.verify(c.Value); ok {
		t.Error("a state passed for a session cookie")
	}
	if _, ok := a.verifyState(a.sign("ses-id")); ok {
		t.Error("a session cookie passed for a state")
	}
	// A changed payload breaks the signature.
	payload, sig, _ := strings.Cut(c.Value, ".")
	if _, ok := a.verifyState(payload[:len(payload)-1] + "A." + sig); ok {
		t.Error("a tampered state verified")
	}

	// Single use, on any dispatcher of the store.
	exp := time.Unix(st.Expires, 0)
	if err := a.useState(ctx, st.ID, exp); err != nil {
		t.Fatal(err)
	}
	other.d.Store = a.d.Store
	if err := other.useState(ctx, st.ID, exp); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("a state used twice: %v, want ErrStateMismatch", err)
	}
	// The sweep keeps the record until the state expires, then deletes it.
	if n, err := a.SweepUsedStates(ctx); err != nil || n != 0 {
		t.Fatalf("sweep before the expiry = %d, %v", n, err)
	}
	clk.Advance(DefaultStateTTL)
	if n, err := a.SweepUsedStates(ctx); err != nil || n != 1 {
		t.Fatalf("sweep after the expiry = %d, %v", n, err)
	}
}

func TestSessionKeyRotation(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	cookie := a.sign("ses-id")
	b, _ := newAuth(t, clk)
	if _, ok := b.verify(cookie); ok {
		t.Fatal("another key verified the session cookie")
	}
	b.cfg.PreviousSessionKey = a.key
	if id, ok := b.verify(cookie); !ok || id != "ses-id" {
		t.Fatalf("the previous key didn't verify the session cookie: %q %v", id, ok)
	}
	if _, ok := b.verify("ses-id." + strings.Repeat("A", 43)); ok {
		t.Fatal("a forged signature verified")
	}
}

// TestFinishLoginWritesNothingWithoutACode: a callback with a valid state but no code (or a
// code GitHub refuses) creates no UsedLoginState, so an anonymous client can't make the
// store write; the state stays usable for the real callback.
func TestFinishLoginWritesNothingWithoutACode(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	a.cfg.OAuth.ClientID = "c"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bad_verification_code"}`, http.StatusOK)
	}))
	defer gh.Close()
	a.cfg.OAuth.TokenURL = gh.URL
	a.cfg.HTTPClient = gh.Client()
	_, cookie, err := a.BeginLogin(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback", nil)
	r.AddCookie(cookie)
	if _, err := a.FinishLogin(ctx, r, "", cookie.Value); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no code: %v, want ErrUnauthenticated", err)
	}
	if _, err := a.FinishLogin(ctx, r, "forged", cookie.Value); err == nil {
		t.Fatal("a code GitHub refuses finished the login")
	}
	var used []*UsedLoginState
	if _, err := a.d.Store.List(ctx, KindUsedLoginState, store.ListOptions{}, &used); err != nil || len(used) != 0 {
		t.Fatalf("UsedLoginStates after callbacks with no valid code: %d (%v), want none", len(used), err)
	}
}

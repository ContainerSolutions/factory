package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/clock"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// jwt is an unsigned JWT with claims.
func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(b) + ".sig"
}

// access is an access token that expires at exp, tagged so the tests tell tokens apart.
func access(tag string, exp time.Time) string {
	return jwt(map[string]any{"exp": exp.Unix(), "tag": tag, "secret": "access-secret-" + tag})
}

// memStore is a Store in memory; loadErr and saveErr fail Load and Save.
type memStore struct {
	mu               sync.Mutex
	l                Login
	loadErr, saveErr error
	saves            int
}

func (s *memStore) Load(context.Context) (Login, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.l, s.loadErr
}

func (s *memStore) Save(_ context.Context, l Login) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.l = l
	s.saves++
	return nil
}

func (s *memStore) get() Login {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.l
}

func (s *memStore) set(f func(*memStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// authServer is the fake auth server: each POST gets the next answer (the last repeats), and
// it records what it got. hold, when set, blocks each request until it is closed.
type authServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []map[string]string
	paths   []string
	types   []string
	answers []answer
	hold    chan struct{}
}

type answer struct {
	status int
	body   string
}

func newAuthServer(t *testing.T, answers ...answer) *authServer {
	t.Helper()
	a := &authServer{answers: answers}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.got, a.paths, a.types = append(a.got, body), append(a.paths, r.Method+" "+r.URL.Path), append(a.types, r.Header.Get("Content-Type"))
		ans := a.answers[min(len(a.got), len(a.answers))-1]
		hold := a.hold
		a.mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.WriteHeader(ans.status)
		_, _ = io.WriteString(w, ans.body)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *authServer) posts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.got)
}

// req is the i-th request: its body, method and path, and Content-Type.
func (a *authServer) req(i int) (body map[string]string, path, ctype string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.got[i], a.paths[i], a.types[i]
}

func ok(accessTok, refreshTok string) answer {
	b, _ := json.Marshal(map[string]string{"access_token": accessTok, "refresh_token": refreshTok,
		"id_token": jwt(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_1"}})})
	return answer{200, string(b)}
}

// setup is a Refresher over a memStore holding l, the auth server, a fake clock and a log
// that the test checks for tokens at its end.
func setup(t *testing.T, l Login, answers ...answer) (*Refresher, *memStore, *authServer, *clock.Fake) {
	t.Helper()
	a := newAuthServer(t, answers...)
	st := &memStore{l: l}
	clk := clock.NewFake(now)
	var logs safeBuffer
	r, err := New(Config{Store: st, Client: a.srv.Client(), TokenURL: a.srv.URL + "/oauth/token", Clock: clk,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out := logs.String()
		for _, secret := range []string{"access-secret", "refresh-secret", base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))} {
			if strings.Contains(out, secret) {
				t.Errorf("the log holds a token:\n%s", out)
			}
		}
	})
	return r, st, a, clk
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func baseLogin(exp time.Time) Login {
	return Login{AccessToken: access("old", exp), RefreshToken: "refresh-secret-old", AccountID: "acct_1"}
}

// TestCurrent: Current refreshes only when the login is due (codex's rules), sends codex's
// request, writes the new tokens back and uses them.
func TestCurrent(t *testing.T) {
	newAccess := access("new", now.Add(time.Hour))
	tests := []struct {
		name    string
		login   Login
		refresh bool
	}{
		{"expires in an hour", baseLogin(now.Add(time.Hour)), false},
		{"expires in 4 minutes", baseLogin(now.Add(4 * time.Minute)), true},
		{"expired", baseLogin(now.Add(-time.Minute)), true},
		{"no exp, refreshed 7 days ago", Login{AccessToken: "opaque", RefreshToken: "refresh-secret-old", LastRefresh: now.Add(-7 * 24 * time.Hour)}, false},
		{"no exp, refreshed 9 days ago", Login{AccessToken: "opaque", RefreshToken: "refresh-secret-old", LastRefresh: now.Add(-9 * 24 * time.Hour)}, true},
		{"no exp, never refreshed", Login{AccessToken: "opaque", RefreshToken: "refresh-secret-old"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, st, a, _ := setup(t, tt.login, ok(newAccess, "refresh-secret-new"))
			l, err := r.Current(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !tt.refresh {
				if a.posts() != 0 || l.AccessToken != tt.login.AccessToken {
					t.Errorf("refreshed a login that isn't due (%d posts)", a.posts())
				}
				return
			}
			body, path, ctype := a.req(0)
			if a.posts() != 1 || path != "POST /oauth/token" || ctype != "application/json" {
				t.Fatalf("%d posts: %s %s", a.posts(), path, ctype)
			}
			if want := map[string]string{"client_id": ClientID, "grant_type": "refresh_token", "refresh_token": "refresh-secret-old"}; fmt.Sprint(body) != fmt.Sprint(want) {
				t.Errorf("the request body differs from codex's (keys %d)", len(body))
			}
			if l.AccessToken != newAccess || l.RefreshToken != "refresh-secret-new" || !l.LastRefresh.Equal(now) {
				t.Errorf("Current returned the old login")
			}
			if s := st.get(); s.AccessToken != newAccess || s.RefreshToken != "refresh-secret-new" || !s.LastRefresh.Equal(now) {
				t.Errorf("the store has the old login")
			}
			if l, err := r.Current(context.Background()); err != nil || l.AccessToken != newAccess || a.posts() != 1 {
				t.Errorf("a second Current: %v, %d posts", err, a.posts())
			}
		})
	}
}

// TestUnauthorized: a 401 refreshes, also a token that isn't due; a 401 for a token already
// replaced returns the new one without a refresh.
func TestUnauthorized(t *testing.T) {
	login := baseLogin(now.Add(time.Hour))
	newAccess := access("new", now.Add(2*time.Hour))
	r, _, a, _ := setup(t, login, ok(newAccess, "refresh-secret-new"))
	l, err := r.Unauthorized(context.Background(), login.AccessToken)
	if err != nil || l.AccessToken != newAccess || a.posts() != 1 {
		t.Fatalf("Unauthorized: %v, %d posts", err, a.posts())
	}
	l, err = r.Unauthorized(context.Background(), login.AccessToken)
	if err != nil || l.AccessToken != newAccess || a.posts() != 1 {
		t.Errorf("a 401 for the replaced token: %v, %d posts", err, a.posts())
	}
}

// TestUnauthorizedMinInterval: a 401 for a login refreshed under minUnauthorizedRefresh ago
// returns it unrefreshed; after the interval a 401 refreshes; the expiry-driven refresh
// (Current) doesn't wait for the interval.
func TestUnauthorizedMinInterval(t *testing.T) {
	tests := []struct {
		name    string
		after   time.Duration // since the first refresh
		current bool          // the second call is Current on a token due by exp
		posts   int           // in all, the first refresh included
	}{
		{"a second 401 at once", 0, false, 1},
		{"a second 401 within the interval", minUnauthorizedRefresh - time.Second, false, 1},
		{"a 401 after the interval", minUnauthorizedRefresh, false, 2},
		{"Current on an expiring token within the interval", 10 * time.Second, true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			login := baseLogin(now.Add(time.Hour))
			first := access("first", now.Add(time.Hour))
			if tt.current {
				first = access("first", now.Add(time.Minute)) // due by exp at once
			}
			second := access("second", now.Add(2*time.Hour))
			r, _, a, clk := setup(t, login, ok(first, "refresh-secret-1"), ok(second, "refresh-secret-2"))
			l, err := r.Unauthorized(context.Background(), login.AccessToken)
			if err != nil || l.AccessToken != first || a.posts() != 1 {
				t.Fatalf("the first 401: %v, %d posts", err, a.posts())
			}
			clk.Advance(tt.after)
			if tt.current {
				l, err = r.Current(context.Background())
			} else {
				l, err = r.Unauthorized(context.Background(), first)
			}
			want := first
			if tt.posts == 2 {
				want = second
			}
			if err != nil || a.posts() != tt.posts || l.AccessToken != want {
				t.Errorf("the second call: %v, %d posts, want %d; got the %s token", err, a.posts(), tt.posts, map[bool]string{true: "new", false: "first"}[l.AccessToken == second])
			}
		})
	}
}

// TestSingleFlight: concurrent 401s for the same token make one refresh, and every caller gets
// its result.
func TestSingleFlight(t *testing.T) {
	login := baseLogin(now.Add(time.Hour))
	newAccess := access("new", now.Add(2*time.Hour))
	r, _, a, _ := setup(t, login, ok(newAccess, "refresh-secret-new"))
	a.hold = make(chan struct{})
	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	for i := range n {
		wg.Go(func() {
			l, err := r.Unauthorized(context.Background(), login.AccessToken)
			if err != nil {
				t.Error(err)
			}
			got[i] = l.AccessToken
		})
	}
	for a.posts() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(a.hold)
	wg.Wait()
	if a.posts() != 1 {
		t.Errorf("%d refreshes, want 1", a.posts())
	}
	for i, g := range got {
		if g != newAccess {
			t.Errorf("caller %d got the old token", i)
		}
	}
}

// TestPartialResponse: a 200 without access_token keeps the old one and saves the new refresh
// token; an id_token for another account is logged and the account kept.
func TestPartialResponse(t *testing.T) {
	login := baseLogin(now.Add(time.Hour))
	idOther := jwt(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_2"}})
	r, st, _, _ := setup(t, login, answer{200, `{"refresh_token":"refresh-secret-new","id_token":"` + idOther + `"}`})
	l, err := r.Unauthorized(context.Background(), login.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if l.AccessToken != login.AccessToken || l.RefreshToken != "refresh-secret-new" || l.AccountID != "acct_1" || st.get().RefreshToken != "refresh-secret-new" {
		t.Errorf("the partial response wasn't merged")
	}
}

// TestFailures: the permanent failures stop the refreshes until the stored login changes; the
// transient ones are retried, 3 attempts in all; a bad 200 isn't retried.
func TestFailures(t *testing.T) {
	newAccess := access("new", now.Add(2*time.Hour))
	tests := []struct {
		name      string
		answers   []answer
		posts     int
		permanent bool
		ok        bool
	}{
		{"401", []answer{{401, `{"error":"whatever"}`}}, 1, true, false},
		{"400 invalid_grant", []answer{{400, `{"error":"INVALID_GRANT","error_description":"x"}`}}, 1, true, false},
		{"refresh_token_reused in error.code", []answer{{403, `{"error":{"code":"refresh_token_reused","message":"m"}}`}}, 1, true, false},
		{"refresh_token_expired at the top", []answer{{400, `{"code":"refresh_token_expired"}`}}, 1, true, false},
		{"refresh_token_invalidated on a 500", []answer{{500, `{"error":"refresh_token_invalidated"}`}}, 1, true, false},
		{"invalid_grant on a 403 is transient", []answer{{403, `{"error":"invalid_grant"}`}}, 3, false, false},
		{"5xx then ok", []answer{{502, "bad gateway"}, {503, ""}, ok(newAccess, "refresh-secret-new")}, 3, false, true},
		{"5xx three times", []answer{{500, "x"}}, 3, false, false},
		{"a 200 that isn't an object", []answer{{200, `["access-secret-x"]`}}, 1, false, false},
		{"a 200 over 64 KiB", []answer{{200, `{"access_token":"` + strings.Repeat("a", maxResponse) + `"}`}}, 1, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			login := baseLogin(now.Add(time.Hour))
			r, st, a, clk := setup(t, login, tt.answers...)
			done := make(chan struct{})
			go func() { // the backoff waits on the clock
				for {
					select {
					case <-done:
						return
					default:
					}
					if clk.Timers() > 0 {
						clk.Advance(2 * time.Second)
					}
					time.Sleep(time.Millisecond)
				}
			}()
			l, err := r.Unauthorized(context.Background(), login.AccessToken)
			close(done)
			if a.posts() != tt.posts {
				t.Errorf("%d posts, want %d", a.posts(), tt.posts)
			}
			if tt.ok {
				if err != nil || l.AccessToken != newAccess {
					t.Errorf("err %v", err)
				}
				return
			}
			if err == nil || l.AccessToken != login.AccessToken {
				t.Fatalf("err %v; a failed refresh must return the cached login", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the error holds a token: %v", err)
			}
			if got := errors.Is(err, ErrPermanent); got != tt.permanent || (r.Err() != nil) != tt.permanent {
				t.Fatalf("permanent %v (Err %v), want %v: %v", got, r.Err(), tt.permanent, err)
			}
			if !tt.permanent {
				return
			}
			// stopped: no more posts until the stored login changes.
			if _, err := r.Unauthorized(context.Background(), login.AccessToken); !errors.Is(err, ErrPermanent) || a.posts() != 1 {
				t.Errorf("after a permanent failure: %v, %d posts", err, a.posts())
			}
			fresh := Login{AccessToken: access("reimported", now.Add(time.Hour)), RefreshToken: "refresh-secret-reimported", AccountID: "acct_1"}
			st.set(func(s *memStore) { s.l = fresh })
			if l, err := r.Unauthorized(context.Background(), login.AccessToken); err != nil || l.AccessToken != fresh.AccessToken || r.Err() != nil || a.posts() != 1 {
				t.Errorf("after a re-import: %v, Err %v, %d posts", err, r.Err(), a.posts())
			}
		})
	}
}

// TestNoRefreshToken: an empty refresh token or the VM's placeholder is never sent.
func TestNoRefreshToken(t *testing.T) {
	for _, rt := range []string{"", harness.Placeholder} {
		login := baseLogin(now.Add(time.Minute))
		login.RefreshToken = rt
		r, _, a, _ := setup(t, login, ok("x", "y"))
		if l, err := r.Current(context.Background()); !errors.Is(err, ErrNoRefreshToken) || l.AccessToken != login.AccessToken || a.posts() != 0 {
			t.Errorf("refresh token %q: %v, %d posts", rt, err, a.posts())
		}
	}
}

// TestProvenance: a login with codex's oauth_provenance refreshes at <issuer>/oauth/token with
// its client id.
func TestProvenance(t *testing.T) {
	login := baseLogin(now.Add(time.Minute))
	r, _, a, _ := setup(t, login, ok(access("new", now.Add(time.Hour)), "refresh-secret-new"))
	login.Issuer, login.ClientID = a.srv.URL+"/tenant/", "app_other"
	r.cfg.Store.(*memStore).set(func(s *memStore) { s.l = login })
	if _, err := r.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, path, _ := a.req(0)
	if a.posts() != 1 || path != "POST /tenant/oauth/token" || body["client_id"] != "app_other" {
		t.Errorf("%d posts: %s, client_id %q", a.posts(), path, body["client_id"])
	}
}

// TestSaveFails: when the write-back fails the new tokens are used from memory, and the save is
// retried on the next Current.
func TestSaveFails(t *testing.T) {
	login := baseLogin(now.Add(time.Minute))
	newAccess := access("new", now.Add(time.Hour))
	r, st, a, _ := setup(t, login, ok(newAccess, "refresh-secret-new"))
	st.set(func(s *memStore) { s.saveErr = errors.New("disk full") })
	if l, err := r.Current(context.Background()); err != nil || l.AccessToken != newAccess {
		t.Fatalf("Current: %v", err)
	}
	st.set(func(s *memStore) { s.saveErr = nil })
	if l, err := r.Current(context.Background()); err != nil || l.AccessToken != newAccess || a.posts() != 1 {
		t.Fatalf("Current: %v, %d posts", err, a.posts())
	}
	if s := st.get(); s.RefreshToken != "refresh-secret-new" || st.saves != 1 {
		t.Errorf("the save wasn't retried (%d saves)", st.saves)
	}
}

// TestLoadFails: no login is no token.
func TestLoadFails(t *testing.T) {
	r, st, _, _ := setup(t, Login{})
	st.set(func(s *memStore) { s.loadErr = errors.New("no login") })
	if l, err := r.Current(context.Background()); err == nil || l.AccessToken != "" {
		t.Errorf("Current: %v", err)
	}
}

// TestRedacted: Login never prints its tokens.
func TestRedacted(t *testing.T) {
	l := Login{AccessToken: "access-secret", RefreshToken: "refresh-secret"}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "login", l, "p", &l)
	b, _ := json.Marshal(map[string]any{"l": l})
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", l, l, l, l), logs.String(), string(b)} {
		if strings.Contains(s, "secret") {
			t.Errorf("printed a token: %s", s)
		}
	}
}

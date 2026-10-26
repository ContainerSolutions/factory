// Package chatgpt is the codex ChatGPT login the gateway sends to chatgpt.com, and its
// Refresher (gateway.md, "ChatGPT refresh"). One Refresher per supervisor, shared by its
// gateways, refreshes the access token when its exp is less than 5 minutes away and after an
// upstream 401, one refresh at a time, and writes the new tokens back to the Store before they
// are used. The VM never refreshes: its auth.json has placeholders.
//
// The endpoint, the client id, the request and the failure classes are codex's at
// rust-v0.163.0, the version provision/base.sh pins: codex-rs/login/src/auth/manager.rs
// (REFRESH_TOKEN_URL, CLIENT_ID, request_chatgpt_token_refresh, classify_refresh_token_failure,
// RefreshResponse, the 5-minute window and the 8-day interval) and
// codex-rs/login/src/oauth/client.rs (OAuthClient::refresh, TokenEncoding::Json), with the
// error code read as codex-rs/login/src/oauth/error.rs reads it. They are unverified against the
// live service until the owner's smoke run.
//
// No token is ever logged, printed or put in an error: Login redacts, and errors carry the
// status and an error code from the known list only.
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
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/clock"
)

// The refresh endpoint and client id of a login without provenance (codex rust-v0.163.0's
// REFRESH_TOKEN_URL and CLIENT_ID).
const (
	TokenURL = "https://auth.openai.com/oauth/token"
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

const (
	// refreshWindow: an access token whose exp is closer than this is refreshed first.
	refreshWindow = 5 * time.Minute
	// refreshInterval: without a readable exp, a login last refreshed longer ago is refreshed.
	refreshInterval = 8 * 24 * time.Hour
	// minUnauthorizedRefresh: a 401 for a login refreshed less than this ago doesn't refresh it
	// again. chatgpt.com may keep 401ing a valid token (account state, or a request the VM
	// shaped); a refresh per 401 would rotate the owner's refresh token on every round trip,
	// which the auth server's reuse detection may answer by revoking the whole token family.
	minUnauthorizedRefresh = time.Minute
	// flightTimeout bounds a refresh, its retries included; httpTimeout each attempt.
	flightTimeout = 30 * time.Second
	httpTimeout   = 30 * time.Second
	saveTimeout   = 10 * time.Second
	maxResponse   = 64 << 10
)

// backoff is the wait before each retry of a transient failure: 3 attempts in all.
var backoff = []time.Duration{time.Second, 2 * time.Second}

// Login is the owner's codex ChatGPT login. Issuer and ClientID are codex's
// tokens.oauth_provenance, when auth.json has it (codex main records it; rust-v0.163.0
// doesn't): a refresh then goes to <Issuer>/oauth/token with ClientID. String, Format,
// GoString, LogValue, MarshalJSON and MarshalText redact.
type Login struct {
	AccessToken, RefreshToken string
	AccountID                 string
	Issuer, ClientID          string
	LastRefresh               time.Time
}

func (Login) String() string { return "chatgpt.Login{redacted}" }

// Format redacts every verb, %#v included.
func (l Login) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, l.String()) }

// GoString redacts.
func (l Login) GoString() string { return l.String() }

// LogValue redacts.
func (l Login) LogValue() slog.Value { return slog.StringValue(l.String()) }

// MarshalJSON is the redacted form as a JSON string.
func (l Login) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// MarshalText is the redacted form.
func (l Login) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Store is where the login lives (the supervisor's login store). Load's and Save's errors must
// never hold a token.
type Store interface {
	Load(ctx context.Context) (Login, error)
	Save(ctx context.Context, l Login) error
}

// ErrPermanent is wrapped by a refresh the auth server refused for good (codex's
// RefreshTokenError::Permanent): the refresher stops trying until the stored login changes.
var ErrPermanent = errors.New("chatgpt: the auth server refused the refresh token: log codex in again, then factory-agent login codex")

// ErrNoRefreshToken is a login without a refresh token (or with the VM's placeholder): never sent.
var ErrNoRefreshToken = errors.New("chatgpt: the login has no refresh token: factory-agent login codex again")

// Config configures a Refresher.
type Config struct {
	// Store is required.
	Store Store
	// Client sends the refresh; default 30 s, the system roots and no proxy.
	Client *http.Client
	// TokenURL is the endpoint of a login without provenance; default TokenURL (tests).
	TokenURL string
	// Clock defaults to the real clock; Logger to discarding.
	Clock  clock.Clock
	Logger *slog.Logger
}

// Refresher holds the cached login and refreshes it, one refresh at a time.
type Refresher struct {
	cfg Config
	log *slog.Logger

	// saveMu serialises the write-backs, so the newest login is the last saved.
	saveMu sync.Mutex

	mu      sync.Mutex
	cur     Login
	loaded  bool
	unsaved bool  // cur's write-back failed: retried on every Current
	perm    error // a permanent failure, until the stored login changes
	flight  *flight
}

type flight struct {
	done  chan struct{}
	login Login
	err   error
}

// New returns a Refresher; it loads the login at first use.
func New(cfg Config) (*Refresher, error) {
	if cfg.Store == nil {
		return nil, errors.New("chatgpt: no Store")
	}
	if cfg.Client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		cfg.Client = &http.Client{Timeout: httpTimeout, Transport: tr}
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = TokenURL
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Refresher{cfg: cfg, log: cfg.Logger}, nil
}

// Current returns the cached login, refreshed first when it's due: its access token's exp is
// less than 5 minutes away, or, without a readable exp, it was last refreshed more than 8 days
// ago. A failed refresh returns the cached login with the error, so the request still goes out
// with it. Only a login that can't be loaded returns no token.
func (r *Refresher) Current(ctx context.Context) (Login, error) {
	l, err := r.cached(ctx)
	if err != nil || !r.due(l) {
		return l, err
	}
	return r.refresh(ctx)
}

// Unauthorized follows an upstream 401 for the access token used: when the cached token is no
// longer used, another caller has refreshed it already and it is returned; when the login was
// refreshed less than minUnauthorizedRefresh ago, the cached login is returned unrefreshed (the
// gateway then passes the 401 on); otherwise it refreshes.
func (r *Refresher) Unauthorized(ctx context.Context, used string) (Login, error) {
	l, err := r.cached(ctx)
	if err != nil || l.AccessToken != used {
		return l, err
	}
	if !l.LastRefresh.IsZero() && r.cfg.Clock.Now().Sub(l.LastRefresh) < minUnauthorizedRefresh {
		r.log.Warn("chatgpt: a 401 for a login refreshed under a minute ago: not refreshing it again")
		return l, nil
	}
	return r.refresh(ctx)
}

// Err is the permanent failure that stopped the refreshes, or nil.
func (r *Refresher) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perm
}

// cached is the cached login, loaded from the Store at first use. A failed write-back is
// retried first.
func (r *Refresher) cached(ctx context.Context) (Login, error) {
	r.mu.Lock()
	loaded := r.loaded
	r.mu.Unlock()
	if !loaded {
		l, err := r.cfg.Store.Load(ctx)
		if err != nil {
			return Login{}, err
		}
		r.mu.Lock()
		if !r.loaded {
			r.cur, r.loaded = l, true
		}
		r.mu.Unlock()
	}
	r.retrySave(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur, nil
}

// due reports whether l is refreshed before use (codex's rules).
func (r *Refresher) due(l Login) bool {
	now := r.cfg.Clock.Now()
	if exp, ok := jwtExp(l.AccessToken); ok {
		return !exp.After(now.Add(refreshWindow))
	}
	return !l.LastRefresh.IsZero() && l.LastRefresh.Before(now.Add(-refreshInterval))
}

// refresh joins the refresh in flight, or starts one. The refresh runs on its own context, so a
// caller that goes away doesn't abandon a refresh token that may already be spent.
func (r *Refresher) refresh(ctx context.Context) (Login, error) {
	r.mu.Lock()
	f := r.flight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		r.flight = f
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightTimeout)
		go func() {
			defer cancel()
			f.login, f.err = r.run(fctx)
			r.mu.Lock()
			r.flight = nil
			r.mu.Unlock()
			close(f.done)
		}()
	}
	r.mu.Unlock()
	select {
	case <-f.done:
		return f.login, f.err
	case <-ctx.Done():
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.cur, ctx.Err()
	}
}

// run is one refresh: it reloads the Store (an owner's re-import replaces the cached login),
// posts the refresh token, and writes the new tokens back before they are used.
func (r *Refresher) run(ctx context.Context) (Login, error) {
	r.mu.Lock()
	cur, perm, unsaved := r.cur, r.perm, r.unsaved
	r.mu.Unlock()
	// a pending write-back means the Store holds the older, spent tokens.
	if !unsaved {
		stored, err := r.cfg.Store.Load(ctx)
		switch {
		case err != nil:
			r.log.Warn("chatgpt: reloading the login", "err", err)
		case stored.RefreshToken != cur.RefreshToken:
			r.log.Info("chatgpt: the stored login changed: using it")
			r.mu.Lock()
			r.cur, r.perm = stored, nil
			r.mu.Unlock()
			if !r.due(stored) {
				return stored, nil
			}
			cur, perm = stored, nil
		}
	}
	if perm != nil {
		return cur, perm
	}
	if cur.RefreshToken == "" || cur.RefreshToken == harness.Placeholder {
		return cur, ErrNoRefreshToken
	}
	url, clientID := r.cfg.TokenURL, ClientID
	if cur.Issuer != "" && cur.ClientID != "" {
		url, clientID = strings.TrimRight(cur.Issuer, "/")+"/oauth/token", cur.ClientID
	}
	var res tokenResponse
	for attempt := 0; ; attempt++ {
		var err error
		res, err = r.post(ctx, url, clientID, cur.RefreshToken)
		if err == nil {
			break
		}
		var te *transientError
		if !errors.As(err, &te) || attempt == len(backoff) {
			if errors.Is(err, ErrPermanent) {
				r.mu.Lock()
				r.perm = err
				r.mu.Unlock()
			}
			r.log.Error("chatgpt: the refresh failed", "err", err)
			return cur, err
		}
		t := r.cfg.Clock.NewTimer(backoff[attempt])
		select {
		case <-t.C():
		case <-ctx.Done():
			t.Stop()
			return cur, fmt.Errorf("chatgpt: the refresh: %w", ctx.Err())
		}
	}
	next := cur
	if res.AccessToken != "" {
		next.AccessToken = res.AccessToken
	}
	if res.RefreshToken != "" {
		next.RefreshToken = res.RefreshToken
	}
	next.LastRefresh = r.cfg.Clock.Now().UTC()
	if acct := idTokenAccount(res.IDToken); acct != "" && acct != cur.AccountID {
		// the bundles carry the account: it doesn't change under a running task.
		r.log.Error("chatgpt: the refreshed id_token names another ChatGPT account; keeping the login's")
	}
	r.saveMu.Lock()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	serr := r.cfg.Store.Save(sctx, next)
	cancel()
	r.mu.Lock()
	r.cur, r.unsaved = next, serr != nil
	r.mu.Unlock()
	r.saveMu.Unlock()
	if serr != nil {
		r.log.Error("chatgpt: saving the refreshed login (retried on the next request)", "err", serr)
	}
	attrs := []any{}
	if exp, ok := jwtExp(next.AccessToken); ok {
		attrs = append(attrs, "expires", exp.UTC().Format(time.RFC3339))
	}
	r.log.Info("chatgpt: refreshed the login", attrs...)
	return next, nil
}

// retrySave writes the cached login back when its last write-back failed.
func (r *Refresher) retrySave(ctx context.Context) {
	r.mu.Lock()
	unsaved := r.unsaved
	r.mu.Unlock()
	if !unsaved {
		return
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.mu.Lock()
	l, unsaved := r.cur, r.unsaved
	r.mu.Unlock()
	if !unsaved {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()
	if err := r.cfg.Store.Save(sctx, l); err != nil {
		r.log.Error("chatgpt: saving the refreshed login (retried on the next request)", "err", err)
		return
	}
	r.mu.Lock()
	r.unsaved = false
	r.mu.Unlock()
}

// tokenResponse is codex's RefreshResponse: every field optional.
type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// transientError is a failure that is retried: the network, a 5xx, a non-200 whose body doesn't
// parse, an unknown error code.
type transientError struct{ msg string }

func (e *transientError) Error() string { return e.msg }

// knownCodes are the error codes a refresh's failures are classified by and logged with.
var knownCodes = []string{"invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated"}

// post sends one refresh: POST url, a JSON body of client_id, grant_type and refresh_token.
func (r *Refresher) post(ctx context.Context, url, clientID, refreshToken string) (tokenResponse, error) {
	body, err := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": refreshToken})
	if err != nil {
		return tokenResponse{}, fmt.Errorf("chatgpt: the refresh body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("chatgpt: the refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := r.cfg.Client.Do(req)
	if err != nil {
		return tokenResponse{}, &transientError{msg: fmt.Sprintf("chatgpt: the refresh: %v", err)}
	}
	defer func() { _ = res.Body.Close() }()
	b, rerr := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if res.StatusCode == http.StatusOK {
		// not retried: the refresh token may be spent already. The next refresh tries again.
		var tr tokenResponse
		if t := bytes.TrimSpace(b); rerr != nil || len(b) > maxResponse || len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &tr) != nil {
			return tokenResponse{}, errors.New("chatgpt: the refresh's 200 response is not a JSON object of at most 64 KiB")
		}
		return tr, nil
	}
	code := strings.ToLower(errorCode(b))
	logged := "other"
	for _, k := range knownCodes {
		if code == k {
			logged = k
		}
	}
	if res.StatusCode == http.StatusUnauthorized || (res.StatusCode == http.StatusBadRequest && code == "invalid_grant") || (logged != "other" && logged != "invalid_grant") {
		return tokenResponse{}, fmt.Errorf("%w (status %d, error code %s)", ErrPermanent, res.StatusCode, logged)
	}
	return tokenResponse{}, &transientError{msg: fmt.Sprintf("chatgpt: the refresh failed: status %d, error code %s", res.StatusCode, logged)}
}

// errorCode is an error body's code as codex reads it: error when it's a string, else
// error.code, else a top-level code; "" when none.
func errorCode(b []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
		Code  any             `json:"code"`
	}
	if json.Unmarshal(bytes.TrimSpace(b), &body) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(body.Error, &s) == nil && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Code any `json:"code"`
	}
	if json.Unmarshal(body.Error, &obj) == nil {
		if c, ok := obj.Code.(string); ok && strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
	}
	if c, ok := body.Code.(string); ok {
		return strings.TrimSpace(c)
	}
	return ""
}

// jwtClaims is a JWT's payload, decoded and not verified; nil when tok isn't a JWT.
func jwtClaims(tok string) map[string]json.RawMessage {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// jwtExp is an access token's exp claim.
func jwtExp(tok string) (time.Time, bool) {
	var exp float64
	raw, ok := jwtClaims(tok)["exp"]
	if !ok || json.Unmarshal(raw, &exp) != nil || exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

// idTokenAccount is an id_token's chatgpt_account_id; "" when it has none.
func idTokenAccount(tok string) string {
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	raw, ok := jwtClaims(tok)["https://api.openai.com/auth"]
	if !ok || json.Unmarshal(raw, &auth) != nil {
		return ""
	}
	return auth.AccountID
}

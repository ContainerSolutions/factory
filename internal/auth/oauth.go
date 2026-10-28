package auth

import (
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// statePath is where the state cookie is sent: the auth routes only.
const statePath = "/api/v1/auth"

// stateLabel separates the OAuth state's signature from the session cookie's, so neither can
// pass for the other under the same key.
const stateLabel = "factory-oauth-state\x00"

// stateIDBytes is the random part of a login's state.
const stateIDBytes = 32

// loginState is what a signed OAuth state carries: the login is self-contained, so its
// callback may reach any dispatcher that has the same session key (P3 §Several dispatchers).
type loginState struct {
	// ID is random; its hash names the login's UsedLoginState once it is finished.
	ID string `json:"id"`
	// Expires is when the state stops being accepted (Unix seconds).
	Expires int64 `json:"exp"`
	// ReturnTo is where to send the browser once signed in.
	ReturnTo string `json:"rt"`
}

// signState returns the state of st: "<payload>.<HMAC-SHA256(key, label || payload)>", both
// base64url.
func (a *Authenticator) signState(st loginState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("oauth: encode state: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac(a.key, stateLabel+payload)), nil
}

// verifyState returns the login a state carries, if its signature is right under the session
// key or a previous one. It doesn't check the expiry.
func (a *Authenticator) verifyState(state string) (loginState, bool) {
	payload, sig, ok := strings.Cut(state, ".")
	if !ok || payload == "" {
		return loginState{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !a.anyKey(func(key []byte) bool { return hmac.Equal(got, mac(key, stateLabel+payload)) }) {
		return loginState{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return loginState{}, false
	}
	var st loginState
	if err := json.Unmarshal(b, &st); err != nil || st.ID == "" {
		return loginState{}, false
	}
	return st, true
}

// MaxReturnTo bounds a returnTo, which the signed OAuth state carries in its cookie: cookies
// over about 4 KB are dropped by browsers.
const MaxReturnTo = 1024

// SafeReturnTo validates a returnTo: it must be a same-origin absolute path ("/board?x=1") of
// at most MaxReturnTo bytes, not a URL, a scheme-relative "//host" or a backslash trick
// ("/\host"). "" is "/".
func SafeReturnTo(s string) (string, error) {
	if s == "" {
		return "/", nil
	}
	if len(s) > MaxReturnTo {
		return "", ErrBadReturnTo
	}
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\r\n\t") {
		return "", ErrBadReturnTo
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", ErrBadReturnTo
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "", ErrBadReturnTo
	}
	return s, nil
}

// BeginLogin starts the OAuth web flow: it returns the GitHub authorize URL to redirect to and
// the cookie that binds the login's state to this browser (CSRF). returnTo must be a
// same-origin path (ErrBadReturnTo).
func (a *Authenticator) BeginLogin(ctx context.Context, returnTo string) (string, *http.Cookie, error) {
	rt, err := SafeReturnTo(returnTo)
	if err != nil {
		return "", nil, err
	}
	if a.cfg.OAuth.ClientID == "" {
		return "", nil, fmt.Errorf("oauth: no client id configured: %w", ErrOAuth)
	}
	b, err := randomBytes(a.cfg.Rand, stateIDBytes)
	if err != nil {
		return "", nil, err
	}
	now := a.clk.Now()
	state, err := a.signState(loginState{
		ID: base64.RawURLEncoding.EncodeToString(b), Expires: now.Add(a.cfg.StateTTL).Unix(), ReturnTo: rt,
	})
	if err != nil {
		return "", nil, err
	}

	q := url.Values{
		"client_id": {a.cfg.OAuth.ClientID},
		"state":     {state},
		"scope":     {strings.Join(a.cfg.OAuth.Scopes, " ")},
	}
	if a.cfg.OAuth.RedirectURL != "" {
		q.Set("redirect_uri", a.cfg.OAuth.RedirectURL)
	}
	loc := a.cfg.OAuth.AuthorizeURL + "?" + q.Encode()
	a.log.DebugContext(ctx, "login started")
	return loc, &http.Cookie{
		Name: StateCookie, Value: state, Path: statePath,
		MaxAge: int(a.cfg.StateTTL / time.Second), Expires: now.Add(a.cfg.StateTTL),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}, nil
}

// Login is a finished login: the session cookie to set, the state cookie to clear and where
// to send the browser.
type Login struct {
	User       User
	Session    *http.Cookie
	ClearState *http.Cookie
	ReturnTo   string
}

// FinishLogin is the OAuth callback. The state must equal the state cookie of r (constant
// time), carry a valid signature under the session key (or the previous one) and not have
// expired (ErrStateMismatch). The code is exchanged for a user token first, so a callback
// without a real OAuth round trip writes nothing. Each state is used once: then the login
// creates its UsedLoginState, so a second use, on any dispatcher of the store, is
// ErrStateMismatch. The token is exchanged for the login (GET /user) and dropped. A user in neither the readers nor the admin Group is ErrNotMember and gets no
// session.
func (a *Authenticator) FinishLogin(ctx context.Context, r *http.Request, code, state string) (*Login, error) {
	c, err := r.Cookie(StateCookie)
	if err != nil || c.Value == "" || state == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		return nil, fmt.Errorf("callback state doesn't match the login's cookie: %w", ErrStateMismatch)
	}
	p, ok := a.verifyState(state)
	if !ok {
		return nil, fmt.Errorf("login state not signed by this factory: %w", ErrStateMismatch)
	}
	exp := time.Unix(p.Expires, 0)
	if !a.clk.Now().Before(exp) {
		return nil, fmt.Errorf("expired login state: %w", ErrStateMismatch)
	}
	// The code first: GitHub's codes need a user's authorization and are single-use, so only
	// a real OAuth round trip reaches the store write below.
	if code == "" {
		return nil, fmt.Errorf("no code: %w", ErrUnauthenticated)
	}
	token, err := a.exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	if err := a.useState(ctx, p.ID, exp); err != nil {
		return nil, err
	}
	gu, err := a.users.user(ctx, token) // the token goes no further: the session holds none
	if err != nil {
		return nil, err
	}
	u := User{Provider: a.cfg.Provider, Login: gu.Login, Name: gu.Name, AvatarURL: gu.AvatarURL}
	reader, err := a.IsReader(ctx, &u)
	if err != nil {
		return nil, fmt.Errorf("check membership: %w", err)
	}
	if !reader {
		a.log.InfoContext(ctx, "login refused: not a member", "login", u.Login)
		return nil, ErrNotMember
	}
	cookie, err := a.newSession(ctx, u)
	if err != nil {
		return nil, err
	}
	a.log.InfoContext(ctx, "signed in", "login", u.Login)
	return &Login{User: u, Session: cookie, ClearState: expired(StateCookie, statePath), ReturnTo: p.ReturnTo}, nil
}

// useState records that the login with state id id is finished: a create of its
// UsedLoginState, kept until the state expires. One that exists already means the state was
// used (ErrStateMismatch).
func (a *Authenticator) useState(ctx context.Context, id string, expires time.Time) error {
	err := a.d.Store.Create(ctx, &UsedLoginState{
		TypeMeta:  v1alpha1.TypeMeta{Kind: KindUsedLoginState},
		Metadata:  v1alpha1.ObjectMeta{Name: usedStateName(id)},
		ExpiresAt: expires,
	})
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		return fmt.Errorf("login state already used: %w", ErrStateMismatch)
	case err != nil:
		return fmt.Errorf("record the login state as used: %w", err)
	}
	return nil
}

// SweepUsedStates deletes the UsedLoginStates whose state has expired (the leader's sweep),
// and returns how many it deleted.
func (a *Authenticator) SweepUsedStates(ctx context.Context) (int, error) {
	var used []*UsedLoginState
	if _, err := a.d.Store.List(ctx, KindUsedLoginState, store.ListOptions{}, &used); err != nil {
		return 0, fmt.Errorf("list used login states: %w", err)
	}
	now, n := a.clk.Now(), 0
	for _, u := range used {
		if now.Before(u.ExpiresAt) {
			continue
		}
		switch err := a.d.Store.Delete(ctx, store.KeyOf(u)); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return n, fmt.Errorf("delete used login state %s: %w", u.Metadata.Name, err)
		default:
			n++
		}
	}
	return n, nil
}

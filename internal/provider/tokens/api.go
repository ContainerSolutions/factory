package tokens

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/provider"
)

// Errors for GitHub's authentication failures; match them with errors.Is. 404 and 422 match
// provider.ErrNotFound and provider.ErrInvalid instead.
var (
	// ErrUnauthorized is a 401: GitHub rejected the App JWT (wrong App ID or key, clock skew).
	ErrUnauthorized = errors.New("github: unauthorized")
	// ErrForbidden is a 403 that isn't a rate limit: the App may not do this. Errors that
	// match it match provider.ErrForbidden too.
	ErrForbidden = errors.New("github: forbidden")
)

// APIError is a GitHub error response. errors.Is maps its status: 401 → ErrUnauthorized,
// 403 → ErrForbidden and provider.ErrForbidden (not a 403 that is a rate limit), 404 and 410
// (a deleted issue) → provider.ErrNotFound, 422 → provider.ErrInvalid. Rate limits and 5xx come
// wrapped in a *provider.RetryableError instead.
type APIError struct {
	// Op is the call that failed ("create installation token").
	Op         string
	StatusCode int
	// Message is GitHub's error message, truncated.
	Message string
	// RequestID is GitHub's X-GitHub-Request-Id, for support.
	RequestID string
	// rateLimited is a 403 that is a rate limit: retryable, not forbidden.
	rateLimited bool
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github: %s: %d %s", e.Op, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request %s)", e.RequestID)
	}
	return b.String()
}

// Is maps the status to the package's and the provider's errors.
func (e *APIError) Is(target error) bool {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return target == ErrUnauthorized
	case http.StatusForbidden:
		return !e.rateLimited && (target == ErrForbidden || target == provider.ErrForbidden)
	case http.StatusNotFound, http.StatusGone:
		return target == provider.ErrNotFound
	case http.StatusUnprocessableEntity:
		return target == provider.ErrInvalid
	}
	return false
}

const (
	maxResponse   = 8 << 20
	maxMessage    = 256
	apiVersion    = "2022-11-28"
	userAgent     = "ccf-factory"
	acceptHeader  = "application/vnd.github+json"
	maxRetryAfter = time.Hour
)

// accessTokenRequest is the body of POST /app/installations/{id}/access_tokens. Neither field
// has omitempty: the body always names its one repo and its permissions.
type accessTokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// newAccessTokenRequest is the only way to build the body: exactly one repo name, at least one
// permission.
func newAccessTokenRequest(name string, perms map[string]string) (accessTokenRequest, error) {
	if name == "" || strings.Contains(name, "/") || len(perms) == 0 {
		return accessTokenRequest{}, fmt.Errorf("access token body: repo %q, %d permissions: unscoped: %w", name, len(perms), provider.ErrInvalid)
	}
	return accessTokenRequest{Repositories: []string{name}, Permissions: perms}, nil
}

type accessTokenResponse struct {
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Repositories []struct {
		Name string `json:"name"`
	} `json:"repositories"`
}

// findInstallation looks up the App's installation on owner through one of its repos
// (GET /repos/{owner}/{repo}/installation), which works for organisations and users alike.
func (m *Minter) findInstallation(ctx context.Context, id *identity, owner, name string) (cachedInstall, error) {
	var resp struct {
		ID          int64             `json:"id"`
		Permissions map[string]string `json:"permissions"`
	}
	path := "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/installation"
	if err := m.call(ctx, id, http.MethodGet, path, nil, http.StatusOK, &resp, "find installation for "+owner+"/"+name); err != nil {
		return cachedInstall{}, err
	}
	if resp.ID <= 0 {
		return cachedInstall{}, fmt.Errorf("github: find installation for %s/%s: no installation id in the response", owner, name)
	}
	return cachedInstall{id: resp.ID, permissions: resp.Permissions, fetchedAt: m.clock.Now()}, nil
}

// createToken asks GitHub for a token on exactly sc's repo with exactly sc's permissions.
func (m *Minter) createToken(ctx context.Context, id *identity, installation int64, sc scope) (cachedToken, error) {
	body, err := newAccessTokenRequest(sc.name, sc.perms)
	if err != nil {
		return cachedToken{}, err
	}
	var resp accessTokenResponse
	path := "app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	if err := m.call(ctx, id, http.MethodPost, path, body, http.StatusCreated, &resp, "create installation token"); err != nil {
		return cachedToken{}, err
	}
	switch {
	case resp.Token == "":
		return cachedToken{}, errors.New("github: create installation token: no token in the response")
	case resp.ExpiresAt.IsZero():
		return cachedToken{}, errors.New("github: create installation token: no expires_at in the response")
	case len(resp.Repositories) > 0 && (len(resp.Repositories) != 1 || !strings.EqualFold(resp.Repositories[0].Name, sc.name)):
		// Never hand out a token whose scope isn't the one asked for.
		return cachedToken{}, fmt.Errorf("github: create installation token: GitHub scoped it to %d repos, want only %s", len(resp.Repositories), sc.repo)
	}
	return cachedToken{value: resp.Token, expiresAt: resp.ExpiresAt}, nil
}

// call makes one authenticated (App JWT) GitHub call and decodes a want-status response into
// out.
func (m *Minter) call(ctx context.Context, id *identity, method, path string, in any, want int, out any, op string) error {
	_, err := m.send(ctx, id.client, "", method, path, nil, in, want, out, op)
	return err
}

// send makes one GitHub call with client and decodes a want-status response into out. auth,
// when set, is the installation token sent as the bearer (client then adds no JWT). It returns
// the response headers (for Link pagination).
func (m *Minter) send(ctx context.Context, client *http.Client, auth, method, path string, query url.Values, in any, want int, out any, op string) (http.Header, error) {
	u := m.api.JoinPath(path)
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return m.sendURL(ctx, client, auth, method, u, in, want, out, op)
}

// sendURL is send to an absolute URL on the configured API (the GraphQL endpoint). A nil out
// skips decoding the response.
func (m *Minter) sendURL(ctx context.Context, client *http.Client, auth, method string, u *url.URL, in any, want int, out any, op string) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("github: %s: encode request: %w", op, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("github: %s: %w", op, err)
	}
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("github: %s: %w", op, ctx.Err())
		}
		return nil, &provider.RetryableError{Err: fmt.Errorf("github: %s: %w", op, err)}
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("github: %s: %w", op, ctx.Err())
		}
		return nil, &provider.RetryableError{Err: fmt.Errorf("github: %s: read response: %w", op, err)}
	}
	if res.StatusCode != want {
		return nil, m.statusError(op, res, data)
	}
	if out == nil {
		return res.Header, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("github: %s: decode response: %w", op, err)
	}
	return res.Header, nil
}

// statusError maps an unexpected status: rate limits (429, or a 403 that says so), 5xx →
// *provider.RetryableError wrapping the *APIError; anything else → the *APIError.
func (m *Minter) statusError(op string, res *http.Response, data []byte) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &body)
	msg := body.Message
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "…"
	}
	apiErr := &APIError{Op: op, StatusCode: res.StatusCode, Message: msg, RequestID: res.Header.Get("X-GitHub-Request-Id")}
	retryAfter, limited := m.rateLimit(res, msg)
	apiErr.rateLimited = limited && res.StatusCode == http.StatusForbidden
	switch {
	case res.StatusCode == http.StatusTooManyRequests,
		res.StatusCode == http.StatusForbidden && limited,
		res.StatusCode >= 500:
		return &provider.RetryableError{RetryAfter: retryAfter, Err: apiErr}
	}
	return apiErr
}

// rateLimit reads GitHub's rate-limit signals: Retry-After (seconds), or an exhausted primary
// limit (X-RateLimit-Remaining: 0) with its reset time, or a secondary-limit message.
func (m *Minter) rateLimit(res *http.Response, msg string) (time.Duration, bool) {
	if s := res.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return min(time.Duration(n)*time.Second, maxRetryAfter), true
		}
	}
	if res.Header.Get("X-RateLimit-Remaining") == "0" {
		var d time.Duration
		if reset, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			d = max(time.Unix(reset, 0).Sub(m.clock.Now()), 0)
		}
		return min(d, maxRetryAfter), true
	}
	return 0, strings.Contains(strings.ToLower(msg), "rate limit")
}

// parseRSAKey parses a PEM RSA private key, PKCS#1 or PKCS#8. Errors never carry key material.
func parseRSAKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not an RSA private key (PKCS#1 or PKCS#8, block %q)", block.Type)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("a %T key, not RSA", k)
	}
	return rk, nil
}

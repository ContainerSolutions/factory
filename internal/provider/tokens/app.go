package tokens

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/provider"
)

// AppInfo is what GET /app says about an identity's App.
type AppInfo struct {
	ID int64
	// Slug is the App's URL name; the App acts on GitHub as the user "<slug>[bot]".
	Slug string
}

// BotLogin is the login an App with this slug acts as: "<slug>[bot]".
func (a AppInfo) BotLogin() string { return a.Slug + "[bot]" }

// App authenticates as the identity's App (GET /app) and returns its ID and slug.
func (m *Minter) App(ctx context.Context, identityName string) (AppInfo, error) {
	id, ok := m.idents[identityName]
	if !ok {
		return AppInfo{}, fmt.Errorf("get app: unknown identity %q: %w", identityName, provider.ErrInvalid)
	}
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err := m.call(ctx, id, http.MethodGet, "app", nil, http.StatusOK, &app, "get app"); err != nil {
		return AppInfo{}, fmt.Errorf("check app for %s: %w", identityName, err)
	}
	if app.ID != id.appID {
		return AppInfo{}, fmt.Errorf("check app for %s: GitHub answered for app %d, want %d", identityName, app.ID, id.appID)
	}
	return AppInfo{ID: app.ID, Slug: app.Slug}, nil
}

// AppGet makes one GET authenticated as the identity's App (the App JWT: /app/… endpoints such
// as the webhook deliveries) and decodes a 200 into out. It returns the response headers, for
// Link pagination. Errors map as Token's do.
func (m *Minter) AppGet(ctx context.Context, identityName, path string, query url.Values, out any) (http.Header, error) {
	id, ok := m.idents[identityName]
	if !ok {
		return nil, fmt.Errorf("app call: unknown identity %q: %w", identityName, provider.ErrInvalid)
	}
	return m.send(ctx, id.client, "", http.MethodGet, path, query, nil, http.StatusOK, out, "GET "+path)
}

// Get makes one GET authenticated with an installation token from Token or OrgToken and
// decodes a 200 into out. It returns the response headers, for Link pagination. Errors map as
// Token's do: *APIError (errors.Is: ErrUnauthorized, ErrForbidden, provider.ErrNotFound,
// provider.ErrInvalid) or *provider.RetryableError. The token is never logged.
func (m *Minter) Get(ctx context.Context, token, path string, query url.Values, out any) (http.Header, error) {
	if token == "" {
		return nil, fmt.Errorf("github: GET %s: no token: %w", path, provider.ErrInvalid)
	}
	return m.send(ctx, m.plain, token, http.MethodGet, path, query, nil, http.StatusOK, out, "GET "+path)
}

// orgPermissions are the organisation permissions OrgToken may ask for. None of them reaches a
// repository, and only read is allowed.
var orgPermissions = map[string]bool{"members": true}

// OrgToken returns an installation token on an organisation with only organisation
// permissions (members: read, to resolve teams and org roles), and its GitHub expiry. It is the
// one token whose body has no "repositories": organisation permissions don't apply to
// repositories, so there is no repo to scope it to, and the permission check below keeps every
// repository permission out of it. It is for the adapter's own reads and is never handed to an
// agent.
//
// It refuses with provider.ErrInvalid an unknown identity, an empty org, no permissions, a
// permission that isn't an organisation permission in orgPermissions, a level other than read,
// and permissions the installation wasn't granted. Tokens are cached and shared like Token's.
func (m *Minter) OrgToken(ctx context.Context, identityName, org string, perms map[string]string) (string, time.Time, error) {
	id, ok := m.idents[identityName]
	switch {
	case !ok:
		return "", time.Time{}, fmt.Errorf("mint org token: unknown identity %q (have %v): %w", identityName, m.Identities(), provider.ErrInvalid)
	case org == "" || strings.ContainsAny(org, "/ "):
		return "", time.Time{}, fmt.Errorf("mint org token: org %q: %w", org, provider.ErrInvalid)
	case len(perms) == 0:
		return "", time.Time{}, fmt.Errorf("mint org token on %s: no permissions: %w", org, provider.ErrInvalid)
	}
	for _, p := range slices.Sorted(maps.Keys(perms)) {
		if !orgPermissions[p] || perms[p] != "read" {
			return "", time.Time{}, fmt.Errorf("mint org token on %s: permission %s: %s: only read organisation permissions %v: %w",
				org, p, perms[p], slices.Sorted(maps.Keys(orgPermissions)), provider.ErrInvalid)
		}
	}
	perms = maps.Clone(perms)
	key := orgTokenKey(identityName, org, perms)
	for {
		if tok, ok := m.cached(key); ok {
			return tok.value, tok.expiresAt, nil
		}
		tok, retry, err := m.flight.do(ctx, &m.mu, key, func() (cachedToken, error) {
			if tok, ok := m.cached(key); ok {
				return tok, nil
			}
			tok, err := m.mintOrg(ctx, id, org, perms)
			if err != nil {
				return cachedToken{}, err
			}
			m.mu.Lock()
			m.pruneLocked()
			m.tokens[key] = tok
			m.mu.Unlock()
			return tok, nil
		})
		if retry {
			continue
		}
		if err != nil {
			return "", time.Time{}, fmt.Errorf("mint org token for %s on %s: %w", identityName, org, err)
		}
		return tok.value, tok.expiresAt, nil
	}
}

// orgTokenKey can't collide with a scope key: those start with the identity name.
func orgTokenKey(identity, org string, perms map[string]string) string {
	var b strings.Builder
	b.WriteString("\x01org\x00")
	b.WriteString(identity)
	b.WriteByte(0)
	b.WriteString(strings.ToLower(org))
	for _, p := range slices.Sorted(maps.Keys(perms)) {
		b.WriteByte(0)
		b.WriteString(p + "=" + perms[p])
	}
	return b.String()
}

// orgTokenRequest is the body of an org token: permissions only.
type orgTokenRequest struct {
	Permissions map[string]string `json:"permissions"`
}

func (m *Minter) mintOrg(ctx context.Context, id *identity, org string, perms map[string]string) (cachedToken, error) {
	find := func() (cachedInstall, error) { return m.findOrgInstallation(ctx, id, org) }
	inst, fresh, err := m.installationFor(ctx, id, org, find)
	if err != nil {
		return cachedToken{}, err
	}
	if err := subset(perms, inst.permissions); err != nil {
		if fresh {
			return cachedToken{}, err
		}
		m.forgetInstallation(id, org)
		if inst, _, err = m.installationFor(ctx, id, org, find); err != nil {
			return cachedToken{}, err
		}
		if err := subset(perms, inst.permissions); err != nil {
			return cachedToken{}, err
		}
	}
	var resp accessTokenResponse
	path := fmt.Sprintf("app/installations/%d/access_tokens", inst.id)
	if err := m.call(ctx, id, http.MethodPost, path, orgTokenRequest{Permissions: perms}, http.StatusCreated, &resp, "create org installation token"); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && slices.Contains([]int{401, 404, 422}, apiErr.StatusCode) {
			m.forgetInstallation(id, org)
		}
		return cachedToken{}, err
	}
	switch {
	case resp.Token == "":
		return cachedToken{}, errors.New("github: create org installation token: no token in the response")
	case resp.ExpiresAt.IsZero():
		return cachedToken{}, errors.New("github: create org installation token: no expires_at in the response")
	}
	m.log.DebugContext(ctx, "github org token minted", "identity", id.name, "org", org,
		"installation", inst.id, "permissions", perms, "expiresAt", resp.ExpiresAt)
	return cachedToken{value: resp.Token, expiresAt: resp.ExpiresAt}, nil
}

// findOrgInstallation looks up the App's installation on an organisation
// (GET /orgs/{org}/installation).
func (m *Minter) findOrgInstallation(ctx context.Context, id *identity, org string) (cachedInstall, error) {
	var resp struct {
		ID          int64             `json:"id"`
		Permissions map[string]string `json:"permissions"`
	}
	path := "orgs/" + url.PathEscape(org) + "/installation"
	if err := m.call(ctx, id, http.MethodGet, path, nil, http.StatusOK, &resp, "find installation for org "+org); err != nil {
		return cachedInstall{}, err
	}
	if resp.ID <= 0 {
		return cachedInstall{}, fmt.Errorf("github: find installation for org %s: no installation id in the response", org)
	}
	return cachedInstall{id: resp.ID, permissions: resp.Permissions, fetchedAt: m.clock.Now()}, nil
}

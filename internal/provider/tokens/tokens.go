// Package tokens mints GitHub App installation tokens for the factory's App identities
// (design/workstreams/provider-github/LLD/github-provider.md §Auth and tokens).
//
// Every token is scoped to exactly one repository and to an explicit permission subset of
// what the identity's installation was granted: the only way in is Minter.Token, which takes a
// provider.GitCredentialRequest, refuses what its Validate refuses, refuses ReadRepos and an
// empty permission set, and always sends "repositories": [<name>] and "permissions": {…}. So
// an unscoped token can't be minted by construction. The two exceptions carry no repository
// write: OrgToken (organisation permissions only) and the metadata: read token
// InstallationRepos keeps to itself.
//
// Tokens are cached per (identity, repo, permissions) and reused until RefreshBefore before
// GitHub's expiry; concurrent requests for the same key share one GitHub call. Installation
// IDs are resolved per (identity, account) and cached too.
package tokens

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"

	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
)

// Defaults and limits.
const (
	// DefaultAPIURL is github.com's REST API.
	DefaultAPIURL = "https://api.github.com"
	// DefaultHost is github.com's git host.
	DefaultHost = "github.com"
	// Username is the git username that goes with an installation token.
	Username = "x-access-token"
	// MaxTTL caps a credential's lifetime; GitHub's installation tokens last an hour.
	MaxTTL = time.Hour
	// RefreshBefore is how long before GitHub's expiry a cached token stops being reused.
	RefreshBefore = 5 * time.Minute
	// DefaultInstallationTTL is how long an installation lookup is cached.
	DefaultInstallationTTL = time.Hour
	// DefaultTimeout bounds each GitHub call when the HTTP client has no timeout of its own.
	DefaultTimeout = 30 * time.Second
)

// Identity is one GitHub App the minter acts as.
type Identity struct {
	AppID int64
	// PrivateKey is the App's PEM private key (PKCS#1, as GitHub issues it, or PKCS#8).
	PrivateKey []byte
}

// Options configure a Minter. The zero value mints against github.com with the wall clock.
type Options struct {
	// APIURL is the REST API base (default DefaultAPIURL; "https://<host>/api/v3" on GHE).
	APIURL string
	// HTTPClient makes the calls (default: a client with DefaultTimeout). Its Transport is
	// wrapped with the App JWT; redirects are never followed, so the JWT can't leave APIURL.
	HTTPClient *http.Client
	// Clock times the caches and credential expiry (default clock.Real).
	Clock clock.Clock
	// Logger gets debug lines for mints and cache hits; tokens and JWTs are never logged.
	Logger *slog.Logger
	// Provider is the Provider instance name set on credentials ("github-public").
	Provider string
	// Host is the git host set on credentials (default: APIURL's host, "github.com" for
	// api.github.com).
	Host string
	// InstallationTTL is how long an installation lookup is reused (default
	// DefaultInstallationTTL).
	InstallationTTL time.Duration
}

// Minter mints installation tokens. It is safe for concurrent use.
type Minter struct {
	api      *url.URL
	graphql  *url.URL
	clock    clock.Clock
	log      *slog.Logger
	provider string
	host     string
	instTTL  time.Duration
	idents   map[string]*identity
	// plain makes the calls authenticated with an installation token (Get); like the App
	// clients, it never follows a redirect, so a token can't leave APIURL.
	plain *http.Client

	mu     sync.Mutex
	tokens map[string]cachedToken   // by tokenKey
	insts  map[string]cachedInstall // by installKey
	flight flight[cachedToken]      // token mints, by tokenKey
	lookup flight[cachedInstall]    // installation lookups, by installKey
}

type identity struct {
	name   string
	appID  int64
	client *http.Client
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

type cachedInstall struct {
	id          int64
	permissions map[string]string
	fetchedAt   time.Time
}

// New returns a Minter for identities (by name: "author", "reviewer"). It parses every
// private key up front, so a bad key fails here rather than at the first mint.
func New(identities map[string]Identity, opts Options) (*Minter, error) {
	if len(identities) == 0 {
		return nil, fmt.Errorf("tokens: no identities: %w", provider.ErrInvalid)
	}
	apiURL := opts.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	api, err := url.Parse(strings.TrimRight(apiURL, "/"))
	if err != nil || (api.Scheme != "https" && api.Scheme != "http") || api.Host == "" {
		return nil, fmt.Errorf("tokens: API URL %q: not an http(s) URL: %w", apiURL, provider.ErrInvalid)
	}
	m := &Minter{
		api:      api,
		graphql:  GraphQLURL(api),
		clock:    opts.Clock,
		log:      opts.Logger,
		provider: opts.Provider,
		host:     opts.Host,
		instTTL:  opts.InstallationTTL,
		idents:   map[string]*identity{},
		tokens:   map[string]cachedToken{},
		insts:    map[string]cachedInstall{},
	}
	if m.clock == nil {
		m.clock = clock.Real{}
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.host == "" {
		m.host = HostForAPI(api)
	}
	if m.instTTL <= 0 {
		m.instTTL = DefaultInstallationTTL
	}
	base := opts.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: DefaultTimeout}
	}
	tr := base.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	timeout := base.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	m.plain = &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: noRedirect}
	for _, name := range slices.Sorted(maps.Keys(identities)) {
		id := identities[name]
		if name == "" {
			return nil, fmt.Errorf("tokens: identity with no name: %w", provider.ErrInvalid)
		}
		if id.AppID <= 0 {
			return nil, fmt.Errorf("tokens: identity %q: app ID %d: %w", name, id.AppID, provider.ErrInvalid)
		}
		key, err := parseKey(id.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("tokens: identity %q: %w", name, err)
		}
		apps := ghinstallation.NewAppsTransportFromPrivateKey(tr, id.AppID, key)
		apps.BaseURL = api.String()
		m.idents[name] = &identity{
			name:  name,
			appID: id.AppID,
			client: &http.Client{
				Transport: apps,
				Timeout:   timeout,
				// Never follow a redirect: the App transport would sign the next hop too.
				CheckRedirect: noRedirect,
			},
		}
	}
	return m, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// HostForAPI is the git host of a REST API base: github.com for api.github.com, the GHE host
// otherwise.
func HostForAPI(api *url.URL) string {
	h := api.Hostname()
	if strings.EqualFold(h, "api.github.com") {
		return DefaultHost
	}
	if p := api.Port(); p != "" {
		return h + ":" + p
	}
	return h
}

// Identities returns the identity names, sorted.
func (m *Minter) Identities() []string { return slices.Sorted(maps.Keys(m.idents)) }

// Token returns an installation token for req: one repo (req.Repo), req.Permissions (a
// subset of what the identity's installation on the repo's owner was granted), and an
// ExpiresAt no later than GitHub's expiry nor min(req.TTL, MaxTTL) from now. A cached token for
// the same (identity, repo, permissions) is reused while it has more than RefreshBefore left.
//
// It refuses with provider.ErrInvalid a request GitCredentialRequest.Validate refuses, one with
// ReadRepos (every token covers exactly one repo), one with no permissions, unknown
// permission names or levels, a repo that isn't "owner/name", an unknown identity, and
// permissions the installation wasn't granted. GitHub's errors map to *APIError (errors.Is:
// ErrUnauthorized, ErrForbidden, provider.ErrNotFound, provider.ErrInvalid) or, for rate limits,
// 5xx and transport failures, *provider.RetryableError.
func (m *Minter) Token(ctx context.Context, req provider.GitCredentialRequest) (provider.Credential, error) {
	sc, err := newScope(req)
	if err != nil {
		return provider.Credential{}, err
	}
	id, ok := m.idents[sc.identity]
	if !ok {
		return provider.Credential{}, fmt.Errorf("mint token: unknown identity %q (have %v): %w", sc.identity, m.Identities(), provider.ErrInvalid)
	}
	tok, err := m.token(ctx, id, sc)
	if err != nil {
		return provider.Credential{}, fmt.Errorf("mint token for %s on %s: %w", sc.identity, sc.repo, err)
	}
	expires := tok.expiresAt
	if limit := m.clock.Now().Add(min(sc.ttl, MaxTTL)); limit.Before(expires) {
		expires = limit
	}
	return provider.Credential{
		Provider:    m.provider,
		Host:        m.host,
		Username:    Username,
		Token:       tok.value,
		ExpiresAt:   expires,
		Repo:        sc.repo,
		Permissions: maps.Clone(sc.perms),
	}, nil
}

// token returns a cached token or mints one, sharing a mint already in flight for the key.
func (m *Minter) token(ctx context.Context, id *identity, sc scope) (cachedToken, error) {
	key := sc.key()
	for {
		if tok, ok := m.cached(key); ok {
			m.log.DebugContext(ctx, "github token cache hit", "identity", sc.identity, "repo", sc.repo,
				"permissions", sc.perms, "expiresAt", tok.expiresAt)
			return tok, nil
		}
		tok, retry, err := m.flight.do(ctx, &m.mu, key, func() (cachedToken, error) {
			// Another caller may have stored it between our cache miss and this flight.
			if tok, ok := m.cached(key); ok {
				return tok, nil
			}
			tok, err := m.mint(ctx, id, sc)
			if err != nil {
				return cachedToken{}, err
			}
			m.mu.Lock()
			m.pruneLocked()
			m.tokens[key] = tok
			m.mu.Unlock()
			return tok, nil
		})
		// The caller whose mint we shared gave up (its context ended); ours hasn't: try again.
		if retry {
			continue
		}
		return tok, err
	}
}

// cached returns the token for key if it has more than RefreshBefore left.
func (m *Minter) cached(key string) (cachedToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.tokens[key]
	if !ok || !m.clock.Now().Before(tok.expiresAt.Add(-RefreshBefore)) {
		return cachedToken{}, false
	}
	return tok, true
}

// pruneLocked drops the tokens that can no longer be reused.
func (m *Minter) pruneLocked() {
	now := m.clock.Now()
	for k, tok := range m.tokens {
		if !now.Before(tok.expiresAt.Add(-RefreshBefore)) {
			delete(m.tokens, k)
		}
	}
}

// mint resolves the installation, checks the permissions against it and asks GitHub for the
// token.
func (m *Minter) mint(ctx context.Context, id *identity, sc scope) (cachedToken, error) {
	inst, fresh, err := m.installation(ctx, id, sc)
	if err != nil {
		return cachedToken{}, err
	}
	if err := subset(sc.perms, inst.permissions); err != nil {
		if fresh {
			return cachedToken{}, err
		}
		// The cached installation may predate a permission change: look again before refusing.
		m.forgetInstallation(id, sc.owner)
		if inst, _, err = m.installation(ctx, id, sc); err != nil {
			return cachedToken{}, err
		}
		if err := subset(sc.perms, inst.permissions); err != nil {
			return cachedToken{}, err
		}
	}
	tok, err := m.createToken(ctx, id, inst.id, sc)
	if err != nil {
		// 401/404/422 can mean the installation went away or changed: look it up next time.
		var apiErr *APIError
		if errors.As(err, &apiErr) && slices.Contains([]int{401, 404, 422}, apiErr.StatusCode) {
			m.forgetInstallation(id, sc.owner)
		}
		return cachedToken{}, err
	}
	m.log.DebugContext(ctx, "github token minted", "identity", sc.identity, "repo", sc.repo,
		"installation", inst.id, "permissions", sc.perms, "expiresAt", tok.expiresAt)
	return tok, nil
}

// installation returns the identity's installation on the repo's owner, from the cache when
// it is younger than InstallationTTL; fresh is true when it was just looked up.
func (m *Minter) installation(ctx context.Context, id *identity, sc scope) (cachedInstall, bool, error) {
	return m.installationFor(ctx, id, sc.owner, func() (cachedInstall, error) {
		return m.findInstallation(ctx, id, sc.owner, sc.name)
	})
}

// installationFor returns the identity's installation on owner, from the cache when it is
// younger than InstallationTTL, else through find (shared by concurrent callers); fresh is
// true when it was just looked up. An account has one installation per App, so the repo and
// org lookups share the cache.
func (m *Minter) installationFor(ctx context.Context, id *identity, owner string, find func() (cachedInstall, error)) (cachedInstall, bool, error) {
	key := installKey(id.name, owner)
	m.mu.Lock()
	inst, ok := m.insts[key]
	m.mu.Unlock()
	if ok && m.clock.Now().Before(inst.fetchedAt.Add(m.instTTL)) {
		return inst, false, nil
	}
	for {
		inst, retry, err := m.lookup.do(ctx, &m.mu, key, func() (cachedInstall, error) {
			inst, err := find()
			if err != nil {
				return cachedInstall{}, err
			}
			m.mu.Lock()
			m.insts[key] = inst
			m.mu.Unlock()
			return inst, nil
		})
		if retry {
			continue
		}
		return inst, true, err
	}
}

func (m *Minter) forgetInstallation(id *identity, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.insts, installKey(id.name, owner))
}

// CheckApp authenticates as the identity's App (GET /app): it proves the App ID, the private
// key and GitHub's reachability, without minting a token.
func (m *Minter) CheckApp(ctx context.Context, identityName string) error {
	_, err := m.App(ctx, identityName)
	return err
}

// ---- scope ----------------------------------------------------------------------------------

// scope is a validated request: one repo, explicit permissions.
type scope struct {
	identity string
	repo     string // owner/name
	owner    string
	name     string
	perms    map[string]string
	ttl      time.Duration
}

var permissionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func newScope(req provider.GitCredentialRequest) (scope, error) {
	if err := req.Validate(); err != nil {
		return scope{}, fmt.Errorf("mint token: %w", err)
	}
	if len(req.ReadRepos) > 0 {
		return scope{}, fmt.Errorf("mint token: readRepos %v: a token covers exactly one repo (one read-only request per repo): %w", req.ReadRepos, provider.ErrInvalid)
	}
	owner, name, ok := strings.Cut(req.Repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return scope{}, fmt.Errorf("mint token: repo %q: want owner/name: %w", req.Repo, provider.ErrInvalid)
	}
	if len(req.Permissions) == 0 {
		return scope{}, fmt.Errorf("mint token for %s: no permissions (a token always names its permissions): %w", req.Repo, provider.ErrInvalid)
	}
	for perm, level := range req.Permissions {
		if !permissionName.MatchString(perm) {
			return scope{}, fmt.Errorf("mint token for %s: permission name %q: %w", req.Repo, perm, provider.ErrInvalid)
		}
		if rank(level) == 0 {
			return scope{}, fmt.Errorf("mint token for %s: permission %s: level %q (want read, write or admin): %w", req.Repo, perm, level, provider.ErrInvalid)
		}
	}
	return scope{
		identity: req.Identity,
		repo:     owner + "/" + name,
		owner:    owner,
		name:     name,
		perms:    maps.Clone(req.Permissions),
		ttl:      req.TTL,
	}, nil
}

// key is the cache key: identity, repo (case-folded, as GitHub treats it) and the sorted
// permissions.
func (s scope) key() string {
	var b strings.Builder
	b.WriteString(s.identity)
	b.WriteByte(0)
	b.WriteString(strings.ToLower(s.repo))
	for _, p := range slices.Sorted(maps.Keys(s.perms)) {
		b.WriteByte(0)
		b.WriteString(p)
		b.WriteByte('=')
		b.WriteString(s.perms[p])
	}
	return b.String()
}

func installKey(identity, owner string) string { return identity + "\x00" + strings.ToLower(owner) }

// rank orders permission levels; 0 is not a level.
func rank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// subset returns an ErrInvalid error unless every requested permission is granted at its
// level or above.
func subset(requested, granted map[string]string) error {
	var missing []string
	for _, p := range slices.Sorted(maps.Keys(requested)) {
		if rank(granted[p]) < rank(requested[p]) {
			have := granted[p]
			if have == "" {
				have = "none"
			}
			missing = append(missing, fmt.Sprintf("%s: %s (granted %s)", p, requested[p], have))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("permissions beyond the identity's installation: %s: %w", strings.Join(missing, ", "), provider.ErrInvalid)
	}
	return nil
}

func parseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	if len(pemBytes) == 0 {
		return nil, fmt.Errorf("no private key: %w", provider.ErrInvalid)
	}
	key, err := parseRSAKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w: %w", err, provider.ErrInvalid)
	}
	return key, nil
}

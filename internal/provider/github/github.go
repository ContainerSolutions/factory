// Package github is the provider adapter for GitHub and GHE
// (design/workstreams/provider-github/LLD/github-provider.md): App auth and scoped git
// credentials (GH-S1-T01), webhook ingress and normalisation (GH-S1-T02), observe, list and
// projection (GH-S1-T03) and effects (GH-S1-T04).
//
// It acts as the Provider's App identities ("author", "reviewer"), whose installation tokens
// internal/provider/tokens mints, one repo per token.
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/tokens"
)

// Type is the Provider spec.type this adapter connects.
const Type = string(v1alpha1.ProviderTypeGitHub)

// Options are the adapter-wide dependencies, shared by every Conn.
type Options struct {
	// HTTPClient makes the GitHub calls (default: tokens' default client).
	HTTPClient *http.Client
	// Clock times token caches, credential expiry and health (default clock.Real).
	Clock clock.Clock
	// Logger gets the adapter's logs (default: discarded). Tokens are never logged.
	Logger *slog.Logger
	// Groups resolves the catalog Groups named by trust rules; nil means group rules never
	// match.
	Groups provider.GroupLookup
	// TrainRepos are the repos ("owner/name") whose closed issues carry a release train's
	// state, read by the ccf-train extractor (default DefaultTrainRepos).
	TrainRepos []string
	// UIBaseURL is the factory's public URL, which the projection marker's visible line links
	// to (<UIBaseURL>/workitems/<id>); without it the line has no link.
	UIBaseURL string
	// DeployEnvironment is the environment whose latest deployment of the default branch
	// Observe reports for merged PRs (default DefaultDeployEnvironment).
	DeployEnvironment string
}

// DefaultTrainRepos is where CCF's release train keeps its tracking issues.
var DefaultTrainRepos = []string{"compliance-framework/workflows"}

// Adapter connects Provider objects of type github.
type Adapter struct {
	opts Options
}

var _ provider.Adapter = (*Adapter)(nil)

// NewAdapter returns the GitHub adapter.
func NewAdapter(opts Options) *Adapter {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.TrainRepos == nil {
		opts.TrainRepos = DefaultTrainRepos
	}
	opts.TrainRepos = slices.Clone(opts.TrainRepos)
	if opts.DeployEnvironment == "" {
		opts.DeployEnvironment = DefaultDeployEnvironment
	}
	return &Adapter{opts: opts}
}

// Type implements provider.Adapter.
func (a *Adapter) Type() string { return Type }

// Connect implements provider.Adapter. It resolves the webhook secret and every identity's
// private key through secrets and parses the keys, so a missing secret or a bad key fails
// here; it makes no GitHub call (CheckHealth does).
func (a *Adapter) Connect(ctx context.Context, p v1alpha1.Provider, secrets provider.SecretResolver) (provider.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := p.Metadata.Name
	switch {
	case name == "":
		return nil, fmt.Errorf("github: connect: provider has no name: %w", provider.ErrInvalid)
	case string(p.Spec.Type) != Type:
		return nil, fmt.Errorf("github: connect %q: type %q: %w", name, p.Spec.Type, provider.ErrInvalid)
	case p.Spec.GitHub == nil:
		return nil, fmt.Errorf("github: connect %q: no spec.github: %w", name, provider.ErrInvalid)
	case len(p.Spec.GitHub.Identities) == 0:
		return nil, fmt.Errorf("github: connect %q: no identities: %w", name, provider.ErrInvalid)
	case secrets == nil:
		return nil, fmt.Errorf("github: connect %q: no secret resolver: %w", name, provider.ErrInvalid)
	}
	spec := p.Spec.GitHub
	hook := spec.Webhook.SecretRef
	if hook.Name == "" || hook.Key == "" {
		return nil, fmt.Errorf("github: connect %q: webhook secretRef needs name and key: %w", name, provider.ErrInvalid)
	}
	webhookSecret, err := secrets.Resolve(ctx, hook)
	if err != nil {
		return nil, fmt.Errorf("github: connect %q: resolve webhook secret %s/%s: %w", name, hook.Name, hook.Key, err)
	}
	if len(webhookSecret) == 0 {
		return nil, fmt.Errorf("github: connect %q: webhook secret %s/%s is empty: %w", name, hook.Name, hook.Key, provider.ErrInvalid)
	}
	idents := make(map[string]tokens.Identity, len(spec.Identities))
	for _, role := range slices.Sorted(maps.Keys(spec.Identities)) {
		app := spec.Identities[role]
		ref := app.PrivateKeyRef
		if ref.Name == "" || ref.Key == "" {
			return nil, fmt.Errorf("github: connect %q: identity %q: privateKeyRef needs name and key: %w", name, role, provider.ErrInvalid)
		}
		key, err := secrets.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("github: connect %q: identity %q: resolve secret %s/%s: %w", name, role, ref.Name, ref.Key, err)
		}
		idents[role] = tokens.Identity{AppID: app.AppID, PrivateKey: key}
	}
	minter, err := tokens.New(idents, tokens.Options{
		APIURL:     spec.APIURL,
		HTTPClient: a.opts.HTTPClient,
		Clock:      a.opts.Clock,
		Logger:     a.opts.Logger.With("provider", name),
		Provider:   name,
	})
	if err != nil {
		return nil, fmt.Errorf("github: connect %q: %w", name, err)
	}
	ids := minter.Identities()
	readIdentity := ids[0]
	if slices.Contains(ids, EventIdentity) {
		readIdentity = EventIdentity
	}
	var orgs []string
	if p.Spec.Scope != nil {
		orgs = slices.Clone(p.Spec.Scope.Orgs)
	}
	return &Conn{
		name:          name,
		orgs:          orgs,
		uiBase:        a.opts.UIBaseURL,
		deployEnv:     a.opts.DeployEnvironment,
		minter:        minter,
		clock:         a.opts.Clock,
		log:           a.opts.Logger.With("provider", name),
		webhookSecret: webhookSecret,
		trust:         slices.Clone(p.Spec.Trust),
		groups:        a.opts.Groups,
		trainRepos:    a.opts.TrainRepos,
		readIdentity:  readIdentity,
		health:        map[string]identityHealth{},
		memberCache:   map[string]membersEntry{},
	}, nil
}

// EventIdentity is the identity whose App is the event source (the only App with a webhook)
// and holds the read permissions normalisation needs (members, contents, pull requests). When
// a Provider has no such identity, its first identity by name is used.
const EventIdentity = "author"

// Conn is a connection to one GitHub Provider instance. It is safe for concurrent use.
type Conn struct {
	name string
	// orgs is the Provider's scope.orgs: List's outer bound and where its globs look.
	orgs          []string
	uiBase        string
	deployEnv     string
	minter        *tokens.Minter
	clock         clock.Clock
	log           *slog.Logger
	webhookSecret []byte
	trust         []v1alpha1.TrustRule
	groups        provider.GroupLookup
	trainRepos    []string
	// readIdentity mints the tokens for the adapter's own reads (EventIdentity).
	readIdentity string

	mu     sync.Mutex
	health map[string]identityHealth // by identity; absent until first checked
	// bots maps the factory Apps' bot logins (lower-case) to identities; nil until learned.
	bots        map[string]string
	memberCache map[string]membersEntry // group member sources resolved for trust, by source key
}

var _ provider.Conn = (*Conn)(nil)

// identityHealth is the last outcome of authenticating as one identity.
type identityHealth struct {
	err error
	at  time.Time
}

// MintGitCredential implements provider.Conn: one installation token on req.Repo with
// req.Permissions, via tokens.Minter.Token (which refuses unscoped or over-broad requests with
// provider.ErrInvalid). Outcomes that say whether the identity can authenticate update Health.
func (c *Conn) MintGitCredential(ctx context.Context, req provider.GitCredentialRequest) (provider.Credential, error) {
	cred, err := c.minter.Token(ctx, req)
	c.record(req.Identity, err)
	if err != nil {
		return provider.Credential{}, fmt.Errorf("github %s: %w", c.name, err)
	}
	return cred, nil
}

// CheckHealth authenticates as every identity's App (GET /app), records the outcomes and
// returns the resulting Health. `factory serve` calls it when it connects the Provider and
// again whenever the catalog changes.
func (c *Conn) CheckHealth(ctx context.Context) provider.HealthStatus {
	idents := c.minter.Identities()
	errs := make([]error, len(idents))
	var wg sync.WaitGroup
	for i, id := range idents {
		wg.Go(func() { errs[i] = c.minter.CheckApp(ctx, id) })
	}
	wg.Wait()
	now := c.clock.Now()
	c.mu.Lock()
	for i, id := range idents {
		if errs[i] != nil && ctx.Err() != nil {
			continue // we gave up; that says nothing about the identity
		}
		c.health[id] = identityHealth{err: errs[i], at: now}
	}
	c.mu.Unlock()
	return c.Health()
}

// record updates an identity's health after a mint. Only outcomes about the identity itself
// count: success, an auth failure (401/403) or GitHub being unreachable (retryable). A refused
// request, a repo the App isn't installed on or a cancelled context say nothing about it.
func (c *Conn) record(identity string, err error) {
	switch {
	case err == nil,
		errors.Is(err, tokens.ErrUnauthorized),
		errors.Is(err, tokens.ErrForbidden),
		errors.Is(err, provider.ErrRetryable):
	default:
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.minter.Identities(), identity) {
		return
	}
	c.health[identity] = identityHealth{err: err, at: c.clock.Now()}
}

// Health implements provider.Conn: ok when every identity's last check or mint succeeded,
// down when every one failed, degraded in between, unknown before anything was checked. It
// doesn't block.
func (c *Conn) Health() provider.HealthStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.health) == 0 {
		return provider.HealthStatus{Status: provider.HealthUnknown, Message: "not checked yet"}
	}
	var failing []string
	var checked time.Time
	for _, id := range slices.Sorted(maps.Keys(c.health)) {
		h := c.health[id]
		if h.at.After(checked) {
			checked = h.at
		}
		if h.err != nil {
			failing = append(failing, fmt.Sprintf("%s: %s", id, healthReason(h.err)))
		}
	}
	st := provider.HealthStatus{Status: provider.HealthOK, CheckedAt: checked}
	switch {
	case len(failing) == 0:
	case len(failing) == len(c.minter.Identities()):
		st.Status, st.Message = provider.HealthDown, strings.Join(failing, "; ")
	default:
		st.Status, st.Message = provider.HealthDegraded, strings.Join(failing, "; ")
	}
	return st
}

// healthReason is a short, secret-free reason for a failed check.
func healthReason(err error) string {
	switch {
	case errors.Is(err, tokens.ErrUnauthorized):
		return "GitHub rejected the App JWT (check the App ID and private key)"
	case errors.Is(err, tokens.ErrForbidden):
		return "GitHub refused the App"
	case errors.Is(err, provider.ErrRetryable):
		return "GitHub unreachable or rate-limited"
	}
	return err.Error()
}

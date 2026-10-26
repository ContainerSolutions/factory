package controllers

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/cel"
	"github.com/compliance-framework/factory/internal/provider"
)

// DefaultInstallationCheckTimeout bounds one InstallationCheck.Check.
const DefaultInstallationCheckTimeout = time.Minute

// InstallationTarget is the repos one identity of one github Provider can touch.
type InstallationTarget struct {
	Provider, Identity string
	// Repos are "owner/name", sorted, without duplicates.
	Repos []string
}

// InstallationTargets lists the repos each identity of each github Provider can touch, from
// the catalog. For each RuleSet bound to a github Provider: its literal scope repos (the
// targets its work items link; a glob names no repo), as the identity of every TaskSpec its
// rules dispatch and, when they notify or apply effects, as ruleIdentity; and, for each such
// TaskSpec, its literal workspace repos, a literal dispatch target and its skill sources on
// the Provider's git host (a source that isn't a URL is a repo under skillsBase). Sorted by
// provider, then identity.
func InstallationTargets(cat *catalog.Catalog, ruleIdentity, skillsBase string) []InstallationTarget {
	repos := map[[2]string]map[string]bool{}
	add := func(prov, identity, repo string) {
		if identity == "" || repo == "" || !strings.Contains(repo, "/") {
			return
		}
		k := [2]string{prov, identity}
		if repos[k] == nil {
			repos[k] = map[string]bool{}
		}
		repos[k][repo] = true
	}
	for _, rs := range cat.RuleSets {
		var scope []string
		if s := rs.Spec.Spec.Scope; s != nil {
			for _, r := range s.Repos {
				if literalRepo(r) && rs.Scope.MatchRepo(r) {
					scope = append(scope, r)
				}
			}
		}
		for _, prov := range rs.Providers {
			p := cat.Providers[prov]
			if p == nil || p.Spec.Type != v1alpha1.ProviderTypeGitHub {
				continue
			}
			host := GitHost(p)
			for _, rule := range rs.Rules {
				for _, a := range rule.Actions {
					switch {
					case a.Action.Dispatch != nil:
						d := a.Action.Dispatch
						ts := cat.TaskSpecs[d.TaskSpec]
						if ts == nil {
							continue
						}
						id := ts.Spec.Spec.Identity
						for _, r := range scope {
							add(prov, id, r)
						}
						if t := d.Target; t != nil && literalRepo(t.Repo) {
							add(prov, id, t.Repo)
						}
						for _, r := range taskSpecRepos(ts.Spec.Spec, host, skillsBase) {
							add(prov, id, r)
						}
					case a.Action.Notify != nil, a.Action.Effect != nil:
						for _, r := range scope {
							add(prov, ruleIdentity, r)
						}
					}
				}
			}
		}
	}
	out := make([]InstallationTarget, 0, len(repos))
	for k, set := range repos {
		rs := make([]string, 0, len(set))
		for r := range set {
			rs = append(rs, r)
		}
		slices.Sort(rs)
		out = append(out, InstallationTarget{Provider: k[0], Identity: k[1], Repos: rs})
	}
	slices.SortFunc(out, func(a, b InstallationTarget) int {
		return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Identity, b.Identity))
	})
	return out
}

// taskSpecRepos are a TaskSpec's literal workspace repos and its skill sources on host.
func taskSpecRepos(spec v1alpha1.TaskSpecSpec, host, skillsBase string) []string {
	var out []string
	if ws := spec.Workspace; ws != nil {
		for _, m := range ws.Repos {
			if _, isCEL := cel.CELSource(m.Ref); isCEL {
				continue
			}
			repo, _, _ := strings.Cut(cel.Unescape(m.Ref), "@")
			if literalRepo(repo) {
				out = append(out, repo)
			}
		}
	}
	for _, s := range spec.Skills {
		src := s.Source
		if !strings.Contains(src, "://") {
			if skillsBase == "" {
				continue
			}
			src = strings.TrimSuffix(skillsBase, "/") + "/" + src
		}
		if repo, ok := RepoOnHost(src, host); ok {
			out = append(out, repo)
		}
	}
	return out
}

// literalRepo reports whether r is one "owner/name", not a glob.
func literalRepo(r string) bool {
	owner, name, ok := strings.Cut(r, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(r, "*?[]{}") && !strings.Contains(name, "/")
}

// InstallationGap is an identity's App that isn't installed on a repo the catalog can touch.
type InstallationGap struct {
	Provider, Identity string
	// App is the App's slug, "" when it couldn't be learnt.
	App  string
	Repo string
}

// Message is the gap as a sentence for the catalog status.
func (g InstallationGap) Message() string {
	return provider.NotInstalledMessage(g.Identity, g.App, g.Repo) + " (provider " + g.Provider + "): install it there"
}

// InstallationCheckOptions configure an InstallationCheck. Zero fields take the defaults.
type InstallationCheckOptions struct {
	// RuleIdentity is the identity of rule-level notifications and effects. Default
	// DefaultRuleIdentity.
	RuleIdentity string
	// SkillsBase is where a skill source that isn't a URL lives (`--skills-base`).
	SkillsBase string
	// Timeout bounds one Check. Default DefaultInstallationCheckTimeout.
	Timeout time.Duration
	// Logger. Default slog.Default().
	Logger *slog.Logger
}

// InstallationCheck checks that each identity's App is installed on every repo the catalog can
// touch (InstallationTargets), through the Conns that are provider.InstallationCheckers. Each
// gap is logged at WARN; Gaps reports the last check's. It is safe for concurrent use.
type InstallationCheck struct {
	conns ConnSource
	opts  InstallationCheckOptions
	log   *slog.Logger

	mu   sync.Mutex
	gaps []InstallationGap
}

// NewInstallationCheck returns a check over conns.
func NewInstallationCheck(conns ConnSource, opts InstallationCheckOptions) *InstallationCheck {
	if opts.RuleIdentity == "" {
		opts.RuleIdentity = DefaultRuleIdentity
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultInstallationCheckTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &InstallationCheck{conns: conns, opts: opts, log: opts.Logger.With("component", "installations")}
}

// Gaps returns the last check's gaps.
func (c *InstallationCheck) Gaps() []InstallationGap {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.gaps)
}

// Check checks cat's targets and keeps the gaps it finds. A target whose check fails (GitHub
// unreachable, the App's key refused) is logged and keeps no gaps: the check reports what it
// knows, and the next one tries again.
func (c *InstallationCheck) Check(ctx context.Context, cat *catalog.Catalog) []InstallationGap {
	if cat == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	var gaps []InstallationGap
	for _, t := range InstallationTargets(cat, c.opts.RuleIdentity, c.opts.SkillsBase) {
		conn, ok := c.conns.Conn(t.Provider)
		if !ok {
			continue
		}
		ic, ok := conn.(provider.InstallationChecker)
		if !ok {
			continue
		}
		app, missing, err := ic.MissingInstallations(ctx, t.Identity, t.Repos)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			c.log.WarnContext(ctx, "App installation check failed", "provider", t.Provider, "identity", t.Identity, "err", err)
			continue
		}
		for _, r := range missing {
			g := InstallationGap{Provider: t.Provider, Identity: t.Identity, App: app, Repo: r}
			c.log.WarnContext(ctx, "App not installed on a repo the catalog can touch", "provider", t.Provider,
				"identity", t.Identity, "app", app, "repo", r)
			gaps = append(gaps, g)
		}
	}
	c.mu.Lock()
	c.gaps = gaps
	c.mu.Unlock()
	return slices.Clone(gaps)
}

// Follow checks the live catalog at once and again after every swap, until ctx is done.
func (c *InstallationCheck) Follow(ctx context.Context, cs CatalogSnapshotter) error {
	cat, changed := cs.Snapshot()
	for {
		c.Check(ctx, cat)
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
		cat, changed = cs.Snapshot()
	}
}

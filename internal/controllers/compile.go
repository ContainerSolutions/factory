package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	neturl "net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/catalog/gitsource"
	"github.com/compliance-framework/factory/internal/cel"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/tokens"
	"github.com/compliance-framework/factory/internal/rules"
)

// Task compilation defaults.
const (
	// MaxCredentialTTL caps a task's git credential (design/02 §Safety rails: ≤ 1h).
	MaxCredentialTTL = time.Hour
	// credentialSlack is added to maxWallClock for the credential TTL.
	credentialSlack = 10 * time.Minute
	// LabelTaskSpec is the Task label holding its TaskSpec name.
	LabelTaskSpec = "factory.ccf.dev/taskspec"
	// LLMAgentLocal is the only LLM credential mode: the agent's own harness login.
	LLMAgentLocal = "agent-local"
	// TrustUntrusted labels context the agent must treat as untrusted input.
	TrustUntrusted = "untrusted"
)

// SkillResolver resolves a TaskSpec skill (source, ref tag) to a repo URL and a commit.
type SkillResolver interface {
	Resolve(ctx context.Context, source, ref string) (url, commit string, err error)
}

// KeepSkills is the SkillResolver that resolves nothing: the source stays as written and the
// commit empty. It is the default; `factory serve` (RT-S2-T04) wires GitSkills.
type KeepSkills struct{}

// Resolve implements SkillResolver.
func (KeepSkills) Resolve(_ context.Context, source, _ string) (string, string, error) {
	return source, "", nil
}

// GitSkills resolves a skill ref with `git ls-remote` (gitsource.Head), caching each
// (url, ref). A source that isn't a URL is a repo under Base ("ccf-skills" →
// "https://github.com/compliance-framework/ccf-skills"). It is safe for concurrent use.
type GitSkills struct {
	Base string
	// Token, when set, mints a read-only token for a skills repo ("owner/name") on Base's git
	// host, so that a private skills repo resolves: the ls-remote sends it as a header in git's
	// environment (gitsource.HeadAuth), never in the URL or argv. When the mint fails the
	// ls-remote runs anonymously (a public repo still resolves), and a failure then reports
	// both errors. Sources on another host always resolve anonymously. Nil: all anonymous.
	Token func(ctx context.Context, repo string) (string, error)

	// lsRemote is gitsource.HeadAuth; tests replace it.
	lsRemote func(ctx context.Context, url, ref string, auth gitsource.Auth) (string, error)

	mu    sync.Mutex
	cache map[string]string
}

// Resolve implements SkillResolver.
func (g *GitSkills) Resolve(ctx context.Context, source, ref string) (string, string, error) {
	url := source
	if !strings.Contains(source, "://") {
		url = strings.TrimSuffix(g.Base, "/") + "/" + source
	}
	if ref == "" {
		return url, "", nil
	}
	k := url + "@" + ref
	g.mu.Lock()
	commit, ok := g.cache[k]
	g.mu.Unlock()
	if ok {
		return url, commit, nil
	}
	commit, err := g.head(ctx, url, ref)
	if err != nil {
		return "", "", fmt.Errorf("resolve skills %s@%s: %w", gitsource.RedactURL(url), ref, err)
	}
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]string{}
	}
	g.cache[k] = commit
	g.mu.Unlock()
	return url, commit, nil
}

// head is ref's commit in url: with a minted token when url is a repo on Base's git host
// and Token is set, else anonymously.
func (g *GitSkills) head(ctx context.Context, url, ref string) (string, error) {
	ls := g.lsRemote
	if ls == nil {
		ls = gitsource.HeadAuth
	}
	repo, ok := RepoOnHost(url, urlHost(g.Base))
	if g.Token == nil || !ok || gitsource.IsCommit(ref) {
		return ls(ctx, url, ref, gitsource.Auth{})
	}
	tok, mintErr := g.Token(ctx, repo)
	if mintErr == nil && tok == "" {
		mintErr = errors.New("empty token")
	}
	if mintErr != nil {
		commit, err := ls(ctx, url, ref, gitsource.Auth{})
		if err != nil {
			return "", fmt.Errorf("mint a read token for %s: %w; anonymously: %w", repo, mintErr, err)
		}
		return commit, nil
	}
	return ls(ctx, url, ref, gitsource.Auth{Token: tok})
}

// urlHost is raw's host ("github.com"; with its port when it has one), or "".
func urlHost(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// repoPathRE is a repo path, owner/name (an optional ".git" is trimmed first).
var repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// RepoOnHost is the "owner/name" of an http(s) repository URL on host
// ("https://github.com/o/r" or ".../o/r.git" → "o/r"), with ok false for a URL on another
// host, with user info, a query or a fragment, or with a path that isn't exactly a repo.
func RepoOnHost(raw, host string) (repo string, ok bool) {
	u, err := neturl.Parse(raw)
	if err != nil || host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || !strings.EqualFold(u.Host, host) {
		return "", false
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !repoPathRE.MatchString(p) || strings.Contains(p, "..") {
		return "", false
	}
	return p, true
}

// GitHost is the git host of a Provider's repositories, which its tokens are for: for a
// github Provider, the host of spec.github.apiURL (github.com for api.github.com, the GHE
// host otherwise, as the token minter sets it). Other types, and nil, have none ("").
func GitHost(p *v1alpha1.Provider) string {
	if p == nil || p.Spec.Type != v1alpha1.ProviderTypeGitHub {
		return ""
	}
	api := tokens.DefaultAPIURL
	if gh := p.Spec.GitHub; gh != nil && gh.APIURL != "" {
		api = gh.APIURL
	}
	u, err := neturl.Parse(strings.TrimRight(api, "/"))
	if err != nil || u.Host == "" {
		return ""
	}
	return tokens.HostForAPI(u)
}

// objectID is a deterministic id for an idempotency key: "<prefix>_<20 hex>".
func objectID(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + "_" + hex.EncodeToString(sum[:10])
}

// ErrInvalidDispatch is a dispatch that can't become a Task: the rule controller records it on
// the decision (not retried); Recompile returns it for a task the current catalog can't
// compile again.
var ErrInvalidDispatch = errors.New("invalid dispatch")

// ErrNotInCatalog is an ErrInvalidDispatch whose TaskSpec or queue the catalog doesn't have.
var ErrNotInCatalog = errors.New("not in the catalog")

// compileInput is what one Dispatch compiles from.
type compileInput struct {
	cat      *catalog.Catalog
	d        *rules.Dispatch
	wi       *v1alpha1.WorkItem
	ev       v1alpha1.Event
	ruleSet  string
	rule     string
	taskID   string
	deferred string // the budget reason, when the rails deferred the dispatch
	now      time.Time
	// kindOf resolves the kind of an "owner/name#n" context ref that neither the event nor
	// the work item's links name (applier.objectKind). Nil: it is guessed an issue.
	kindOf func(ctx context.Context, provider, ref string) (string, error)
}

// taskBasis is what a Task is built from besides its TaskSpec and queue: what the dispatch
// decided (compileTask) or what the original task kept (Recompile).
type taskBasis struct {
	cat *catalog.Catalog
	ts  *catalog.CompiledTaskSpec
	q   *v1alpha1.Queue
	wi  *v1alpha1.WorkItem
	// taskSpec and queue are the names ts and q have in cat.
	taskSpec, queue string
	// target is the requested target, completed from the work item (taskTarget).
	target *v1alpha1.Target
	// inputs overlay the TaskSpec's defaults.
	inputs map[string]any
	// context is the requested context, resolved for the completed target; the primary link
	// and the workspace repos follow it.
	context func(ctx context.Context, target v1alpha1.Target) ([]v1alpha1.ContextItem, error)
	// harness is the requested harness; nil: the TaskSpec's default.
	harness *v1alpha1.HarnessRequest
	// requires are merged with the TaskSpec's.
	requires []string
	// labels are the task's labels besides LabelTaskSpec.
	labels   map[string]string
	prov     v1alpha1.Provenance // CatalogSHA is set to cat's
	taskID   string
	deferred string
	now      time.Time
}

// lookupSpec is the TaskSpec and the queue a task names in cat.
func lookupSpec(cat *catalog.Catalog, taskSpec, queue string) (*catalog.CompiledTaskSpec, *v1alpha1.Queue, error) {
	ts := cat.TaskSpecs[taskSpec]
	if ts == nil {
		return nil, nil, fmt.Errorf("%w: TaskSpec %q is %w", ErrInvalidDispatch, taskSpec, ErrNotInCatalog)
	}
	q := cat.Queues[queue]
	if q == nil {
		return nil, nil, fmt.Errorf("%w: queue %q is %w", ErrInvalidDispatch, queue, ErrNotInCatalog)
	}
	return ts, q, nil
}

// compileTask compiles a Dispatch into a Pending Task and its prompt (controllers.md §Task
// compilation): inputs merged with the TaskSpec defaults and validated, workspace "=" fields
// evaluated, the prompt rendered from context refs (never inline bodies), skills resolved and
// the credentials request (minted at lease time) filled in.
func compileTask(ctx context.Context, in compileInput, skills SkillResolver) (*v1alpha1.Task, *TaskPrompt, error) {
	ts, q, err := lookupSpec(in.cat, in.d.TaskSpec, in.d.Queue)
	if err != nil {
		return nil, nil, err
	}
	b := taskBasis{
		cat: in.cat, ts: ts, q: q, wi: in.wi, taskSpec: in.d.TaskSpec, queue: in.d.Queue,
		target: in.d.Target, inputs: in.d.Inputs, harness: in.d.Harness,
		prov:   v1alpha1.Provenance{Event: in.ev.ID, RuleSet: in.ruleSet, Rule: in.rule},
		taskID: in.taskID, deferred: in.deferred, now: in.now,
		context: func(ctx context.Context, target v1alpha1.Target) ([]v1alpha1.ContextItem, error) {
			var items []v1alpha1.ContextItem
			for _, c := range in.d.Context {
				it := contextItem(c, in.ev)
				if it.Kind == "ref" && ticketRefRE.MatchString(it.Ref) {
					var err error
					if it, err = ticketItem(ctx, in, target.Provider, it.Ref); err != nil {
						return nil, err // transient (provider): retried
					}
				}
				items = append(items, it)
			}
			return items, nil
		},
	}
	if in.d.Requires != nil {
		b.requires = in.d.Requires.Labels
	}
	return buildTask(ctx, b, skills)
}

// RecompileOptions are what Recompile takes besides the original task.
type RecompileOptions struct {
	// TaskID is the new task's id.
	TaskID string
	// Harness, when set, replaces the harness request.
	Harness *v1alpha1.HarnessRequest
	Now     time.Time
}

// Recompile compiles orig again against cat, as a new Pending task opts.TaskID, with the code
// path compileTask uses (the redispatch command). It keeps what the original dispatch
// decided: the TaskSpec and queue names, the target, the inputs (as merged then, over the
// current defaults), the dispatch's context refs, the requires labels, the labels and the
// rule provenance. It recomputes everything the current TaskSpec gives: the prompt (with wi's
// current summary), skills (resolved again), workspace, context mounts, credentials request,
// effects, limits, output, environment and versions (from the kept inputs); the
// provenance's CatalogSHA is cat's and its Event is left for the caller. The harness request
// is opts.Harness, else the original's while the TaskSpec is unchanged (same digest), else the
// current TaskSpec's default: a stored task can't tell a dispatch's request from the default
// it was given.
//
// A TaskSpec or queue cat no longer has is ErrNotInCatalog; a task the current TaskSpec can't
// express (an input it no longer declares, a prompt that doesn't render) is
// ErrInvalidDispatch; any other error (skills resolution) is transient.
func Recompile(ctx context.Context, cat *catalog.Catalog, skills SkillResolver, wi *v1alpha1.WorkItem, orig *v1alpha1.Task, opts RecompileOptions) (*v1alpha1.Task, *TaskPrompt, error) {
	if skills == nil {
		skills = KeepSkills{}
	}
	ts, q, err := lookupSpec(cat, orig.Spec.TaskSpec.Name, orig.Spec.Queue)
	if err != nil {
		return nil, nil, err
	}
	// The dispatch's context: the original's without the workspace repos (recomputed).
	var kept []v1alpha1.ContextItem
	for _, c := range orig.Spec.Context {
		if c.Mount == "" {
			kept = append(kept, c)
		}
	}
	harness := opts.Harness
	if harness == nil && orig.Spec.TaskSpec.Digest == ts.Digest && orig.Spec.Harness != nil {
		harness = orig.Spec.Harness.Request
	}
	b := taskBasis{
		cat: cat, ts: ts, q: q, wi: wi, taskSpec: orig.Spec.TaskSpec.Name, queue: orig.Spec.Queue,
		target: orig.Spec.Target, inputs: orig.Spec.Inputs, harness: harness,
		labels: orig.Metadata.Labels, taskID: opts.TaskID, now: opts.Now,
		context: func(context.Context, v1alpha1.Target) ([]v1alpha1.ContextItem, error) { return kept, nil },
	}
	if p := orig.Metadata.Provenance; p != nil {
		b.prov.RuleSet, b.prov.Rule = p.RuleSet, p.Rule
	}
	if orig.Spec.Requires != nil {
		b.requires = orig.Spec.Requires.Labels
	}
	return buildTask(ctx, b, skills)
}

// buildTask builds a Pending Task and its prompt from b and its TaskSpec (compileTask's steps).
func buildTask(ctx context.Context, b taskBasis, skills SkillResolver) (*v1alpha1.Task, *TaskPrompt, error) {
	ts := b.ts
	spec := ts.Spec.Spec

	// 1. Inputs: TaskSpec defaults ← dispatch inputs, validated.
	inputs := map[string]any{}
	for k, decl := range spec.Inputs {
		if decl.Default != nil {
			inputs[k] = decl.Default
		}
	}
	maps.Copy(inputs, b.inputs)
	if errs := ts.ValidateInputs(inputs); len(errs) > 0 {
		return nil, nil, fmt.Errorf("%w: inputs: %w", ErrInvalidDispatch, errors.Join(errs...))
	}

	target := taskTarget(b.target, b.wi)

	// 2. Workspace: evaluate the "=" refs with inputs and task.
	act := cel.Activation{
		Inputs: inputs,
		Task:   map[string]any{"id": b.taskID, "target": map[string]any{"repo": target.Repo}},
		Now:    b.now,
	}
	type mount struct{ repo, branch, mode string }
	var mounts []mount
	if ws := spec.Workspace; ws != nil {
		for i, m := range ws.Repos {
			ref := m.Ref
			if _, ok := cel.CELSource(ref); ok {
				p := ts.Expressions[fmt.Sprintf("spec.workspace.repos[%d].ref", i)]
				if p == nil {
					return nil, nil, fmt.Errorf("%w: workspace.repos[%d].ref is not compiled", ErrInvalidDispatch, i)
				}
				v, err := p.EvalValue(ctx, act)
				if err != nil {
					return nil, nil, fmt.Errorf("%w: workspace.repos[%d].ref: %w", ErrInvalidDispatch, i, err)
				}
				s, ok := v.(string)
				if !ok {
					return nil, nil, fmt.Errorf("%w: workspace.repos[%d].ref is %T, want a string", ErrInvalidDispatch, i, v)
				}
				ref = s
			} else {
				ref = cel.Unescape(ref)
			}
			repo, branch, _ := strings.Cut(ref, "@")
			mode := m.Mode
			if mode == "" {
				mode = "ro"
			}
			mounts = append(mounts, mount{repo, branch, mode})
		}
	}

	// 3. Context refs: the agent fetches them; bodies are never inlined.
	var ctxItems []v1alpha1.ContextItem
	seen := map[string]bool{}
	add := func(c v1alpha1.ContextItem) {
		k := c.Kind + "|" + c.Provider + "|" + c.Ref
		if c.Ref == "" || seen[k] {
			return
		}
		seen[k] = true
		ctxItems = append(ctxItems, c)
	}
	requested, err := b.context(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range requested {
		add(c)
	}
	if l, ok := primaryLink(b.wi.Spec.Links); ok {
		add(v1alpha1.ContextItem{Kind: l.Kind, Provider: l.Provider, Ref: l.Ref, Trust: TrustUntrusted})
	}
	for _, m := range mounts {
		ref := m.repo
		if m.branch != "" {
			ref += "@" + m.branch
		}
		add(v1alpha1.ContextItem{Kind: "repo", Provider: target.Provider, Ref: ref, Mount: "/work/" + path.Base(m.repo)})
	}

	// 4. Prompt.
	prompt := &TaskPrompt{TypeMeta: v1alpha1.TypeMeta{Kind: KindTaskPrompt}, Metadata: v1alpha1.ObjectMeta{Name: b.taskID}}
	if ts.Prompt != nil && ts.Prompt.Template != nil {
		var buf bytes.Buffer
		data := map[string]any{
			"task": map[string]any{
				"id": b.taskID, "spec": b.taskSpec,
				"target": map[string]any{"provider": target.Provider, "repo": target.Repo, "ref": target.Ref},
			},
			"inputs":  inputs,
			"item":    itemSummary(b.wi),
			"context": jsonValue(ctxItems),
		}
		if err := ts.Prompt.Template.Execute(&buf, data); err != nil {
			return nil, nil, fmt.Errorf("%w: render prompt %s: %w", ErrInvalidDispatch, ts.Prompt.Path, err)
		}
		prompt.Text = buf.String()
	}
	sum := sha256.Sum256([]byte(prompt.Text))
	prompt.Digest = "sha256:" + hex.EncodeToString(sum[:])

	// 5. Skills: ref tag → commit.
	var skillRefs []v1alpha1.SkillRef
	for _, s := range spec.Skills {
		url, commit, err := skills.Resolve(ctx, s.Source, s.Ref)
		if err != nil {
			return nil, nil, err // transient (git): retried
		}
		if s.Commit != "" {
			commit = s.Commit
		}
		skillRefs = append(skillRefs, v1alpha1.SkillRef{Source: url, Ref: s.Ref, Commit: commit, Use: slices.Clone(s.Use)})
	}

	// 6. Credentials request: write on at most one repo, read on the others.
	cred := &v1alpha1.GitCredentialSpec{Provider: target.Provider, Identity: spec.Identity, Permissions: maps.Clone(spec.Permissions)}
	for _, m := range mounts {
		if m.mode == "rw" && cred.Repo == "" {
			cred.Repo = m.repo
		}
	}
	if cred.Repo == "" {
		cred.Repo = target.Repo
	}
	for _, m := range mounts {
		if m.repo != cred.Repo && !slices.Contains(cred.ReadRepos, m.repo) {
			cred.ReadRepos = append(cred.ReadRepos, m.repo)
		}
	}
	if cred.Repo == "" && len(cred.ReadRepos) > 0 {
		cred.Repo, cred.ReadRepos = cred.ReadRepos[0], cred.ReadRepos[1:]
	}
	// Skills repos on the task's git host are read repos too, so a private one gets a
	// read-only token that the agent's credential helper answers its skills fetch with. They
	// are added after the task repo is chosen: a skills repo never gets the TaskSpec's
	// permissions. Skills the agent doesn't fetch (no use) and sources the resolver left as
	// written (not a URL) are skipped.
	cred.ReadRepos = appendSkillRepos(cred.ReadRepos, cred.Repo, GitHost(b.cat.Providers[target.Provider]), skillRefs)
	// Public read repos are cloned anonymously; the others get read-only tokens at lease time.
	// With publicReads the skills repos get none either: they must be public too.
	if ws := spec.Workspace; ws != nil && ws.PublicReads != nil {
		cred.PublicReads = *ws.PublicReads
	}
	ttl := MaxCredentialTTL
	if spec.Limits != nil && spec.Limits.MaxWallClock != "" {
		if d, err := spec.Limits.MaxWallClock.Parse(); err == nil && d > 0 {
			ttl = min(ttl, d+credentialSlack)
		}
	}
	cred.TTL = v1alpha1.Duration(ttl.String())

	// Harness and requires: the dispatch's request, else the TaskSpec default.
	var harness *v1alpha1.TaskHarness
	switch {
	case b.harness != nil:
		h := *b.harness
		harness = &v1alpha1.TaskHarness{Request: &h}
	case spec.Harness != nil && spec.Harness.Default != nil:
		h := *spec.Harness.Default
		harness = &v1alpha1.TaskHarness{Request: &h}
	}
	var requires []string
	if spec.Requires != nil {
		requires = append(requires, spec.Requires.Labels...)
	}
	requires = append(requires, b.requires...)
	slices.Sort(requires)
	requires = slices.Compact(requires)

	labels := maps.Clone(b.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelTaskSpec] = b.taskSpec
	prov := b.prov
	prov.CatalogSHA = b.cat.SHA

	task := &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{
			ID:         b.taskID,
			WorkItem:   b.wi.Metadata.Name,
			Provenance: &prov,
			Labels:     labels,
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec:    v1alpha1.TaskSpecRef{Name: b.taskSpec, Digest: ts.Digest},
			Queue:       b.queue,
			Pool:        b.q.Spec.Pool,
			Provider:    target.Provider,
			Target:      &target,
			Inputs:      inputs,
			Harness:     harness,
			Skills:      skillRefs,
			Prompt:      &v1alpha1.PromptRef{Ref: "api://tasks/" + b.taskID + "/prompt", Digest: prompt.Digest},
			Context:     ctxItems,
			Credentials: &v1alpha1.TaskCredentials{Git: cred, LLM: LLMAgentLocal},
			Effects:     cloneEffects(spec.Effects),
			Limits:      cloneLimits(spec.Limits),
			Output:      cloneOutput(spec.Output),
		},
		Status: &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending, Reason: b.deferred},
	}
	if len(requires) > 0 {
		task.Spec.Requires = &v1alpha1.Requires{Labels: requires}
	}
	if ws := spec.Workspace; ws != nil && ws.Environment != nil {
		task.Spec.Environment = cloneEnvironment(ws.Environment)
		versions, err := resolveVersions(ws.Environment.Versions, inputs)
		if err != nil {
			return nil, nil, err
		}
		task.Spec.Versions = versions
	}
	return task, prompt, nil
}

// resolveVersions is a Task's spec.versions from its environment's versions source. For
// from: inputs it is the input src.Input names (merged with its default): a map of component
// to version (map[string]any with string values, as inputs come from JSON and CEL) that
// passes the Task's versions checks, else ErrInvalidDispatch. An absent or empty input is no
// versions.
func resolveVersions(src *v1alpha1.VersionSource, inputs map[string]any) (map[string]string, error) {
	if src == nil || src.From != "inputs" {
		// from: latest and from: train aren't resolved yet: no versions, so the environment's
		// own defaults apply.
		return nil, nil
	}
	field := "inputs." + src.Input
	raw := inputs[src.Input]
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s is %T, want a map of component to version", ErrInvalidDispatch, field, raw)
	}
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v, ok := m[k].(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s.%s is %T, want a string", ErrInvalidDispatch, field, k, m[k])
		}
		out[k] = v
	}
	if err := (&v1alpha1.Task{Spec: v1alpha1.TaskRunSpec{Versions: out}}).Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidDispatch, field, err)
	}
	return out, nil
}

// cloneEnvironment is a deep copy of e (non-nil).
func cloneEnvironment(e *v1alpha1.Environment) *v1alpha1.Environment {
	c := *e
	if e.Versions != nil {
		v := *e.Versions
		c.Versions = &v
	}
	if e.Ready != nil {
		r := *e.Ready
		r.HTTP = slices.Clone(e.Ready.HTTP)
		c.Ready = &r
	}
	c.Exposes = maps.Clone(e.Exposes)
	if e.Egress != nil {
		g := *e.Egress
		g.Allow = slices.Clone(e.Egress.Allow)
		c.Egress = &g
	}
	return &c
}

// appendSkillRepos adds to readRepos the "owner/name" of each fetched skills source on host
// (RepoOnHost) that is neither the task repo nor already there (case-insensitively, as the
// agent's credential helper matches them).
func appendSkillRepos(readRepos []string, taskRepo, host string, skills []v1alpha1.SkillRef) []string {
	if host == "" {
		return readRepos
	}
	for _, s := range skills {
		if len(s.Use) == 0 {
			continue
		}
		repo, ok := RepoOnHost(s.Source, host)
		if !ok || strings.EqualFold(repo, taskRepo) ||
			slices.ContainsFunc(readRepos, func(r string) bool { return strings.EqualFold(r, repo) }) {
			continue
		}
		readRepos = append(readRepos, repo)
	}
	return readRepos
}

// taskTarget is the dispatch target, completed from the work item's primary link.
func taskTarget(t *v1alpha1.Target, wi *v1alpha1.WorkItem) v1alpha1.Target {
	var out v1alpha1.Target
	if t != nil {
		out = *t
	}
	if l, ok := primaryLink(wi.Spec.Links); ok {
		if out.Provider == "" {
			out.Provider = l.Provider
		}
		if out.Ref == "" && out.Repo == "" {
			out.Ref = l.Ref
		}
	}
	if out.Repo == "" {
		out.Repo, _, _ = strings.Cut(out.Ref, "#")
	}
	return out
}

// contextItem maps a dispatch context entry ("event.object", "event.review", a ref) to a
// context ref. A plain ref is kind "ref"; compileTask makes an owner/name#n one a ticket
// (ticketItem).
func contextItem(c string, ev v1alpha1.Event) v1alpha1.ContextItem {
	if c == "event.object" {
		if o := ev.Data.Object; o != nil {
			p := o.Provider
			if p == "" {
				p = ev.Source
			}
			return v1alpha1.ContextItem{Kind: o.Kind, Provider: p, Ref: o.Ref, Trust: TrustUntrusted}
		}
		return v1alpha1.ContextItem{}
	}
	if fact, ok := strings.CutPrefix(c, "event."); ok {
		switch v := ev.Data.Facts[fact].(type) {
		case map[string]any:
			if ref, _ := v["ref"].(string); ref != "" {
				return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: ref, Trust: TrustUntrusted}
			}
			return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: ev.ID + "#" + fact, Trust: TrustUntrusted}
		case string:
			return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: v, Trust: TrustUntrusted}
		}
		if ev.Data.ContentRef != "" {
			return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: ev.Data.ContentRef, Trust: TrustUntrusted}
		}
		return v1alpha1.ContextItem{}
	}
	return v1alpha1.ContextItem{Kind: "ref", Ref: c, Trust: TrustUntrusted}
}

// ticketRefRE is an issue or PR ref, owner/name#n (the form of the agent's tickets/ files).
var ticketRefRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[0-9]+$`)

// ticketItem is the context item of an "owner/name#n" dispatch context entry: an issue or a PR
// on the dispatch target's provider, so the agent writes it to tickets/. Its kind is the
// event object's or a link's when one of them is that ref, else in.kindOf's; the server reads
// content by kind (another kind is not found), so it is resolved rather than guessed.
func ticketItem(ctx context.Context, in compileInput, prov, ref string) (v1alpha1.ContextItem, error) {
	kind := ""
	if o := in.ev.Data.Object; o != nil && o.Ref == ref && isTicketKind(o.Kind) {
		p := o.Provider
		if p == "" {
			p = in.ev.Source
		}
		if p == prov {
			kind = o.Kind
		}
	}
	for _, l := range in.wi.Spec.Links {
		if kind == "" && l.Provider == prov && l.Ref == ref && isTicketKind(l.Kind) {
			kind = l.Kind
		}
	}
	if kind == "" {
		kind = provider.KindIssue
		if in.kindOf != nil {
			k, err := in.kindOf(ctx, prov, ref)
			if err != nil {
				return v1alpha1.ContextItem{}, err
			}
			kind = k
		}
	}
	return v1alpha1.ContextItem{Kind: kind, Provider: prov, Ref: ref, Trust: TrustUntrusted}, nil
}

func isTicketKind(k string) bool { return k == provider.KindIssue || k == provider.KindPR }

// itemSummary is the work item as prompts see it: id, state and links (no bodies).
func itemSummary(wi *v1alpha1.WorkItem) map[string]any {
	m := map[string]any{"id": wi.Metadata.Name, "links": jsonValue(wi.Spec.Links), "state": map[string]any{}}
	if wi.Status != nil && wi.Status.State != nil {
		m["state"] = jsonValue(wi.Status.State)
	}
	return m
}

func cloneEffects(e *v1alpha1.EffectsPolicy) *v1alpha1.EffectsPolicy {
	if e == nil {
		return nil
	}
	return &v1alpha1.EffectsPolicy{Allow: slices.Clone(e.Allow), Labels: slices.Clone(e.Labels)}
}

func cloneLimits(l *v1alpha1.Limits) *v1alpha1.Limits {
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

func cloneOutput(o *v1alpha1.OutputSpec) *v1alpha1.OutputSpec {
	if o == nil {
		return nil
	}
	return &v1alpha1.OutputSpec{Schema: o.Schema, Outcomes: slices.Clone(o.Outcomes)}
}

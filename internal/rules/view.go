package rules

import (
	"strconv"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/cel"
)

// WorkItemView is the read-only snapshot of a work item that the runtime builds (store plus
// the latest provider observation) before Evaluate. The engine never modifies it.
type WorkItemView struct {
	ID    string          `json:"id"`
	Links []v1alpha1.Link `json:"links,omitzero"`
	State map[string]any  `json:"state,omitzero"`
	// Counters are runs per TaskSpec as stored (e.g. rebuilt from the marker). item.count
	// reads the larger of Counters[spec] and len(RunsBySpec[spec]).
	Counters map[string]int  `json:"counters,omitzero"`
	Reporter *v1alpha1.Actor `json:"reporter,omitzero"`
	// PR is the primary PR observation, if any; PRs are every linked PR (stacks).
	PR  *PRView  `json:"pr,omitzero"`
	PRs []PRView `json:"prs,omitzero"`
	// Deploy is the latest deployment observation (item.deploy), if any.
	Deploy *DeployView `json:"deploy,omitzero"`
	// RunsBySpec are the work item's Run summaries by TaskSpec, oldest first.
	RunsBySpec map[string][]RunSummary `json:"runsBySpec,omitzero"`
}

// PRView is a PR observation (item.pr, item.prs[]).
type PRView struct {
	Provider          string       `json:"provider,omitzero"`
	Ref               string       `json:"ref,omitzero"`
	Author            PRAuthor     `json:"author"`
	Draft             bool         `json:"draft"`
	Head              string       `json:"head,omitzero"`
	Labels            []string     `json:"labels,omitzero"`
	Checks            PRChecks     `json:"checks"`
	ApprovedBy        []PRApproval `json:"approvedBy,omitzero"`
	UnresolvedThreads int          `json:"unresolvedThreads,omitzero"`
}

// PRAuthor is who opened the PR.
type PRAuthor struct {
	IsFactory bool `json:"isFactory"`
}

// PRChecks is the rollup of the PR head's checks.
type PRChecks struct {
	State   string `json:"state,omitzero"`
	Settled bool   `json:"settled,omitzero"`
}

// PRApproval is an approving review: by which factory identity (or login), at which commit.
type PRApproval struct {
	Identity string `json:"identity"`
	Commit   string `json:"commit"`
}

// DeployView is a deployment observation (item.deploy).
type DeployView struct {
	State         string `json:"state,omitzero"`
	SHA           string `json:"sha,omitzero"`
	URL           string `json:"url,omitzero"`
	ContainsMerge bool   `json:"containsMerge,omitzero"`
}

// RunSummary is one Run as rules see it (run, item.runs[spec][], item.lastRun(spec)).
type RunSummary struct {
	ID      string         `json:"id"`
	Spec    string         `json:"spec"`
	Outcome string         `json:"outcome,omitzero"`
	Status  string         `json:"status,omitzero"`
	Inputs  map[string]any `json:"inputs,omitzero"`
	Result  map[string]any `json:"result,omitzero"`
	Agent   string         `json:"agent,omitzero"`
	Harness *RunHarness    `json:"harness,omitzero"`
}

// RunHarness is the harness a run used.
type RunHarness struct {
	Name  string `json:"name"`
	Model string `json:"model,omitzero"`
}

// Fact keys of factory.* events that carry the run and the approval.
const (
	factRun      = "run"
	factApproval = "approval"
)

// base is the part of the activation that doesn't depend on the RuleSet or the state.
type base struct {
	event, actor, run, approval, provider map[string]any
	item                                  map[string]any // without "state"
	now                                   time.Time
}

func newBase(c *catalog.Catalog, ev v1alpha1.Event, wi WorkItemView, now time.Time) base {
	b := base{
		event:    eventMap(ev),
		actor:    actorMap(ev.Data.Actor),
		provider: providerMap(c, ev.Source),
		item:     itemMap(wi),
		now:      now,
	}
	if ev.Type.IsFactory() {
		b.run, _ = ev.Data.Facts[factRun].(map[string]any)
		b.approval, _ = ev.Data.Facts[factApproval].(map[string]any)
	}
	return b
}

// activation is the base with the RuleSet's params and the given state.
func (b base) activation(params, state map[string]any) cel.Activation {
	item := make(map[string]any, len(b.item)+1)
	for k, v := range b.item {
		item[k] = v
	}
	item["state"] = state
	return cel.Activation{
		Event: b.event, Actor: b.actor, Item: item, Run: b.run, Approval: b.approval,
		Provider: b.provider, Params: params, Now: b.now,
	}
}

// eventMap is the event variable: the facts, then the envelope (which facts can't shadow).
func eventMap(ev v1alpha1.Event) map[string]any {
	m := make(map[string]any, len(ev.Data.Facts)+8)
	for k, v := range ev.Data.Facts {
		m[k] = v
	}
	m["id"] = ev.ID
	m["type"] = string(ev.Type)
	m["source"] = ev.Source
	m["subject"] = ev.Subject
	m["time"] = ev.Time
	m["repo"] = eventRepo(ev)
	if o := ev.Data.Object; o != nil {
		obj := map[string]any{"provider": o.Provider, "kind": o.Kind, "ref": o.Ref}
		if n, ok := refNumber(o.Ref); ok {
			obj["number"] = n
		}
		m["object"] = obj
	}
	return m
}

func actorMap(a *v1alpha1.Actor) map[string]any {
	if a == nil {
		a = &v1alpha1.Actor{}
	}
	return map[string]any{"id": a.ID, "tier": a.Tier, "isBot": a.IsBot, "isFactory": a.IsFactory, "identity": a.Identity}
}

func providerMap(c *catalog.Catalog, name string) map[string]any {
	if name == v1alpha1.FactoryProvider {
		return map[string]any{"name": name, "type": string(v1alpha1.ProviderTypeFactory), "labels": map[string]any{}}
	}
	p := c.Providers[name]
	if p == nil {
		return map[string]any{"name": name, "labels": map[string]any{}}
	}
	labels := make(map[string]any, len(p.Metadata.Labels))
	for k, v := range p.Metadata.Labels {
		labels[k] = v
	}
	return map[string]any{"name": name, "type": string(p.Spec.Type), "labels": labels}
}

func itemMap(wi WorkItemView) map[string]any {
	counters := make(map[string]any, len(wi.Counters)+len(wi.RunsBySpec))
	for k, n := range wi.Counters {
		counters[k] = int64(n)
	}
	runs := make(map[string]any, len(wi.RunsBySpec))
	for spec, rs := range wi.RunsBySpec {
		list := make([]any, len(rs))
		for i, r := range rs {
			list[i] = r.toMap()
		}
		runs[spec] = list
		if n, _ := counters[spec].(int64); int64(len(rs)) > n {
			counters[spec] = int64(len(rs))
		}
	}
	links := make([]any, len(wi.Links))
	for i, l := range wi.Links {
		links[i] = map[string]any{"provider": l.Provider, "ref": l.Ref, "kind": l.Kind, "primary": l.Primary}
	}
	prs := make([]any, len(wi.PRs))
	for i, p := range wi.PRs {
		prs[i] = p.toMap()
	}
	m := map[string]any{"id": wi.ID, "counters": counters, "runs": runs, "links": links, "prs": prs}
	if r := wi.Reporter; r != nil {
		m["reporter"] = map[string]any{"id": r.ID, "tier": r.Tier}
	}
	if wi.PR != nil {
		m["pr"] = wi.PR.toMap()
	}
	if d := wi.Deploy; d != nil {
		m["deploy"] = map[string]any{"state": d.State, "sha": d.SHA, "url": d.URL, "containsMerge": d.ContainsMerge}
	}
	return m
}

func (p PRView) toMap() map[string]any {
	labels := make([]any, len(p.Labels))
	for i, l := range p.Labels {
		labels[i] = l
	}
	approved := make([]any, len(p.ApprovedBy))
	for i, a := range p.ApprovedBy {
		approved[i] = map[string]any{"identity": a.Identity, "commit": a.Commit}
	}
	return map[string]any{
		"provider": p.Provider, "ref": p.Ref,
		"author": map[string]any{"isFactory": p.Author.IsFactory},
		"draft":  p.Draft, "head": p.Head, "labels": labels,
		"checks":     map[string]any{"state": p.Checks.State, "settled": p.Checks.Settled},
		"approvedBy": approved, "unresolvedThreads": int64(p.UnresolvedThreads),
	}
}

func (r RunSummary) toMap() map[string]any {
	m := map[string]any{
		"id": r.ID, "spec": r.Spec, "outcome": r.Outcome, "status": r.Status, "agent": r.Agent,
		"inputs": orEmpty(r.Inputs), "result": orEmpty(r.Result),
	}
	if h := r.Harness; h != nil {
		m["harness"] = map[string]any{"name": h.Name, "model": h.Model}
	}
	return m
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// eventRepo is data.repo, or the repo of the object ref ("owner/name#42" → "owner/name").
func eventRepo(ev v1alpha1.Event) string {
	if ev.Data.Repo != "" {
		return ev.Data.Repo
	}
	if o := ev.Data.Object; o != nil {
		return refRepo(o.Ref)
	}
	return ""
}

// refRepo is the "owner/name" part of a GitHub-style ref, or "" when ref has none.
func refRepo(ref string) string {
	repo, _, _ := strings.Cut(ref, "#")
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return ""
	}
	return repo
}

// refNumber is the number after "#" in a ref.
func refNumber(ref string) (int64, bool) {
	_, num, ok := strings.Cut(ref, "#")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(num, 10, 64)
	return n, err == nil
}

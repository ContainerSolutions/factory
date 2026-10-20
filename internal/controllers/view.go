package controllers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// Task and run failure reasons (Task.status.reason).
const (
	ReasonBudget        = "budget"
	ReasonInvalidResult = "invalid-result"
	ReasonTimeout       = "timeout"
	ReasonThrottled     = "agent-throttled"
	ReasonCancelled     = "cancelled"
)

func isNotFound(err error) bool { return errors.Is(err, provider.ErrNotFound) }

// isForbidden reports a provider refusing access: permanent until its installation changes.
// A rate limit (a 403 GitHub sends with rate-limit headers, say) is retryable, never this.
func isForbidden(err error) bool {
	return errors.Is(err, provider.ErrForbidden) && !errors.Is(err, provider.ErrRetryable)
}

// counted reports whether a run counts as one of the work item's runs (item.count, RunsBySpec).
func counted(r *v1alpha1.Run) bool {
	if r.Status == nil {
		return false
	}
	switch r.Status.End {
	case v1alpha1.RunCompleted, v1alpha1.RunFailed, v1alpha1.RunCancelled:
		return true
	}
	return false
}

// ended reports whether a run has ended (Run.status.end is set; an open run has none).
func ended(r *v1alpha1.Run) bool { return r.Status != nil && r.Status.End != "" }

// primaryLink is the link marked primary, else the first one.
func primaryLink(links []v1alpha1.Link) (v1alpha1.Link, bool) {
	for _, l := range links {
		if l.Primary {
			return l, true
		}
	}
	if len(links) > 0 {
		return links[0], true
	}
	return v1alpha1.Link{}, false
}

// activePhase reports whether a task is still to run or running.
func activePhase(p v1alpha1.TaskPhase) bool {
	switch p {
	case v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
		return true
	}
	return false
}

func taskPhase(t *v1alpha1.Task) v1alpha1.TaskPhase {
	if t.Status == nil {
		return v1alpha1.TaskPending
	}
	return t.Status.Phase
}

// listTasks returns the work item's Tasks.
func listTasks(ctx context.Context, s store.Store, workItem string) ([]*v1alpha1.Task, error) {
	var tasks []*v1alpha1.Task
	if _, err := s.List(ctx, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskWorkItem: workItem}}, &tasks); err != nil {
		return nil, fmt.Errorf("list tasks of %s: %w", workItem, err)
	}
	return tasks, nil
}

// listRuns returns a task's Runs, oldest first.
func listRuns(ctx context.Context, s store.Store, task string) ([]*v1alpha1.Run, error) {
	var runs []*v1alpha1.Run
	if _, err := s.List(ctx, v1alpha1.KindRun, store.ListOptions{Field: map[string]string{store.FieldRunTask: task}}, &runs); err != nil {
		return nil, fmt.Errorf("list runs of %s: %w", task, err)
	}
	slices.SortStableFunc(runs, func(a, b *v1alpha1.Run) int {
		return cmp.Or(a.Metadata.CreatedAt.Compare(b.Metadata.CreatedAt), cmp.Compare(a.Metadata.Name, b.Metadata.Name))
	})
	return runs, nil
}

// runsBySpec returns the work item's counted runs as rules see them, by TaskSpec, oldest first.
func runsBySpec(ctx context.Context, s store.Store, tasks []*v1alpha1.Task) (map[string][]rules.RunSummary, error) {
	type item struct {
		run *v1alpha1.Run
		sum rules.RunSummary
	}
	by := map[string][]item{}
	for _, t := range tasks {
		runs, err := listRuns(ctx, s, t.Metadata.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			if counted(r) {
				by[t.Spec.TaskSpec.Name] = append(by[t.Spec.TaskSpec.Name], item{r, runSummary(t, r)})
			}
		}
	}
	out := make(map[string][]rules.RunSummary, len(by))
	for spec, items := range by {
		slices.SortStableFunc(items, func(a, b item) int {
			return cmp.Or(a.run.Metadata.CreatedAt.Compare(b.run.Metadata.CreatedAt), cmp.Compare(a.run.Metadata.Name, b.run.Metadata.Name))
		})
		for _, it := range items {
			out[spec] = append(out[spec], it.sum)
		}
	}
	return out, nil
}

// runSummary is one run as rules see it (run, item.runs[spec][]).
func runSummary(t *v1alpha1.Task, r *v1alpha1.Run) rules.RunSummary {
	sum := rules.RunSummary{
		ID:     r.Metadata.Name,
		Spec:   t.Spec.TaskSpec.Name,
		Inputs: maps.Clone(t.Spec.Inputs),
		Agent:  r.Spec.Agent,
	}
	var end v1alpha1.RunEnd
	if r.Status != nil {
		end = r.Status.End
	}
	switch end {
	case v1alpha1.RunCancelled:
		sum.Status = "cancelled"
	case v1alpha1.RunFailed:
		sum.Status = string(v1alpha1.ResultFailed)
	}
	if r.Status != nil && r.Status.Result != nil {
		res := r.Status.Result
		sum.Outcome = res.Outcome
		if sum.Status == "" {
			sum.Status = string(res.Status)
		}
		sum.Result = resultMap(res)
		if h := res.Harness; h != nil {
			sum.Harness = &rules.RunHarness{Name: h.Name, Model: h.EffectiveConfig.Model}
		}
	}
	if sum.Harness == nil && t.Spec.Harness != nil && t.Spec.Harness.Resolved != nil {
		sum.Harness = &rules.RunHarness{Name: t.Spec.Harness.Resolved.Name, Model: t.Spec.Harness.Resolved.Model}
	}
	return sum
}

// resultMap is a Result as rules see it (run.result): its JSON fields, with the fields of
// data also at the top level (run.result.plan for data.plan) unless a base field has the
// name.
func resultMap(res *v1alpha1.Result) map[string]any {
	b, err := json.Marshal(res)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	if data, ok := m["data"].(map[string]any); ok {
		for k, v := range data {
			if _, taken := m[k]; !taken {
				m[k] = v
			}
		}
	}
	return m
}

// runFacts are the facts of factory.run.completed/failed: {id, spec, outcome, status, inputs,
// result, agent, harness}, plus reason when the run failed for one.
func runFacts(sum rules.RunSummary, reason string) map[string]any {
	run := map[string]any{
		"id": sum.ID, "spec": sum.Spec, "outcome": sum.Outcome, "status": sum.Status,
		"inputs": orEmpty(sum.Inputs), "result": orEmpty(sum.Result), "agent": sum.Agent,
	}
	if h := sum.Harness; h != nil {
		run["harness"] = map[string]any{"name": h.Name, "model": h.Model}
	} else {
		run["harness"] = map[string]any{}
	}
	if reason != "" {
		run["reason"] = reason
	}
	// A JSON round trip makes every value a plain JSON value (numbers float64), as stored.
	return map[string]any{"run": jsonValue(run)}
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// jsonValue returns v after a JSON round trip (nil when it doesn't encode).
func jsonValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// observations are a work item's links observed (cache) and the ones that failed.
type observations struct {
	byLink    map[string]provider.Observation // by cacheKey; objects that exist
	at        map[string]time.Time            // by cacheKey: when each was observed
	gone      map[string]bool                 // objects the provider doesn't have, or won't show
	forbidden map[string]error                // by cacheKey: the links the provider won't show (also gone)
}

// forbiddenErr is the first forbidden link's error, in link order; nil when there is none.
func (o observations) forbiddenErr(links []v1alpha1.Link) error {
	for _, l := range links {
		if err := o.forbidden[cacheKey(l)]; err != nil {
			return err
		}
	}
	return nil
}

// observeLinks observes every link through the cache. A transient error fails the whole view
// (the reconcile is retried). A link the provider won't show (forbidden) is evaluated as gone
// and recorded in forbidden, for the caller to report.
func observeLinks(ctx context.Context, cache *ObservationCache, links []v1alpha1.Link) (observations, error) {
	out := observations{byLink: map[string]provider.Observation{}, at: map[string]time.Time{}, gone: map[string]bool{}, forbidden: map[string]error{}}
	for _, l := range links {
		obs, at, err := cache.observe(ctx, l)
		switch {
		case isNotFound(err):
			out.gone[cacheKey(l)] = true
		case isForbidden(err):
			out.gone[cacheKey(l)] = true
			out.forbidden[cacheKey(l)] = fmt.Errorf("observe %s/%s: %w", l.Provider, l.Ref, err)
		case err != nil:
			return observations{}, fmt.Errorf("observe %s/%s: %w", l.Provider, l.Ref, err)
		default:
			out.byLink[cacheKey(l)] = obs
			out.at[cacheKey(l)] = at
		}
	}
	return out, nil
}

// buildView is the WorkItemView of wi: the store (state, counters, reporter, runs) plus the
// observations of its links (item.pr, item.prs, item.deploy).
func buildView(wi *v1alpha1.WorkItem, runs map[string][]rules.RunSummary, obs observations) rules.WorkItemView {
	v := rules.WorkItemView{ID: wi.Metadata.Name, Links: slices.Clone(wi.Spec.Links), RunsBySpec: runs}
	if st := wi.Status; st != nil {
		v.State = maps.Clone(st.State)
		v.Counters = maps.Clone(st.Counters)
		if st.Reporter != nil {
			r := *st.Reporter
			v.Reporter = &r
		}
	}
	primary, _ := primaryLink(wi.Spec.Links)
	var lastPR *rules.PRView
	for _, l := range wi.Spec.Links {
		o, ok := obs.byLink[cacheKey(l)]
		if !ok {
			continue
		}
		if o.PR != nil {
			pv := prView(l, o)
			v.PRs = append(v.PRs, pv)
			if l == primary {
				p := pv
				v.PR = &p
			}
			lastPR = &pv
		}
	}
	if v.PR == nil && lastPR != nil {
		v.PR = lastPR
	}
	v.Deploy = deployView(wi.Spec.Links, obs)
	return v
}

// deployView is item.deploy over the work item's merged PRs, since a stack merges several and
// the provider reports the deployment per PR (containsMerge: the latest deployment contains
// that PR's merge commit). containsMerge holds only when every merged PR has a deployment
// containing its merge; state, sha and url are the most recently observed deployment's (the
// first such link on a tie). Unmerged PRs and other links are ignored; with no merged PR that
// has a deployment, there is no item.deploy.
func deployView(links []v1alpha1.Link, obs observations) *rules.DeployView {
	var (
		out    *rules.DeployView
		latest time.Time
		all    = true
	)
	for _, l := range links {
		o, ok := obs.byLink[cacheKey(l)]
		if !ok || !merged(o) {
			continue
		}
		d := o.Deploy
		if d == nil {
			all = false // merged, not deployed yet
			continue
		}
		all = all && d.ContainsMerge
		if at := obs.at[cacheKey(l)]; out == nil || at.After(latest) {
			out, latest = &rules.DeployView{State: d.State, SHA: d.SHA, URL: d.URL}, at
		}
	}
	if out != nil {
		out.ContainsMerge = all
	}
	return out
}

// merged reports whether an observation is a merged PR.
func merged(o provider.Observation) bool {
	return o.PR != nil && (o.State == provider.StateMerged || !o.PR.MergedAt.IsZero())
}

func prView(l v1alpha1.Link, o provider.Observation) rules.PRView {
	pv := rules.PRView{
		Provider: l.Provider, Ref: l.Ref,
		Draft: o.PR.Draft, Head: o.PR.Head, Labels: slices.Clone(o.Labels),
		Checks:            rules.PRChecks{State: o.PR.Checks.State, Settled: o.PR.Checks.Settled},
		UnresolvedThreads: o.PR.UnresolvedThreads,
	}
	if o.Author != nil {
		pv.Author.IsFactory = o.Author.IsFactory
	}
	for _, a := range o.PR.ApprovedBy {
		pv.ApprovedBy = append(pv.ApprovedBy, rules.PRApproval{Identity: a.Identity, Commit: a.Commit})
	}
	return pv
}

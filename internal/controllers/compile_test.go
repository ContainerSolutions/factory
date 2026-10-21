package controllers

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/rules"
)

// withHosts is the PoC catalog with docs-review's workspace listing hosts (the TaskSpec and
// the catalog are copied: the loaded ones are shared).
func withHosts(c *catalog.Catalog, hosts []string) *catalog.Catalog {
	ts := *c.TaskSpecs["docs-review"]
	spec := *ts.Spec
	ws := v1alpha1.Workspace{}
	if spec.Spec.Workspace != nil {
		ws = *spec.Spec.Workspace
	}
	ws.Hosts = hosts
	spec.Spec.Workspace = &ws
	ts.Spec = &spec
	out := *c
	out.TaskSpecs = maps.Clone(c.TaskSpecs)
	out.TaskSpecs["docs-review"] = &ts
	return &out
}

func TestCompileHosts(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	for _, tc := range []struct {
		name  string
		hosts []string
		want  []string
	}{
		{name: "hosts", hosts: []string{"registry.npmjs.org", "*.yarnpkg.com"}, want: []string{"registry.npmjs.org", "*.yarnpkg.com"}},
		{name: "none"},
		{name: "empty", hosts: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := withHosts(e.cat, tc.hosts)
			task, _, err := compileTask(e.ctx, compileInput{
				cat: cat, wi: wi, ev: v1alpha1.Event{ID: "ev"}, taskID: "tsk_h", now: e.clk.Now(),
				d: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc"},
			}, KeepSkills{})
			if err != nil {
				t.Fatal(err)
			}
			if got := task.Spec.Hosts; !slices.Equal(got, tc.want) || (tc.want == nil && got != nil) {
				t.Fatalf("hosts = %#v, want %#v", got, tc.want)
			}
			if len(tc.hosts) > 0 {
				task.Spec.Hosts[0] = "changed.example.com"
				if tc.hosts[0] == "changed.example.com" {
					t.Error("the task's hosts alias the TaskSpec's")
				}
			}
		})
	}
}

func TestCompileTicketContext(t *testing.T) {
	const (
		sub   = docsRepo + "#43"
		pr    = docsRepo + "#50"
		gone  = docsRepo + "#99"
		other = "other-provider"
	)
	e := newEnv(t)
	wi, _ := e.startPlan() // primary link: docs#42, an issue
	e.seed(provider.KindIssue, sub, nil)
	e.seed(provider.KindPR, pr, nil)
	prEvent := v1alpha1.Event{ID: "ev", Source: ghProvider, Data: v1alpha1.EventData{
		Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: docsRepo + "#51"},
	}}
	primary := v1alpha1.ContextItem{Kind: provider.KindIssue, Provider: ghProvider, Ref: docsRepo + "#42", Trust: TrustUntrusted}
	ticket := func(kind, ref string) v1alpha1.ContextItem {
		return v1alpha1.ContextItem{Kind: kind, Provider: ghProvider, Ref: ref, Trust: TrustUntrusted}
	}
	for _, tc := range []struct {
		name    string
		context []string
		ev      v1alpha1.Event
		target  *v1alpha1.Target
		fault   error
		want    []v1alpha1.ContextItem
		wantErr error
	}{
		{name: "sub-issue", context: []string{sub}, want: []v1alpha1.ContextItem{ticket(provider.KindIssue, sub), primary}},
		{name: "pr", context: []string{pr}, want: []v1alpha1.ContextItem{ticket(provider.KindPR, pr), primary}},
		{name: "gone is guessed an issue", context: []string{gone}, want: []v1alpha1.ContextItem{ticket(provider.KindIssue, gone), primary}},
		{name: "event object (not observed)", context: []string{docsRepo + "#51"}, ev: prEvent,
			want: []v1alpha1.ContextItem{ticket(provider.KindPR, docsRepo+"#51"), primary}},
		{name: "the primary link once", context: []string{docsRepo + "#42"}, want: []v1alpha1.ContextItem{primary}},
		{name: "not a ref", context: []string{"docs/README.md", "owner/name#x"}, want: []v1alpha1.ContextItem{
			{Kind: "ref", Ref: "docs/README.md", Trust: TrustUntrusted},
			{Kind: "ref", Ref: "owner/name#x", Trust: TrustUntrusted},
			primary,
		}},
		{name: "target provider without a connection", context: []string{sub}, target: &v1alpha1.Target{Provider: other, Repo: docsRepo},
			want: []v1alpha1.ContextItem{{Kind: provider.KindIssue, Provider: other, Ref: sub, Trust: TrustUntrusted}, primary}},
		{name: "refused is guessed an issue", context: []string{pr}, fault: provider.ErrInvalid,
			want: []v1alpha1.ContextItem{ticket(provider.KindIssue, pr), primary}},
		{name: "retryable", context: []string{pr}, fault: &provider.RetryableError{Err: errors.New("503")}, wantErr: provider.ErrRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fault != nil {
				e.gh.FailNext(fake.MethodObserve, tc.fault)
			}
			ev := tc.ev
			if ev.ID == "" {
				ev = v1alpha1.Event{ID: "ev", Source: ghProvider}
			}
			task, _, err := compileTask(e.ctx, compileInput{
				cat: e.cat, wi: wi, ev: ev, taskID: "tsk_c", now: e.clk.Now(), kindOf: e.rule.apply.objectKind,
				d: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc", Context: tc.context, Target: tc.target},
			}, KeepSkills{})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || errors.Is(err, ErrInvalidDispatch) {
					t.Fatalf("err = %v, want a transient %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := slices.DeleteFunc(slices.Clone(task.Spec.Context), func(c v1alpha1.ContextItem) bool { return c.Kind == "repo" })
			if !slices.Equal(got, tc.want) {
				t.Errorf("context = %+v\nwant      %+v", got, tc.want)
			}
		})
	}
}

// TestDispatchedTicketReachesTheLease is a dispatch naming a sub-issue: the leased task's
// context, which GET /tasks/{id}/context/{n} serves, has it as an issue on the provider.
func TestDispatchedTicketReachesTheLease(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	e.seed(provider.KindIssue, docsRepo+"#43", nil)
	key := "evt/ev-sub/test-rule/0"
	note, err := e.rule.apply.dispatch(e.ctx, &applyCtx{cat: e.cat, ev: v1alpha1.Event{ID: "ev-sub", Source: ghProvider}, wi: wi, now: e.clk.Now()}, applied{
		rule: "test-rule", key: key, verdict: v1alpha1.RailVerdict{Verdict: rules.VerdictAllowed},
		action: rules.ActionIntent{Dispatch: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc", Context: []string{docsRepo + "#43"}}},
	})
	if err != nil || note != "" {
		t.Fatalf("dispatch = %q, %v", note, err)
	}
	id := objectID("tsk", key)
	e.lease(id)
	task := e.task(id)
	if task.Status.Phase != v1alpha1.TaskLeased || len(e.runs(id)) != 1 {
		t.Fatalf("task %s is %s with %d runs, want Leased with 1", id, task.Status.Phase, len(e.runs(id)))
	}
	want := v1alpha1.ContextItem{Kind: provider.KindIssue, Provider: ghProvider, Ref: docsRepo + "#43", Trust: TrustUntrusted}
	if len(task.Spec.Context) == 0 || task.Spec.Context[0] != want {
		t.Errorf("leased context = %+v, want %+v first", task.Spec.Context, want)
	}
}

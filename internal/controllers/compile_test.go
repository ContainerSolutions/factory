package controllers

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/rules"
)

// withLimits is the PoC catalog with docs-review's limits replaced (the TaskSpec and
// the catalog are copied: the loaded ones are shared).
func withLimits(c *catalog.Catalog, l *v1alpha1.Limits) *catalog.Catalog {
	ts := *c.TaskSpecs["docs-review"]
	spec := *ts.Spec
	spec.Spec.Limits = l
	ts.Spec = &spec
	out := *c
	out.TaskSpecs = maps.Clone(c.TaskSpecs)
	out.TaskSpecs["docs-review"] = &ts
	return &out
}

// TestCompileLimits checks that every limit, the workspace size included, reaches the task as
// a copy.
func TestCompileLimits(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	for _, tc := range []struct {
		name   string
		limits *v1alpha1.Limits
	}{
		{name: "none"},
		{name: "size", limits: &v1alpha1.Limits{CPUs: 2, Memory: "4Gi"}},
		{name: "all", limits: &v1alpha1.Limits{MaxTurns: 40, MaxWallClock: "15m", MaxCostUSD: 1.5, CPUs: 4, Memory: "8Gi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, _, err := compileTask(e.ctx, compileInput{
				cat: withLimits(e.cat, tc.limits), wi: wi, ev: v1alpha1.Event{ID: "ev"}, taskID: "tsk_l", now: e.clk.Now(),
				d: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc"},
			}, KeepSkills{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(task.Spec.Limits, tc.limits) {
				t.Fatalf("limits = %+v, want %+v", task.Spec.Limits, tc.limits)
			}
			if tc.limits != nil && task.Spec.Limits == tc.limits {
				t.Error("the task's limits alias the TaskSpec's")
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

// withEnvironment is the PoC catalog with docs-review's workspace bringing up env and
// declaring a versions input (default def).
func withEnvironment(c *catalog.Catalog, env *v1alpha1.Environment, def any) *catalog.Catalog {
	return withTaskSpec(c, "docs-review", func(ts *catalog.CompiledTaskSpec) {
		if env == nil {
			return
		}
		ws := *ts.Spec.Spec.Workspace
		ws.Environment = env
		ts.Spec.Spec.Workspace = &ws
		ts.Spec.Spec.Inputs = maps.Clone(ts.Spec.Spec.Inputs)
		ts.Spec.Spec.Inputs["versions"] = v1alpha1.InputSpec{Type: "object", Default: def}
	})
}

func TestCompileEnvironment(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	env := func(from, input string) *v1alpha1.Environment {
		return &v1alpha1.Environment{
			Versions: &v1alpha1.VersionSource{From: from, Input: input},
			Setup:    "corepack enable",
			Up:       "make up",
			Ready:    &v1alpha1.ReadyCheck{HTTP: []string{"http://localhost:8080/api/health"}, Timeout: "20m"},
			Exposes:  map[string]string{"FACTORY_STACK_URL": "http://localhost:8000"},
			Egress:   &v1alpha1.EgressPolicy{Mode: "deny-by-default", Allow: []string{"registry.npmjs.org"}},
			Down:     "make down",
		}
	}
	for _, tc := range []struct {
		name    string
		env     *v1alpha1.Environment
		def     any
		inputs  map[string]any
		want    map[string]string
		wantErr bool
	}{
		{name: "no environment"},
		{name: "versions from inputs", env: env("inputs", "versions"),
			inputs: map[string]any{"versions": map[string]any{"api": "0.4.1", "ui": "v0.3.0"}},
			want:   map[string]string{"api": "0.4.1", "ui": "v0.3.0"}},
		{name: "versions from the default", env: env("inputs", "versions"), def: map[string]any{"agent": "0.2.0"},
			want: map[string]string{"agent": "0.2.0"}},
		{name: "absent input", env: env("inputs", "versions")},
		{name: "empty input", env: env("inputs", "versions"), inputs: map[string]any{"versions": map[string]any{}}},
		{name: "latest is deferred", env: env("latest", "")},
		{name: "train is deferred", env: env("train", "versions"), inputs: map[string]any{"versions": map[string]any{"api": "0.4.1"}}},
		{name: "not a map", env: env("inputs", "versions"), inputs: map[string]any{"versions": []any{"api"}}, wantErr: true},
		{name: "not a string", env: env("inputs", "versions"), inputs: map[string]any{"versions": map[string]any{"api": 1.0}}, wantErr: true},
		{name: "bad component", env: env("inputs", "versions"), inputs: map[string]any{"versions": map[string]any{"API": "0.4.1"}}, wantErr: true},
		{name: "empty version", env: env("inputs", "versions"), inputs: map[string]any{"versions": map[string]any{"api": ""}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := withEnvironment(e.cat, tc.env, tc.def)
			task, _, err := compileTask(e.ctx, compileInput{
				cat: cat, wi: wi, ev: v1alpha1.Event{ID: "ev"}, taskID: "tsk_e", now: e.clk.Now(),
				d: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc", Inputs: tc.inputs},
			}, KeepSkills{})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidDispatch) {
					t.Fatalf("err = %v, want ErrInvalidDispatch", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(task.Spec.Environment, tc.env) {
				t.Errorf("environment = %+v, want %+v", task.Spec.Environment, tc.env)
			}
			if !maps.Equal(task.Spec.Versions, tc.want) || (tc.want == nil && task.Spec.Versions != nil) {
				t.Errorf("versions = %#v, want %#v", task.Spec.Versions, tc.want)
			}
			if err := task.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
			if tc.env != nil {
				got := task.Spec.Environment
				got.Versions.Input, got.Ready.HTTP[0], got.Exposes["FACTORY_STACK_URL"], got.Egress.Allow[0] = "x", "x", "x", "x"
				if tc.env.Versions.Input == "x" || tc.env.Ready.HTTP[0] == "x" || tc.env.Exposes["FACTORY_STACK_URL"] == "x" || tc.env.Egress.Allow[0] == "x" {
					t.Error("the task's environment aliases the TaskSpec's")
				}
			}
			again, _, err := Recompile(e.ctx, cat, KeepSkills{}, wi, stored(t, task), RecompileOptions{TaskID: "tsk_e2", Now: e.clk.Now()})
			if err != nil {
				t.Fatalf("Recompile: %v", err)
			}
			if !maps.Equal(again.Spec.Versions, tc.want) {
				t.Errorf("recompiled versions = %#v, want %#v", again.Spec.Versions, tc.want)
			}
		})
	}
}

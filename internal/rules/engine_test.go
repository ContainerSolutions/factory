package rules

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/patterns"
)

// match finds the match of (set, rule).
func match(t *testing.T, ev Evaluation, set, rule string) Match {
	t.Helper()
	for _, m := range ev.Matches {
		if m.RuleSet == set && m.Rule == rule {
			return m
		}
	}
	t.Fatalf("no match for %s/%s in %+v", set, rule, ev.Matches)
	return Match{}
}

// decisions are the decisions as "ruleset/rule: action [verdict]".
func decisions(ev Evaluation) []string {
	out := make([]string, 0, len(ev.Decisions))
	for _, d := range ev.Decisions {
		out = append(out, d.RuleSet+"/"+d.Rule+": "+d.Action.String()+" ["+d.Rail.Verdict+"]")
	}
	return out
}

func wantDecisions(t *testing.T, ev Evaluation, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := decisions(ev); !reflect.DeepEqual(got, want) {
		t.Errorf("decisions:\n got %q\nwant %q", got, want)
	}
}

func wantResult(t *testing.T, ev Evaluation, set, rule string, want MatchResult) Match {
	t.Helper()
	m := match(t, ev, set, rule)
	if m.Result != want {
		t.Errorf("%s/%s: result %s, want %s (%s)", set, rule, m.Result, want, m.Error)
	}
	return m
}

// TestCascade is design/04-flows.md §How rules cascade, one event per step.
func TestCascade(t *testing.T) {
	ev := runCase(t, "cascade/1-issue-opened")
	wantDecisions(t, ev, "docs/triage-outside-issues: dispatch triage → triage [allowed]", "docs/triage-outside-issues: set triage [allowed]")

	ev = runCase(t, "cascade/2-triage-completed")
	wantDecisions(t, ev, "docs/request-implement-approval: set triage [allowed]", "docs/request-implement-approval: unsupported requestApproval [unsupported]")
	if ev.StateAfter["triage"] != "triaged" {
		t.Errorf("state after = %v", ev.StateAfter)
	}

	ev = runCase(t, "cascade/3-approval-granted")
	wantDecisions(t, ev, "docs/implement-on-approval: dispatch implement → default [allowed]", "docs/implement-on-approval: set implement [allowed]")

	ev = runCase(t, "cascade/4-factory-pr-opened")
	wantDecisions(t, ev, "docs/review-factory-prs: cancel review [allowed]", "docs/review-factory-prs: dispatch review → default [allowed]")
	h := ev.Decisions[1].Action.Dispatch.Harness
	if h == nil || h.Name != "claude-code" || h.Config == nil || !reflect.DeepEqual(h.Config.Model.Values, []string{"claude-sonnet-5-*"}) {
		t.Errorf("review harness = %+v", h)
	}
}

func TestPriorityAndFinal(t *testing.T) {
	ev := runCase(t, "cascade/final-autoimplement")
	wantResult(t, ev, "docs", "autoimplement-small-maintainer-issues", Matched)
	wantResult(t, ev, "docs", "request-implement-approval", SuppressedByFinal)
	wantDecisions(t, ev, "docs/autoimplement-small-maintainer-issues: dispatch implement → default [allowed]", "docs/autoimplement-small-maintainer-issues: set implement [allowed]")

	// A final rule whose actions fail still suppresses the rules below it (fails closed).
	ev = runCase(t, "engine/final-action-error")
	wantResult(t, ev, "b-loose", "final-guard", ActionError)
	wantResult(t, ev, "b-loose", "link-and-label", SuppressedByFinal)
	wantDecisions(t, ev)
}

func TestDedupeOnFiring(t *testing.T) {
	t.Run("every where passes", func(t *testing.T) {
		ev := runCase(t, "poc/escalate-needs-human-review-rounds")
		wantResult(t, ev, "poc-docs-epic", "escalate-needs-human", Matched)
		for _, set := range []string{"poc-docs-human-review", "poc-release-docs"} {
			if m := wantResult(t, ev, set, "escalate-needs-human", Duplicate); m.FiredVia != "poc-docs-epic" {
				t.Errorf("%s: firedVia %q", set, m.FiredVia)
			}
		}
		n := 0
		for _, d := range ev.Decisions {
			if d.Rule == "escalate-needs-human" {
				n++
				if d.RuleSet != "poc-docs-epic" {
					t.Errorf("decision through %s", d.RuleSet)
				}
			}
		}
		if n != 2 {
			t.Errorf("%d escalate decisions, want 2 (effect, notify)", n)
		}
	})
	t.Run("the first where fails (dev/prod params)", func(t *testing.T) {
		ev := runCase(t, "poc/train-prod")
		wantResult(t, ev, "dev-release-docs", "docs-on-release-train", NotMatched)
		wantResult(t, ev, "poc-release-docs", "docs-on-release-train", Matched)
		wantDecisions(t, ev,
			"poc-release-docs/docs-on-release-train: dispatch release-docs → poc [allowed]",
			"poc-release-docs/docs-on-release-train: set manifest [allowed]",
			"poc-release-docs/docs-on-release-train: set release [allowed]",
			"poc-release-docs/docs-on-release-train: set train [allowed]")
		if got := ev.Decisions[0].Action.Dispatch.Target.Repo; got != "compliance-framework/docs" {
			t.Errorf("prod train target = %q", got)
		}
		ev = runCase(t, "poc/train-mock")
		if got := ev.Decisions[0].Action.Dispatch.Target.Repo; got != "compliance-framework/mock-docs" || ev.Decisions[0].RuleSet != "dev-release-docs" {
			t.Errorf("mock train: %s target %q", ev.Decisions[0].RuleSet, got)
		}
	})
	t.Run("a where error doesn't consume the dedupe", func(t *testing.T) {
		ev := runCase(t, "engine/where-error-then-second-ruleset")
		m := wantResult(t, ev, "a-strict", "guarded-work", WhereError)
		if !strings.Contains(m.Error, "cel.eval") {
			t.Errorf("error = %q", m.Error)
		}
		wantResult(t, ev, "b-loose", "guarded-work", Matched)
		wantDecisions(t, ev, "b-loose/guarded-work: dispatch work → q [allowed]")
	})
	t.Run("a where passes but the actions fail: still fired", func(t *testing.T) {
		c := loadCatalog(t, "engine")
		f := readFixture(t, "testdata/cases/engine/set-then-bad-dispatch")
		// Select set-then-bad-dispatch through a second RuleSet too: it must not fire there.
		c2 := withRuleSet(c, "c-again", c.RuleSets["b-loose"])
		ev, err := (&Engine{}).Evaluate(context.Background(), c2, f.Event, f.WorkItem, AllowAll)
		if err != nil {
			t.Fatal(err)
		}
		wantResult(t, ev, "b-loose", "set-then-bad-dispatch", ActionError)
		if m := wantResult(t, ev, "c-again", "set-then-bad-dispatch", Duplicate); m.FiredVia != "b-loose" {
			t.Errorf("firedVia %q", m.FiredVia)
		}
		wantDecisions(t, ev)
	})
}

// withRuleSet returns a shallow copy of c with one more RuleSet.
func withRuleSet(c *catalog.Catalog, name string, rs *catalog.CompiledRuleSet) *catalog.Catalog {
	cp := *c
	cp.RuleSets = map[string]*catalog.CompiledRuleSet{name: rs}
	for k, v := range c.RuleSets {
		cp.RuleSets[k] = v
	}
	return &cp
}

// TestReleaseDocsBoundToManifest: both release-docs RuleSets scope workflows, so the pack's
// factory.* rules require the item's train manifest to be the RuleSet's.
func TestReleaseDocsBoundToManifest(t *testing.T) {
	ev := runCase(t, "poc/announce-release-docs")
	wantResult(t, ev, "dev-release-docs", "announce-release-docs", NotMatched)
	wantResult(t, ev, "poc-release-docs", "announce-release-docs", Matched)

	ev = runCase(t, "poc/announce-release-docs-dev")
	wantResult(t, ev, "dev-release-docs", "announce-release-docs", Matched)
	wantResult(t, ev, "poc-release-docs", "announce-release-docs", Duplicate)
	for _, d := range ev.Decisions {
		if d.Rule == "announce-release-docs" && d.RuleSet != "dev-release-docs" {
			t.Errorf("dev item announced through %s", d.RuleSet)
		}
	}
}

// TestStackReview: review-whole-stack (priority 50) reads stackNext before build-stack-next
// increments it, so the review is dispatched after the last PR only.
func TestStackReview(t *testing.T) {
	ev := runCase(t, "poc/build-stack-next-penultimate")
	wantResult(t, ev, "poc-docs-epic", "review-whole-stack", NotMatched)
	wantDecisions(t, ev, "poc-docs-epic/build-stack-next: set stackNext [allowed]", "poc-docs-epic/build-stack-next: dispatch implement → poc [allowed]")

	ev = runCase(t, "poc/review-whole-stack-last-pr")
	wantResult(t, ev, "poc-docs-epic", "build-stack-next", NotMatched)
	wantDecisions(t, ev, "poc-docs-epic/review-whole-stack: dispatch docs-review → poc [allowed]", "poc-docs-epic/review-whole-stack: set review [allowed]")
}

func TestFactoryEventScoping(t *testing.T) {
	ev := runCase(t, "cascade/factory-scope-linked-pr")
	wantResult(t, ev, "docs", "request-implement-approval", Matched)
	ev = runCase(t, "cascade/factory-scope-unlinked")
	if len(ev.Matches) != 0 || len(ev.Decisions) != 0 {
		t.Errorf("unlinked work item: %+v", ev)
	}
	ev = runCase(t, "poc/out-of-scope")
	if len(ev.Matches) != 0 {
		t.Errorf("out of scope: %+v", ev.Matches)
	}
}

func TestWhereError(t *testing.T) {
	ev := runCase(t, "poc/where-error-missing-state")
	for _, r := range []string{"build-stack-next", "review-whole-stack"} {
		m := wantResult(t, ev, "poc-docs-epic", r, WhereError)
		if !strings.Contains(m.Error, "no such key") {
			t.Errorf("%s: error %q", r, m.Error)
		}
	}
	wantDecisions(t, ev)
}

func TestActionErrorAfterSet(t *testing.T) {
	for _, tc := range []struct{ name, set, rule, action, msg string }{
		{"poc/build-stack-start-empty-plan", "poc-docs-epic", "build-stack-start", "actions[2].dispatch", "index out of bounds"},
		{"engine/set-then-bad-dispatch", "b-loose", "set-then-bad-dispatch", "actions[1].dispatch", `"other" is not one of`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readFixture(t, "testdata/cases/"+tc.name)
			ev := runCase(t, tc.name)
			m := wantResult(t, ev, tc.set, tc.rule, ActionError)
			if m.Action != tc.action || !strings.Contains(m.Error, tc.msg) {
				t.Errorf("action %q error %q", m.Action, m.Error)
			}
			for _, d := range ev.Decisions {
				if d.Rule == tc.rule {
					t.Errorf("decision from the failed rule: %s", d.Action)
				}
			}
			if !reflect.DeepEqual(ev.StateAfter, orEmpty(f.WorkItem.State)) {
				t.Errorf("state after = %v, want %v", ev.StateAfter, f.WorkItem.State)
			}
		})
	}
}

func TestStateVisibleToLaterActionsAndRules(t *testing.T) {
	ev := runCase(t, "poc/build-stack-next")
	d := ev.Decisions[1].Action.Dispatch
	if d == nil || d.Inputs["issue"] != "compliance-framework/docs#72" || d.Inputs["base"] != "factory/stack/1" {
		t.Errorf("dispatch = %+v", d)
	}
	if ev.StateAfter["stackNext"] != int64(1) {
		t.Errorf("stackNext = %#v", ev.StateAfter["stackNext"])
	}

	ev = runCase(t, "poc-merge/record-review-outcome-then-merge")
	wantDecisions(t, ev,
		"poc-docs-epic/record-review-outcome: set review [allowed]",
		"poc-docs-epic/merge-whole-stack: dispatch merge → poc [allowed]")

	ev = runCase(t, "poc/record-merge-outcome-then-announce")
	wantResult(t, ev, "poc-docs-epic", "announce-ptbr-live", Matched)
}

func TestRails(t *testing.T) {
	ev := runCase(t, "poc/rails-kill-switch")
	wantDecisions(t, ev, "poc-docs-human-review/review-human-prs: cancel docs-review [suppressed]", "poc-docs-human-review/review-human-prs: dispatch docs-review → poc [suppressed]")
	if r := ev.Decisions[0].Rail; r.Rail != RailKillSwitch || r.Reason != "item has factory:stop" {
		t.Errorf("verdict = %+v", r)
	}

	ev = runCase(t, "poc/rails-budget-deferred")
	wantDecisions(t, ev, "poc-docs-epic/plan-maintainer-epics: dispatch plan-epic → poc [deferred]", "poc-docs-epic/plan-maintainer-epics: set plan [allowed]")
	if ev.StateAfter["plan"] != "planning" {
		t.Errorf("state after = %v", ev.StateAfter)
	}

	// A suppressed set isn't applied, but the dispatch after it was staged with it.
	ev = runCase(t, "poc/rails-suppressed-set")
	wantDecisions(t, ev, "poc-docs-epic/build-stack-next: set stackNext [suppressed]", "poc-docs-epic/build-stack-next: dispatch implement → poc [allowed]")
	if ev.StateAfter["stackNext"] != float64(0) || ev.Decisions[1].Action.Dispatch.Inputs["issue"] != "compliance-framework/docs#72" {
		t.Errorf("state after %v, inputs %v", ev.StateAfter, ev.Decisions[1].Action.Dispatch.Inputs)
	}
}

func TestDebounce(t *testing.T) {
	ev := runCase(t, "cascade/debounce-release")
	if len(ev.Decisions) != 1 {
		t.Fatalf("decisions = %q", decisions(ev))
	}
	d := ev.Decisions[0].Action.Debounce
	if d == nil || d.Key != "docs-on-release/2026-11" || !d.Until.Equal(fixedNow.Add(2*time.Hour)) || len(d.Then) != 1 || d.Then[0].Dispatch == nil {
		t.Fatalf("debounce = %+v", d)
	}

	// Without a clock, now is the event's time.
	f := readFixture(t, "testdata/cases/cascade/debounce-release")
	ev, err := (&Engine{}).Evaluate(context.Background(), loadCatalog(t, "cascade"), f.Event, f.WorkItem, AllowAll)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.Decisions[0].Action.Debounce.Until; !got.Equal(f.Event.Time.Add(2 * time.Hour)) {
		t.Errorf("until = %v", got)
	}
}

func TestEvaluateErrors(t *testing.T) {
	c := loadCatalog(t, "poc")
	f := readFixture(t, "testdata/cases/poc/plan-maintainer-epics")
	e := &Engine{}
	if _, err := e.Evaluate(context.Background(), nil, f.Event, f.WorkItem, AllowAll); err == nil {
		t.Error("nil catalog accepted")
	}
	if _, err := e.Evaluate(context.Background(), c, f.Event, f.WorkItem, nil); err == nil {
		t.Error("nil rails accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Evaluate(ctx, c, f.Event, f.WorkItem, AllowAll); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

// TestEvaluateLeavesViewAlone: the view's state is read-only, and Evaluate is safe to call
// concurrently on one catalog.
func TestEvaluateLeavesViewAlone(t *testing.T) {
	f := readFixture(t, "testdata/cases/poc/build-stack-next")
	c := loadCatalog(t, "poc")
	before := map[string]any{}
	for k, v := range f.WorkItem.State {
		before[k] = v
	}
	var wg sync.WaitGroup
	results := make([]Evaluation, 8)
	for i := range results {
		wg.Go(func() {
			ev, err := (&Engine{}).Evaluate(context.Background(), c, f.Event, f.WorkItem, AllowAll)
			if err != nil {
				t.Error(err)
			}
			results[i] = ev
		})
	}
	wg.Wait()
	if !reflect.DeepEqual(f.WorkItem.State, before) {
		t.Errorf("view state changed: %v", f.WorkItem.State)
	}
	for _, ev := range results[1:] {
		if !reflect.DeepEqual(ev, results[0]) {
			t.Error("concurrent evaluations differ")
		}
	}
}

func TestRecords(t *testing.T) {
	ev := runCase(t, "poc/escalate-needs-human-review-rounds")
	at := fixedNow.Add(time.Second)
	recs := ev.Records(at)
	if len(recs) != len(ev.Matches) {
		t.Fatalf("%d records for %d matches", len(recs), len(ev.Matches))
	}
	byKey := map[string]v1alpha1.Decision{}
	for _, r := range recs {
		if !r.At.Equal(at) || r.Event != ev.Event {
			t.Errorf("record %+v", r)
		}
		byKey[r.RuleSet+"/"+r.Rule] = r
	}
	r := byKey["poc-docs-epic/escalate-needs-human"]
	if !r.Fired || !reflect.DeepEqual(r.Actions, []string{"effect label.add", "notify needs-human"}) || len(r.Rails) != 2 || r.Inputs["run"] == nil {
		t.Errorf("fired record %+v", r)
	}
	if r := byKey["poc-release-docs/escalate-needs-human"]; r.Fired || r.Reason != "already fired through RuleSet poc-docs-epic" {
		t.Errorf("duplicate record %+v", r)
	}
	if r := byKey["poc-docs-epic/fix-stack-review"]; r.Fired || r.Reason != "where is false" {
		t.Errorf("not-matched record %+v", r)
	}
}

func TestFakeRails(t *testing.T) {
	rails := FakeRails{Trips: []RailTrip{
		{Rail: RailActiveRun, Action: KindDispatch, TaskSpec: "implement"},
		{Rail: RailBudget, Queue: "poc"},
		{Rail: RailRunCap, Action: KindCancel},
	}}
	for _, tc := range []struct {
		a    ActionIntent
		want v1alpha1.RailVerdict
	}{
		{ActionIntent{Dispatch: &Dispatch{TaskSpec: "implement", Queue: "poc"}}, Suppressed(RailActiveRun, RailActiveRun)},
		{ActionIntent{Dispatch: &Dispatch{TaskSpec: "merge", Queue: "poc"}}, Deferred(RailBudget, RailBudget)},
		{ActionIntent{Dispatch: &Dispatch{TaskSpec: "merge", Queue: "other"}}, Allowed()},
		{ActionIntent{Cancel: &Cancel{TaskSpec: "x"}}, Suppressed(RailRunCap, RailRunCap)},
		{ActionIntent{Set: &Set{Key: "k"}}, Allowed()},
	} {
		if got := rails.Check(tc.a); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.a, got, tc.want)
		}
	}
}

func TestMatchScope(t *testing.T) {
	compile := func(globs ...string) []patterns.Pattern {
		var out []patterns.Pattern
		for _, g := range globs {
			p, err := patterns.Compile(g)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	repos := catalog.CompiledScope{Repos: compile("org/*"), Exclude: compile("org/secret")}
	channels := catalog.CompiledScope{Channels: compile("#docs")}
	branches := catalog.CompiledScope{Repos: compile("org/*"), Branches: compile("main")}
	for _, tc := range []struct {
		name                  string
		s                     catalog.CompiledScope
		repo, channel, branch string
		want                  bool
	}{
		{"repo in scope", repos, "org/docs", "", "", true},
		{"repo excluded", repos, "org/secret", "", "", false},
		{"repo outside", repos, "other/docs", "", "", false},
		{"channel against a repo scope", repos, "", "#docs", "", false},
		{"channel in scope", channels, "", "#docs", "", true},
		{"repo against a channel scope", channels, "org/docs", "", "", false},
		{"no object, scoped", repos, "", "", "", false},
		{"no object, unscoped", catalog.CompiledScope{}, "", "", "", true},
		{"branch in scope", branches, "org/docs", "", "main", true},
		{"branch outside", branches, "org/docs", "", "dev", false},
	} {
		if got := matchScope(tc.s, tc.repo, tc.channel, tc.branch); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRefs(t *testing.T) {
	for _, tc := range []struct {
		ref, repo string
		num       int64
		ok        bool
	}{
		{"compliance-framework/docs#42", "compliance-framework/docs", 42, true},
		{"compliance-framework/docs", "compliance-framework/docs", 0, false},
		{"compliance-framework/api#v1.4.0", "compliance-framework/api", 0, false},
		{"C123/1700000000.1", "C123/1700000000.1", 0, false},
		{"docs#1", "", 1, true},
		{"/x#1", "", 1, true},
	} {
		if got := refRepo(tc.ref); got != tc.repo {
			t.Errorf("refRepo(%q) = %q, want %q", tc.ref, got, tc.repo)
		}
		if n, ok := refNumber(tc.ref); n != tc.num || ok != tc.ok {
			t.Errorf("refNumber(%q) = %d, %v", tc.ref, n, ok)
		}
	}
}

func TestBound(t *testing.T) {
	long := strings.Repeat("é", snapshotString) // 2 bytes per rune: cut on a rune boundary
	list := make([]any, snapshotList+5)
	keys := map[string]any{}
	for i := range snapshotKeys + 3 {
		keys[string(rune('A'+i%26))+strings.Repeat("x", i)] = i
	}
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"e": 1}}}}}

	s := bound(long, 0).(string)
	if !strings.HasSuffix(s, truncated) || len(s) > snapshotString+len(truncated) || !strings.HasPrefix(s, "é") {
		t.Errorf("string: %d bytes", len(s))
	}
	if l := bound(list, 0).([]any); len(l) != snapshotList+1 || l[snapshotList] != truncated {
		t.Errorf("list: %d", len(l))
	}
	if m := bound(keys, 0).(map[string]any); len(m) != snapshotKeys+1 || m[truncated] != 3 {
		t.Errorf("map: %d keys, rest %v", len(m), m[truncated])
	}
	got := bound(deep, 0).(map[string]any)["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"]
	if got != truncated {
		t.Errorf("depth: %v", got)
	}
}

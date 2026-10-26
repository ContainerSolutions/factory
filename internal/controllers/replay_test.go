package controllers

import (
	"errors"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// A replay applies only what the event didn't do the first time: here the factory:stop label
// suppressed the dispatch, and once it is gone the replay dispatches plan-epic and records the
// new decision as a replay. A second replay changes nothing.
func TestReplay(t *testing.T) {
	e := newEnv(t)
	ref := docsRepo + "#61"
	e.seed(provider.KindIssue, ref, func(o *provider.Observation) { o.Labels = []string{StopLabel} })
	evs := e.deliver(issueOpened(ref, maintainer))
	e.processEvents()
	wi := e.workItemOf(ref)
	if n := len(e.tasks(wi.Metadata.Name)); n != 0 {
		t.Fatalf("tasks of a stopped item = %d, want 0", n)
	}
	before := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions)

	e.seed(provider.KindIssue, ref, nil) // the label is removed
	e.clk.Advance(DefaultObservationTTL + 1)
	res, err := e.rule.Replay(e.ctx, evs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkItem != wi.Metadata.Name {
		t.Errorf("replay work item = %q, want %q", res.WorkItem, wi.Metadata.Name)
	}
	if len(res.Decisions) != 1 {
		t.Fatalf("replay decisions = %+v, want the plan-maintainer-epics one", res.Decisions)
	}
	d := res.Decisions[0]
	if !d.Replay || !d.Fired || d.Rule != "plan-maintainer-epics" || d.Event != evs[0].ID || d.Rails[0].Verdict != rules.VerdictAllowed {
		t.Errorf("replay decision = %+v", d)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "plan-epic")); n != 1 {
		t.Errorf("plan-epic tasks after the replay = %d, want 1", n)
	}
	got := e.getWorkItem(wi.Metadata.Name).Status.Decisions
	if len(got) != before+1 || !got[len(got)-1].Replay {
		t.Errorf("decisions after the replay = %d (last %+v), want %d with a replay last", len(got), got[len(got)-1], before+1)
	}

	// Nothing differs any more: no decision, no second task.
	again, err := e.rule.Replay(e.ctx, evs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Decisions) != 0 {
		t.Errorf("second replay decisions = %+v, want none", again.Decisions)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "plan-epic")); n != 1 {
		t.Errorf("plan-epic tasks after a second replay = %d, want 1", n)
	}
	if n := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions); n != before+1 {
		t.Errorf("decisions after a second replay = %d, want %d", n, before+1)
	}
}

// Replaying an event that already did everything leaves the item as it is: the rule that
// fired isn't applied again (its set doesn't overwrite later state) and records nothing.
func TestReplayAppliesNothingTwice(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	e.completePlan(plan.Metadata.ID)
	done := e.events(v1alpha1.EventFactoryRunCompleted)
	if len(done) != 1 {
		t.Fatalf("run.completed events = %d", len(done))
	}
	item := e.getWorkItem(wi.Metadata.Name)
	before := len(item.Status.Decisions)
	item.Status.State["plan"] = "changed since"
	if err := e.st.Update(e.ctx, item); err != nil {
		t.Fatal(err)
	}
	effects := len(e.gh.Effects())

	res, err := e.rule.Replay(e.ctx, done[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Decisions) != 0 {
		t.Errorf("replay decisions = %+v, want none", res.Decisions)
	}
	after := e.getWorkItem(wi.Metadata.Name)
	if n := len(after.Status.Decisions); n != before {
		t.Errorf("decisions = %d, want %d", n, before)
	}
	if after.Status.State["plan"] != "changed since" {
		t.Errorf("state plan = %v: the replay re-applied a set", after.Status.State["plan"])
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "implement")); n != 1 {
		t.Errorf("implement tasks = %d, want 1", n)
	}
	if n := len(e.gh.Effects()); n != effects {
		t.Errorf("provider effects = %d, want %d", n, effects)
	}
}

func TestReplayMissingEvent(t *testing.T) {
	e := newEnv(t)
	if _, err := e.rule.Replay(e.ctx, "gh-nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Replay(missing) = %v, want ErrNotFound", err)
	}
}

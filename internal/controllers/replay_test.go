package controllers

import (
	"errors"
	"slices"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

const epicRuleSet = "poc-docs-epic"

// catalogRule returns the RuleSet's effective rule name, for a test to change.
func (e *env) catalogRule(ruleSet, name string) (*catalog.CompiledRuleSet, int) {
	e.t.Helper()
	rs := e.cat.RuleSets[ruleSet]
	i := slices.IndexFunc(rs.Rules, func(r *catalog.EffectiveRule) bool { return r.Name == name })
	if i < 0 {
		e.t.Fatalf("no rule %s in %s", name, ruleSet)
	}
	return rs, i
}

// issueEvent is the stored issue.opened event of startPlan.
func (e *env) issueEvent() *v1alpha1.Event {
	e.t.Helper()
	evs := e.events(v1alpha1.EventIssueOpened)
	if len(evs) != 1 {
		e.t.Fatalf("issue.opened events = %d, want 1", len(evs))
	}
	return evs[0]
}

// A rule the catalog adds after the event acts on replay, once; the rule that fired then
// doesn't act again.
func TestReplayNewRule(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	ev := e.issueEvent()
	rs, i := e.catalogRule(epicRuleSet, "plan-maintainer-epics")
	added := *rs.Rules[i]
	added.Name = "plan-maintainer-epics-again"
	added.Actions = added.Actions[1:] // its set plan: planning only
	rs.Rules = append(rs.Rules, &added)
	item := e.getWorkItem(wi.Metadata.Name)
	item.Status.State["plan"] = "changed since"
	if err := e.st.Update(e.ctx, item); err != nil {
		t.Fatal(err)
	}
	before := len(item.Status.Decisions)

	res, err := e.rule.Replay(e.ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkItem != wi.Metadata.Name || len(res.Decisions) != 1 {
		t.Fatalf("replay = %+v, want the new rule's decision only", res)
	}
	if d := res.Decisions[0]; !d.Replay || !d.Fired || d.Rule != added.Name || d.Event != ev.ID {
		t.Errorf("replay decision = %+v", d)
	}
	after := e.getWorkItem(wi.Metadata.Name)
	if after.Status.State["plan"] != "planning" {
		t.Errorf("state plan = %v, want the new rule's set applied", after.Status.State["plan"])
	}
	if n := len(after.Status.Decisions); n != before+1 {
		t.Errorf("decisions = %d, want %d", n, before+1)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "plan-epic")); n != 1 {
		t.Errorf("plan-epic tasks = %d, want 1: the fired rule dispatched again", n)
	}

	// The new rule has fired now: a second replay applies nothing.
	after.Status.State["plan"] = "changed again"
	if err := e.st.Update(e.ctx, after); err != nil {
		t.Fatal(err)
	}
	again, err := e.rule.Replay(e.ctx, ev.ID)
	if err != nil || len(again.Decisions) != 0 {
		t.Errorf("second replay = %+v, %v; want nothing", again, err)
	}
	if got := e.getWorkItem(wi.Metadata.Name).Status.State["plan"]; got != "changed again" {
		t.Errorf("state plan after a second replay = %v", got)
	}
}

// A rule that fired, suppressed by a rail then, isn't applied on replay once the rail allows
// it: whatever changed, a fired rule is never re-applied.
func TestReplayRailChanged(t *testing.T) {
	e := newEnv(t)
	ref := docsRepo + "#61"
	e.seed(provider.KindIssue, ref, func(o *provider.Observation) { o.Labels = []string{StopLabel} })
	evs := e.deliver(issueOpened(ref, maintainer))
	e.processEvents()
	wi := e.workItemOf(ref)
	before := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions)

	e.seed(provider.KindIssue, ref, nil) // the label is removed
	e.clk.Advance(DefaultObservationTTL + 1)
	res, err := e.rule.Replay(e.ctx, evs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Decisions) != 0 {
		t.Errorf("replay decisions = %+v, want none", res.Decisions)
	}
	item := e.getWorkItem(wi.Metadata.Name)
	if n := len(e.tasks(wi.Metadata.Name)); n != 0 {
		t.Errorf("tasks = %d, want none", n)
	}
	if _, ok := item.Status.State["plan"]; ok || len(item.Status.Decisions) != before {
		t.Errorf("state %v, %d decisions (want %d): the fired rule was re-applied", item.Status.State, len(item.Status.Decisions), before)
	}
}

// An action the catalog inserts before a fired rule's dispatch shifts its key: the replay
// must not dispatch again under the new key.
func TestReplayInsertedAction(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	ev := e.issueEvent()
	rs, i := e.catalogRule(epicRuleSet, "plan-maintainer-epics")
	changed := *rs.Rules[i]
	changed.Actions = append([]catalog.CompiledAction{changed.Actions[0]}, changed.Actions...)
	rs.Rules[i] = &changed

	res, err := e.rule.Replay(e.ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Decisions) != 0 {
		t.Errorf("replay decisions = %+v, want none", res.Decisions)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "plan-epic")); n != 1 {
		t.Errorf("plan-epic tasks = %d, want 1", n)
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

// When the event's records aged out of its work item, the replay can't tell what fired: it
// refuses, and changes nothing. An event that recorded nothing has nothing to lose.
func TestReplayHistoryGone(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	e.completePlan(plan.Metadata.ID)
	done := e.events(v1alpha1.EventFactoryRunCompleted)[0]
	item := e.getWorkItem(wi.Metadata.Name)
	item.Status.Decisions = slices.DeleteFunc(item.Status.Decisions, func(d v1alpha1.Decision) bool { return d.Event == done.ID })
	item.Status.State["plan"] = "changed since"
	if err := e.st.Update(e.ctx, item); err != nil {
		t.Fatal(err)
	}
	before := len(item.Status.Decisions)

	if _, err := e.rule.Replay(e.ctx, done.ID); !errors.Is(err, ErrReplayHistoryGone) {
		t.Fatalf("Replay with its records gone = %v, want ErrReplayHistoryGone", err)
	}
	after := e.getWorkItem(wi.Metadata.Name)
	if after.Status.State["plan"] != "changed since" || len(after.Status.Decisions) != before {
		t.Errorf("state %v, %d decisions: the refused replay changed the item", after.Status.State, len(after.Status.Decisions))
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "implement")); n != 1 {
		t.Errorf("implement tasks = %d, want 1", n)
	}

	// A mark that recorded nothing for the item: nothing to lose, the replay runs.
	var st EventStatus
	if err := e.st.Get(e.ctx, store.Key{Kind: KindEventStatus, Name: done.ID}, &st); err != nil {
		t.Fatal(err)
	}
	if st.Recorded == nil || *st.Recorded == 0 {
		t.Fatalf("mark %+v: want the records counted", st)
	}
	zero := 0
	st.Recorded = &zero
	if err := e.st.Update(e.ctx, &st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rule.Replay(e.ctx, done.ID); err != nil {
		t.Errorf("Replay of an event that recorded nothing = %v", err)
	}
}

func TestReplayMissingEvent(t *testing.T) {
	e := newEnv(t)
	if _, err := e.rule.Replay(e.ctx, "gh-nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Replay(missing) = %v, want ErrNotFound", err)
	}
}

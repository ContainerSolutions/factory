package controllers

import (
	"errors"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/store"
)

// Closing cancels the item's active tasks and records why; the rules then evaluate nothing
// for it: a later event is stored and processed, and its decision says the item is closed.
func TestCloseWorkItem(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	at := e.clk.Now()
	got, cancelled, err := CloseWorkItem(e.ctx, e.st, wi.Metadata.Name, v1alpha1.WorkItemClosed{
		Reason: v1alpha1.CloseWontDo, Note: "out of scope", At: at, By: "github-public:gus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Status.Closed; c == nil || c.Reason != v1alpha1.CloseWontDo || c.Note != "out of scope" || !c.At.Equal(at) || c.By != "github-public:gus" {
		t.Errorf("closed = %+v", got.Status.Closed)
	}
	if len(cancelled) != 1 || cancelled[0] != plan.Metadata.ID {
		t.Errorf("cancelled = %v, want [%s]", cancelled, plan.Metadata.ID)
	}
	if p := e.task(plan.Metadata.ID).Status; p.Phase != v1alpha1.TaskCancelling || p.Reason != ReasonCancelled {
		t.Errorf("plan task status = %+v, want Cancelling{cancelled}", p)
	}
	last := got.Status.Decisions[len(got.Status.Decisions)-1]
	if last.Reason != "closed as wont-do by github-public:gus: out of scope" || last.Event != "" {
		t.Errorf("close decision = %+v", last)
	}

	// A later event on the closed item: processed, nothing evaluated.
	evs := e.deliver(issueOpened(docsRepo+"#42", maintainer))
	e.processEvents()
	if n := len(e.tasks(wi.Metadata.Name)); n != 1 {
		t.Errorf("tasks = %d, want only the cancelled plan-epic", n)
	}
	item := e.getWorkItem(wi.Metadata.Name)
	d := item.Status.Decisions[len(item.Status.Decisions)-1]
	if d.Event != evs[0].ID || d.Fired || d.Reason != "the work item is closed (wont-do): no rules evaluated" {
		t.Errorf("decision for the later event = %+v", d)
	}
	if done, _ := e.rule.processed(e.ctx, evs[0].ID); !done {
		t.Error("the later event is not marked processed")
	}

	// A replay says so once.
	res, err := e.rule.Replay(e.ctx, evs[0].ID)
	if err != nil || len(res.Decisions) != 0 {
		t.Errorf("replay on a closed item = %+v, %v; want no new decision", res, err)
	}

	if _, _, err := CloseWorkItem(e.ctx, e.st, wi.Metadata.Name, v1alpha1.WorkItemClosed{Reason: "later"}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("close with a bad reason = %v, want ErrInvalid", err)
	}
	if _, _, err := CloseWorkItem(e.ctx, e.st, "wi_nope", v1alpha1.WorkItemClosed{Reason: v1alpha1.CloseDone}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("close a missing item = %v, want ErrNotFound", err)
	}
}

// Delete removes the item and what is keyed to it, and nothing of another item; a leased task
// refuses it unless forced.
func TestDeleteWorkItem(t *testing.T) {
	e := newEnv(t)
	other := docsRepo + "#61"
	e.seed(provider.KindIssue, other, nil)
	e.deliver(issueOpened(other, maintainer))
	wi, plan := e.startPlan()
	e.completePlan(plan.Metadata.ID)
	keep := e.workItemOf(other)
	impl := e.tasksOf(wi.Metadata.Name, "implement")[0]
	e.lease(impl.Metadata.ID)

	if _, err := DeleteWorkItem(e.ctx, e.st, wi.Metadata.Name, false); !errors.Is(err, ErrTaskLeased) {
		t.Fatalf("delete with a leased task = %v, want ErrTaskLeased", err)
	}
	got, err := DeleteWorkItem(e.ctx, e.st, wi.Metadata.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kinds[v1alpha1.KindWorkItem] != 1 || got.Kinds[v1alpha1.KindTask] != 2 || got.Kinds[KindTaskPrompt] != 2 ||
		got.Kinds[v1alpha1.KindRun] != 2 || got.Kinds[v1alpha1.KindEffect] == 0 || got.Kinds[v1alpha1.KindEvent] == 0 ||
		got.Kinds[KindEventStatus] == 0 || len(got.Tasks) != 2 {
		t.Errorf("deleted = %+v", got)
	}
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wi.Metadata.Name}, &v1alpha1.WorkItem{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the work item is still there: %v", err)
	}
	if n := len(e.tasks(wi.Metadata.Name)); n != 0 {
		t.Errorf("tasks left = %d", n)
	}
	var effects []*v1alpha1.Effect
	if _, err := e.st.List(e.ctx, v1alpha1.KindEffect, store.ListOptions{}, &effects); err != nil {
		t.Fatal(err)
	}
	if len(effects) != 0 {
		t.Errorf("effects left = %d: %+v", len(effects), effects[0].Spec)
	}
	var evs []*v1alpha1.Event
	if _, err := e.st.List(e.ctx, v1alpha1.KindEvent, store.ListOptions{}, &evs); err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.WorkItem == wi.Metadata.Name || strings.Contains(ev.ID, plan.Metadata.ID[len("tsk_"):]) {
			t.Errorf("event left: %s", ev.ID)
		}
	}
	if len(evs) != 1 {
		t.Errorf("events left = %d, want the other item's", len(evs))
	}
	if k := e.getWorkItem(keep.Metadata.Name); k == nil || len(e.tasks(keep.Metadata.Name)) != 1 {
		t.Errorf("the other item lost its task")
	}
	if _, err := DeleteWorkItem(e.ctx, e.st, wi.Metadata.Name, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete again = %v, want ErrNotFound", err)
	}
}

func TestRuleEffectOf(t *testing.T) {
	known := map[string]bool{"p/d": true, "p/d/0": true, "factory/run/x": true}
	tests := []struct {
		key  string
		mine map[string]bool
		want bool
	}{
		{"evt/p/d/0/rule/0", map[string]bool{"p/d/0": true}, true},
		{"evt/p/d/0/rule/0", map[string]bool{"p/d": true}, false},
		{"evt/p/d/rule/0", map[string]bool{"p/d": true}, true},
		{"evt/factory/run/x/set/rs/inline/0/1", map[string]bool{"factory/run/x": true}, true},
		{"evt/unknown/rule/0", map[string]bool{"p/d": true}, false},
		{"dbn/abc/0", nil, true},
		{"dbn/abcd/0", nil, false},
		{"tsk_1/comment/1", map[string]bool{"p/d": true}, false},
	}
	for _, tt := range tests {
		if got := ruleEffectOf(tt.key, known, tt.mine, []string{"abc"}); got != tt.want {
			t.Errorf("ruleEffectOf(%q, %v) = %v, want %v", tt.key, tt.mine, got, tt.want)
		}
	}
}

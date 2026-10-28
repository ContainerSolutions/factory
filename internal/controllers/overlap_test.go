package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/rules"
	"github.com/ContainerSolutions/factory/internal/store"
)

// Overlapping leaders (P3 §Several dispatchers): a leader paused past its lease may still be
// inside a reconcile that read its input before the other leader acted on it. Under the
// work item's lock, that input is read again, so nothing is applied twice.

// TestOverlapRuleEventProcessedOnce: an event a reconcile read as unprocessed, processed by
// the other leader before this one takes the lock, adds no decision.
func TestOverlapRuleEventProcessedOnce(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan() // issue opened: processed, a plan-epic dispatched
	before := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions)
	ev := e.events(v1alpha1.EventIssueOpened)[0]
	if err := e.rule.process(e.ctx, e.cat, *ev, wi.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	if after := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions); after != before {
		t.Fatalf("decisions %d -> %d: the event was applied twice", before, after)
	}
}

// TestOverlapDebounceFiresOnce: a debounce a reconcile read before the other leader fired it
// fires nothing the second time.
func TestOverlapDebounceFiresOnce(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	rec := &DebounceRecord{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindDebounce},
		Metadata: v1alpha1.ObjectMeta{Name: "dbn_overlap"},
		Spec: DebounceSpec{
			Key: "r/" + wi.Metadata.Name, WorkItem: wi.Metadata.Name, Event: "none", RuleSet: "poc-docs-epic", Rule: "r", Until: e.clk.Now(),
			Then: []rules.ActionIntent{{Notify: &rules.Notify{Template: "ptbr-live", To: []v1alpha1.Destination{{Item: "primary"}}}}},
		},
	}
	if err := e.st.Create(e.ctx, rec); err != nil {
		t.Fatal(err)
	}
	stale := *rec // what the paused leader read
	if _, err := e.dbn.Reconcile(e.ctx, rec.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	before := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions)
	if err := e.dbn.fire(e.ctx, e.cat, &stale); err != nil {
		t.Fatal(err)
	}
	if n := len(e.applies(v1alpha1.OpComment)); n != 1 {
		t.Errorf("comments = %d, want 1", n)
	}
	if after := len(e.getWorkItem(wi.Metadata.Name).Status.Decisions); after != before {
		t.Errorf("decisions %d -> %d: the debounce fired twice", before, after)
	}
}

// TestOverlapProjectionUnderTheLock: the work-item controller projects under the item's lock,
// so it waits while another holds it, and projects nothing meanwhile.
func TestOverlapProjectionUnderTheLock(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	u, err := e.st.Lock(e.ctx, "wi/"+wi.Metadata.Name, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := e.wic.Reconcile(ctx, wi.Metadata.Name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile while the item is locked = %v, want it to wait for the lock", err)
	}
	if n := len(e.gh.Projections()); n != 0 {
		t.Fatalf("%d projections while another held the item's lock", n)
	}
	u.Unlock()
	if _, err := e.wic.Reconcile(e.ctx, wi.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	if n := len(e.gh.Projections()); n != 1 {
		t.Fatalf("%d projections once the lock was free, want 1", n)
	}
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wi.Metadata.Name}, &v1alpha1.WorkItem{}); err != nil {
		t.Fatal(err)
	}
}

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

// TestWorkItemForSyncsOnAnEmptyLookup: an object linked on one dispatcher just before
// another, still behind, looks it up: workItemFor finds the item (its empty lookup syncs)
// rather than creating a second one (P3 §Reads and freshness).
func TestWorkItemForSyncsOnAnEmptyLookup(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	b := objectstore.NewFakeBucket(clk)
	open := func(id string) *objectstore.Store {
		s, err := objectstore.Open(ctx, b, store.Options{Clock: clk, Logger: discardLogger()},
			objectstore.WithWriterID(id), objectstore.WithSyncInterval(time.Hour),
			objectstore.WithKind(KindEventStatus, &EventStatus{}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	sa, sb := open("ins_a"), open("ins_b")

	// A has the work item of docs#42, and links the PR an agent's effect just opened.
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: "wi_42"},
		Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: ghProvider, Ref: docsRepo + "#42", Kind: provider.KindIssue, Primary: true}}},
	}
	if err := sa.Create(ctx, wi); err != nil {
		t.Fatal(err)
	}
	if err := sb.Sync(ctx); err != nil { // B saw the item, then falls behind
		t.Fatal(err)
	}
	pr := v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindPR, Ref: docsRepo + "#57"}
	if _, err := LinkObject(ctx, sa, wi.Metadata.Name, pr, ghProvider); err != nil {
		t.Fatal(err)
	}

	// The PR's webhook reaches B's rule controller.
	cat := loadPoC(t)
	rule, err := NewRuleController(sb, staticCatalog{cat}, StaticConns{}, RuleOptions{Clock: clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ev := v1alpha1.Event{
		ID: "evt_pr57", Type: v1alpha1.EventPRLabeled, Source: ghProvider,
		Data: v1alpha1.EventData{Object: &pr, Repo: docsRepo},
	}
	got, err := rule.workItemFor(ctx, cat, ev)
	if err != nil {
		t.Fatal(err)
	}
	if got != wi.Metadata.Name {
		t.Fatalf("workItemFor = %q, want A's %q", got, wi.Metadata.Name)
	}
	var all []*v1alpha1.WorkItem
	if _, err := sb.List(ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &all); err != nil || len(all) != 1 {
		t.Fatalf("work items = %d (%v), want 1", len(all), err)
	}
}

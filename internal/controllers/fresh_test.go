package controllers

import (
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
)

// An event is evaluated against an observation of its own object no older than the event: a
// cached one made before it is observed again, for a new work item and an existing one; one
// made after it is used as is. (docs#151: pr.ready_for_review, 30 s after pr.opened, read the
// draft the opened event had cached, so review-human-prs' !item.pr.draft was false.)
func TestEventObservesItsOwnObjectFresh(t *testing.T) {
	pr := docsRepo + "#151"
	for _, tc := range []struct {
		name     string
		existing bool          // the PR already has a work item
		openedAt time.Duration // pr.opened (the draft observed and cached), after t0
		readyAt  time.Duration // the pr.ready_for_review event's time, after t0
		observes int           // the PR's provider observations, pr.opened's included
		reviews  int           // docs-review tasks dispatched
	}{
		{name: "new item, event after the cached read", readyAt: 10 * time.Second, observes: 2, reviews: 1},
		{name: "new item, event before the cached read", openedAt: 10 * time.Second, readyAt: 5 * time.Second, observes: 1},
		{name: "existing item, event after the cached read", existing: true, readyAt: 10 * time.Second, observes: 2, reviews: 1},
		{name: "existing item, event before the cached read", existing: true, openedAt: 10 * time.Second, readyAt: 5 * time.Second, observes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, cc, _ := newScopeEnv(t)
			e.seed(provider.KindPR, pr, func(o *provider.Observation) { o.PR.Draft = true })
			if tc.existing {
				if err := e.st.Create(e.ctx, &v1alpha1.WorkItem{
					TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
					Metadata: v1alpha1.ObjectMeta{Name: "wi_draft-pr"},
					Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: ghProvider, Ref: pr, Kind: provider.KindPR, Primary: true}}},
					Status:   &v1alpha1.WorkItemStatus{State: map[string]any{}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			e.clk.Set(t0.Add(tc.openedAt))
			e.deliver(webhook(v1alpha1.EventPROpened, provider.KindPR, pr, "alice"))
			e.processEvents()
			if got := cc.observes(pr); got != 1 {
				t.Fatalf("pr.opened observed the PR %d times, want 1", got)
			}

			if err := e.gh.Update(pr, func(o *provider.Observation) { o.PR.Draft = false }); err != nil {
				t.Fatal(err)
			}
			e.clk.Set(t0.Add(15 * time.Second)) // the cached read is within the TTL
			w := webhook(v1alpha1.EventPRReadyForReview, provider.KindPR, pr, "alice")
			w.Time = t0.Add(tc.readyAt)
			evs := e.deliver(w)
			e.processEvents()

			st, ok := eventStatus(e, evs[0].ID)
			if !ok {
				t.Fatalf("event %s not processed", evs[0].ID)
			}
			reviews := 0
			if st.WorkItem != "" {
				reviews = len(e.tasksOf(st.WorkItem, "docs-review"))
			}
			if reviews != tc.reviews {
				t.Errorf("docs-review tasks = %d (work item %q), want %d", reviews, st.WorkItem, tc.reviews)
			}
			if got := cc.observes(pr); got != tc.observes {
				t.Errorf("PR observed %d times, want %d", got, tc.observes)
			}
		})
	}
}

package controllers

import (
	"slices"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/store"
)

func TestProjectionOnlyWhenChanged(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	for range 2 {
		if _, err := e.wic.Reconcile(e.ctx, wi.Metadata.Name); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.gh.Projections()); n != 1 {
		t.Fatalf("Project calls = %d, want 1 (unchanged the second time)", n)
	}
	got := e.gh.Projections()[0]
	if got.Projection.WorkItemID != wi.Metadata.Name || got.Projection.State["plan"] != "planning" || got.Ref.Ref != docsRepo+"#42" {
		t.Errorf("projection = %+v", got)
	}
}

func TestResyncEmitsResynced(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	id := wi.Metadata.Name
	res, err := e.wic.Reconcile(e.ctx, id) // first observation: the baseline
	if err != nil || res.RequeueAfter != DefaultResyncInterval {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	if n := len(e.events(v1alpha1.EventFactoryWorkItemResynced)); n != 0 {
		t.Fatalf("resynced events after the baseline = %d", n)
	}
	// A human labels the issue out of band; nothing tells the dispatcher.
	if err := e.gh.Update(docsRepo+"#42", func(o *provider.Observation) { o.Labels = append(o.Labels, "help-wanted") }); err != nil {
		t.Fatal(err)
	}
	// Not due yet: no observation.
	if res, _ := e.wic.Reconcile(e.ctx, id); res.RequeueAfter != DefaultResyncInterval {
		t.Fatalf("early reconcile = %+v", res)
	}
	e.clk.Advance(DefaultResyncInterval)
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	evs := e.events(v1alpha1.EventFactoryWorkItemResynced)
	if len(evs) != 1 || evs[0].WorkItem != id || evs[0].Source != v1alpha1.FactoryProvider {
		t.Fatalf("resynced events = %+v", evs)
	}
	// The cache was refreshed for the rule controller; the next resync sees no change.
	e.clk.Advance(DefaultResyncInterval)
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(v1alpha1.EventFactoryWorkItemResynced)); n != 1 {
		t.Errorf("resynced events = %d, want still 1", n)
	}
	if n := e.processEvents(); n != 1 {
		t.Errorf("processed %d events, want the resynced one", n)
	}
}

func TestWorkItemRetention(t *testing.T) {
	e := newEnv(t)
	mk := func(ref string, state provider.ObjectState) string {
		e.seed(provider.KindIssue, ref, func(o *provider.Observation) { o.State = state })
		wi := &v1alpha1.WorkItem{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
			Metadata: v1alpha1.ObjectMeta{Name: objectID("wi", ref)},
			Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: ghProvider, Ref: ref, Kind: provider.KindIssue, Primary: true}}},
		}
		if err := e.st.Create(e.ctx, wi); err != nil {
			t.Fatal(err)
		}
		return wi.Metadata.Name
	}
	closed := mk(docsRepo+"#1", provider.StateClosed)
	open := mk(docsRepo+"#2", provider.StateOpen)
	recent := mk(docsRepo+"#3", provider.StateClosed)
	// recent had activity a day ago.
	e.clk.Advance(store.DefaultRetention - 24*time.Hour)
	r := e.getWorkItem(recent)
	appendDecisions(r, []v1alpha1.Decision{{At: e.clk.Now(), Event: "x"}})
	if err := e.st.Update(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(24 * time.Hour)

	for _, id := range []string{closed, open, recent} {
		if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(id string) bool {
		return !isNotFoundErr(e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &v1alpha1.WorkItem{}))
	}
	if exists(closed) {
		t.Error("the closed, idle item was kept")
	}
	if !exists(open) {
		t.Error("the open item was deleted")
	}
	if !exists(recent) {
		t.Error("the recently active item was deleted")
	}
}

// TestResyncBaselinesAcrossLeaders: the baselines are stored, so a new leader (a new
// controller on the same store) reports a change made on GitHub while the last one led. Its
// first resync of an item with stored baselines is spread over the interval.
func TestResyncBaselinesAcrossLeaders(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	id := wi.Metadata.Name
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil { // the first leader's baseline
		t.Fatal(err)
	}
	var base LinkObservations
	if err := e.st.Get(e.ctx, observationsKey(id), &base); err != nil || len(base.Digests) != 1 {
		t.Fatalf("stored baselines = %+v, %v", base, err)
	}
	stored := base.Metadata.ResourceVersion
	// Unchanged: the next resync writes nothing.
	e.clk.Advance(DefaultResyncInterval)
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Get(e.ctx, observationsKey(id), &base); err != nil || base.Metadata.ResourceVersion != stored {
		t.Fatalf("baselines rewritten without a change: %s -> %s (%v)", stored, base.Metadata.ResourceVersion, err)
	}

	// The leader goes; a human labels the issue; another instance leads.
	if err := e.gh.Update(docsRepo+"#42", func(o *provider.Observation) { o.Labels = append(o.Labels, "help-wanted") }); err != nil {
		t.Fatal(err)
	}
	const delay = 3 * time.Minute
	next, err := NewWorkItemController(e.st, staticCatalog{e.cat}, e.conns, e.factory, WorkItemOptions{
		Clock: e.clk, Logger: discardLogger(), Cache: NewObservationCache(e.conns, e.clk, 0),
		Jitter: func(d time.Duration) time.Duration {
			if d != DefaultResyncInterval {
				t.Errorf("jitter over %s, want the resync interval", d)
			}
			return delay
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := next.Reconcile(e.ctx, id); err != nil || res.RequeueAfter != delay {
		t.Fatalf("the new leader's first reconcile = %+v, %v; want a resync in %s", res, err, delay)
	}
	if n := len(e.events(v1alpha1.EventFactoryWorkItemResynced)); n != 0 {
		t.Fatalf("resynced events before the jittered resync = %d", n)
	}
	e.clk.Advance(delay)
	if res, err := next.Reconcile(e.ctx, id); err != nil || res.RequeueAfter != DefaultResyncInterval {
		t.Fatalf("the new leader's first resync = %+v, %v", res, err)
	}
	if evs := e.events(v1alpha1.EventFactoryWorkItemResynced); len(evs) != 1 || evs[0].WorkItem != id {
		t.Fatalf("resynced events = %+v, want the change made while the other led", evs)
	}
}

// TestResyncBaselinesDeleted: a work item's baselines go with it, a removed link's entry goes
// at the next resync, and the sweep deletes the ones whose item is gone.
func TestResyncBaselinesDeleted(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	id := wi.Metadata.Name
	e.seed(provider.KindPR, docsRepo+"#57", nil)
	prRef := v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindPR, Ref: docsRepo + "#57"}
	if _, err := LinkObject(e.ctx, e.st, id, prRef, ghProvider); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	var base LinkObservations
	if err := e.st.Get(e.ctx, observationsKey(id), &base); err != nil || len(base.Digests) != 2 {
		t.Fatalf("baselines = %+v, %v; want two links", base.Digests, err)
	}
	// The PR's link is removed: its entry goes at the next resync.
	cur := e.getWorkItem(id)
	cur.Spec.Links = slices.DeleteFunc(cur.Spec.Links, func(l v1alpha1.Link) bool { return l.Ref == prRef.Ref })
	if err := e.st.Update(e.ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(DefaultResyncInterval)
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Get(e.ctx, observationsKey(id), &base); err != nil || len(base.Digests) != 1 || base.Digests[ghProvider+"/"+prRef.Ref] != "" {
		t.Fatalf("baselines after the link went = %+v, %v", base.Digests, err)
	}
	// The item is deleted: the next reconcile deletes its baselines.
	if err := e.st.Delete(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Get(e.ctx, observationsKey(id), &base); !isNotFoundErr(err) {
		t.Fatalf("baselines of a deleted item: %v", err)
	}
	// An orphan (a delete that failed) is swept; one whose item exists is kept.
	live := &v1alpha1.WorkItem{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem}, Metadata: v1alpha1.ObjectMeta{Name: "wi_live"}}
	if err := e.st.Create(e.ctx, live); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wi_gone", live.Metadata.Name} {
		if err := e.st.Create(e.ctx, &LinkObservations{
			TypeMeta: v1alpha1.TypeMeta{Kind: KindLinkObservations}, Metadata: v1alpha1.ObjectMeta{Name: name},
			Digests: map[string]string{"x": "y"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.wic.SweepObservations(e.ctx); err != nil || n != 1 {
		t.Fatalf("SweepObservations = %d, %v; want the orphan", n, err)
	}
	if err := e.st.Get(e.ctx, observationsKey(live.Metadata.Name), &base); err != nil {
		t.Fatalf("the sweep deleted a live item's baselines: %v", err)
	}
}

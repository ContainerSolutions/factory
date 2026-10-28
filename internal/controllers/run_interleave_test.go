package controllers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// hookStore runs a hook, once, before the first List of Runs or the first Lock, as another
// writer would land between a controller's read and its write.
type hookStore struct {
	store.Store
	mu         sync.Mutex
	beforeList func()
	beforeLock func()
}

func (s *hookStore) take(f *func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := *f
	*f = nil
	return g
}

func (s *hookStore) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	if kind == v1alpha1.KindRun {
		if f := s.take(&s.beforeList); f != nil {
			f()
		}
	}
	return s.Store.List(ctx, kind, opts, out)
}

func (s *hookStore) Lock(ctx context.Context, name string, ttl time.Duration) (store.Unlock, error) {
	if f := s.take(&s.beforeLock); f != nil {
		f()
	}
	return s.Store.Lock(ctx, name, ttl)
}

func newHookedRuns(t *testing.T, e *env) (*hookStore, *RunController) {
	t.Helper()
	hs := &hookStore{Store: e.st}
	rc, err := NewRunController(hs, staticCatalog{e.cat}, e.factory, RunOptions{Clock: e.clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return hs, rc
}

// TestRunNotClosedByAStaleReconcile: a reconcile reads the task Pending; before it looks for
// an open run, the task is leased and its run created. The run is the new lease's, not one
// the task left: it stays open, and the agent's complete ends it.
func TestRunNotClosedByAStaleReconcile(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan() // Pending
	id := plan.Metadata.ID
	hs, rc := newHookedRuns(t, e)
	hs.beforeList = func() { e.lease(id) } // the lease, then its run, land after the task was read
	if _, err := rc.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	runs := e.runs(id)
	if len(runs) != 1 || ended(runs[0]) {
		t.Fatalf("runs after the stale reconcile = %+v, want the new lease's run open", runs)
	}
	if phase, err := rc.Complete(e.ctx, id, doneResult("decomposed", planData)); err != nil || phase != v1alpha1.TaskSucceeded {
		t.Fatalf("Complete = %s, %v; want Succeeded", phase, err)
	}
}

// TestFinishAfterConcurrentWrites: between the complete's read of the run and the task and
// its locked finish, a heartbeat appends progress to the run and renews the task's lease (it
// takes no work item lock). The finish writes on fresh reads: it succeeds, and keeps the
// progress.
func TestFinishAfterConcurrentWrites(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	hs, rc := newHookedRuns(t, e)
	hs.beforeLock = func() {
		run := e.runs(id)[0]
		run.Status.Progress = appendProgress(run.Status.Progress, v1alpha1.ProgressEvent{At: e.clk.Now(), Kind: "step", Message: "a heartbeat"})
		if err := e.st.Update(e.ctx, run); err != nil {
			t.Error(err)
		}
		tk := e.task(id)
		tk.Status.Phase = v1alpha1.TaskRunning
		tk.Status.LeaseExpiresAt = e.clk.Now().Add(4 * time.Minute)
		if err := e.st.Update(e.ctx, tk); err != nil {
			t.Error(err)
		}
	}
	if phase, err := rc.Complete(e.ctx, id, doneResult("decomposed", planData)); err != nil || phase != v1alpha1.TaskSucceeded {
		t.Fatalf("Complete = %s, %v; want Succeeded", phase, err)
	}
	run := e.runs(id)[0]
	if run.Status.End != v1alpha1.RunCompleted || run.Status.Result == nil {
		t.Fatalf("run = %+v, want it completed with the Result", run.Status)
	}
	kept := false
	for _, p := range run.Status.Progress {
		kept = kept || p.Message == "a heartbeat"
	}
	if !kept {
		t.Error("the heartbeat's progress was lost")
	}
	if st := e.task(id).Status; st.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("task = %+v, want Succeeded", st)
	}
}

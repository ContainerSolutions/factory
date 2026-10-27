package controllers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

func TestCompleteInvalidResult(t *testing.T) {
	tests := []struct {
		name string
		res  v1alpha1.Result
		want string // in the progress message
	}{
		{"unknown outcome", doneResult("bogus", nil), `outcome "bogus" is not one of`},
		{"done without outcome", doneResult("", nil), "a done result needs an outcome"},
		{"schema", doneResult("decomposed", map[string]any{"plan": []any{map[string]any{"issue": "x"}}}), "schema schemas/results/plan-epic.json"},
		{"another task", func() v1alpha1.Result { r := doneResult("decomposed", nil); r.Task = "tsk_other"; return r }(), "result is for task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			wi, plan := e.startPlan()
			id := plan.Metadata.ID
			e.lease(id)
			phase, err := e.run.Complete(e.ctx, id, tt.res)
			if err != nil || phase != v1alpha1.TaskFailed {
				t.Fatalf("Complete = %s, %v; want Failed", phase, err)
			}
			if got := e.task(id).Status; got.Phase != v1alpha1.TaskFailed || got.Reason != ReasonInvalidResult {
				t.Errorf("task status = %+v", got)
			}
			runs := e.runs(id)
			if len(runs) != 1 || runs[0].Status.End != v1alpha1.RunFailed || runs[0].Status.Result != nil {
				t.Fatalf("run = %+v", runs)
			}
			p := runs[0].Status.Progress
			if len(p) == 0 || p[len(p)-1].Kind != ReasonInvalidResult || !strings.Contains(p[len(p)-1].Message, tt.want) {
				t.Errorf("progress = %+v, want %q", p, tt.want)
			}
			failed := e.events(v1alpha1.EventFactoryRunFailed)
			if len(failed) != 1 || runFact(failed[0], "reason") != ReasonInvalidResult || runFact(failed[0], "spec") != "plan-epic" ||
				runFact(failed[0], "outcome") != "" || failed[0].WorkItem != wi.Metadata.Name {
				t.Fatalf("run.failed = %+v", failed)
			}
			if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
				t.Errorf("counter = %d, want 1 (a failed run is a run)", n)
			}
			// Completing again is a replay.
			if phase, err := e.run.Complete(e.ctx, id, doneResult("decomposed", planData)); err != nil || phase != v1alpha1.TaskFailed {
				t.Errorf("replay = %s, %v", phase, err)
			}
			if n := len(e.events(v1alpha1.EventFactoryRunFailed)) + len(e.events(v1alpha1.EventFactoryRunCompleted)); n != 1 {
				t.Errorf("run events = %d, want 1", n)
			}
		})
	}
}

func TestCompleteEmitsRunFacts(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	e.lease(plan.Metadata.ID)
	if _, err := e.run.Complete(e.ctx, plan.Metadata.ID, doneResult("decomposed", planData)); err != nil {
		t.Fatal(err)
	}
	evs := e.events(v1alpha1.EventFactoryRunCompleted)
	if len(evs) != 1 {
		t.Fatalf("run.completed = %d", len(evs))
	}
	ev := evs[0]
	if ev.Source != v1alpha1.FactoryProvider || ev.Data.Provider != v1alpha1.FactoryProvider {
		t.Errorf("source = %s/%s", ev.Source, ev.Data.Provider)
	}
	for k, want := range map[string]any{"spec": "plan-epic", "outcome": "decomposed", "status": "done", "agent": agentID} {
		if got := runFact(ev, k); got != want {
			t.Errorf("run.%s = %v, want %v", k, got, want)
		}
	}
	result, _ := runFact(ev, "result").(map[string]any)
	if plan, _ := result["plan"].([]any); len(plan) != 2 { // data.plan mirrored as run.result.plan
		t.Errorf("run.result = %v", result)
	}
	if h, _ := runFact(ev, "harness").(map[string]any); h["name"] != "codex" || h["model"] != "gpt-5-codex" {
		t.Errorf("run.harness = %v", h)
	}
	if _, ok := runFact(ev, "inputs").(map[string]any); !ok {
		t.Errorf("run.inputs = %v", runFact(ev, "inputs"))
	}
}

func TestCompleteFailedStatus(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	res := doneResult("", nil)
	res.Status = v1alpha1.ResultFailed
	if phase, err := e.run.Complete(e.ctx, id, res); err != nil || phase != v1alpha1.TaskFailed {
		t.Fatalf("Complete = %s, %v; want Failed", phase, err)
	}
	// The agent reported: the run completed (with its Result), the task failed.
	runs := e.runs(id)
	if len(runs) != 1 || runs[0].Status.End != v1alpha1.RunCompleted || runs[0].Status.Result == nil || runs[0].Status.EndedAt.IsZero() {
		t.Fatalf("run = %+v", runs[0].Status)
	}
	failed := e.events(v1alpha1.EventFactoryRunFailed)
	if len(failed) != 1 || runFact(failed[0], "status") != "failed" || runFact(failed[0], "reason") != "failed" {
		t.Fatalf("run.failed = %+v", failed)
	}
	if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
		t.Errorf("counter = %d, want 1", n)
	}
}

func TestCompleteNeedsALease(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	if _, err := e.run.Complete(e.ctx, plan.Metadata.ID, doneResult("decomposed", planData)); !errors.Is(err, ErrNotLeased) {
		t.Errorf("Complete of a Pending task = %v, want ErrNotLeased", err)
	}
	if _, err := e.run.Complete(e.ctx, "tsk_nope", doneResult("decomposed", planData)); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("Complete of an unknown task = %v, want ErrUnknownTask", err)
	}
}

func TestWallClockTimeout(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	// plan-epic: maxWallClock 20m, plus the 2 min grace.
	res, err := e.run.Reconcile(e.ctx, id)
	if err != nil || res.RequeueAfter != 22*time.Minute {
		t.Fatalf("Reconcile = %+v, %v; want a requeue at the deadline", res, err)
	}
	e.clk.Advance(21 * time.Minute)
	if res, _ := e.run.Reconcile(e.ctx, id); res.RequeueAfter != time.Minute || e.task(id).Status.Phase != v1alpha1.TaskLeased {
		t.Fatalf("before the deadline: %+v, %s", res, e.task(id).Status.Phase)
	}
	e.clk.Advance(time.Minute)
	if _, err := e.run.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := e.task(id).Status; got.Phase != v1alpha1.TaskFailed || got.Reason != ReasonTimeout {
		t.Errorf("task = %+v, want Failed{timeout}", got)
	}
	failed := e.events(v1alpha1.EventFactoryRunFailed)
	if len(failed) != 1 || runFact(failed[0], "reason") != ReasonTimeout || runFact(failed[0], "status") != "failed" {
		t.Fatalf("run.failed = %+v", failed)
	}
	if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
		t.Errorf("counter = %d, want 1", n)
	}
}

func TestAbandon(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	id := plan.Metadata.ID

	// Throttled: back to Pending without using an attempt; the run doesn't count.
	e.lease(id)
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonThrottled, Until: e.clk.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	st := e.task(id).Status
	if st.Phase != v1alpha1.TaskPending || st.Attempts != 0 || st.Agent != nil || st.Reason != ReasonThrottled {
		t.Fatalf("after throttled abandon: %+v", st)
	}
	if runs := e.runs(id); len(runs) != 1 || runs[0].Status.End != v1alpha1.RunAbandoned {
		t.Fatalf("runs = %+v", runs)
	}
	if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 0 {
		t.Errorf("counter = %d, want 0", n)
	}

	// agent-error: the poc queue retries infra failures once (attempts 1), then fails.
	e.lease(id)
	if runs := e.runs(id); len(runs) != 2 || runs[1].Spec.Attempt != 1 {
		t.Fatalf("second lease runs = %+v", runs)
	}
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); err != nil {
		t.Fatal(err)
	}
	if st := e.task(id).Status; st.Phase != v1alpha1.TaskPending || st.Attempts != 1 || st.Reason != string(v1alpha1.RetryInfraFailure) {
		t.Fatalf("after a retried abandon: %+v", st)
	}
	e.lease(id)
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); err != nil {
		t.Fatal(err)
	}
	if st := e.task(id).Status; st.Phase != v1alpha1.TaskFailed || st.Reason != string(v1alpha1.RetryInfraFailure) {
		t.Fatalf("after the last abandon: %+v", st)
	}
	if n := len(e.events(v1alpha1.EventFactoryRunFailed)); n != 1 {
		t.Errorf("run.failed events = %d, want 1 (only the final failure)", n)
	}
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); !errors.Is(err, ErrNotLeased) {
		t.Errorf("abandon of a failed task = %v, want ErrNotLeased", err)
	}
}

func TestRunClosedWhenLeaseLost(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	// The scheduler expires the lease: the task is Pending again.
	tk := e.task(id)
	tk.Status.Phase, tk.Status.Agent, tk.Status.Attempts = v1alpha1.TaskPending, nil, 1
	if err := e.st.Update(e.ctx, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if runs := e.runs(id); len(runs) != 1 || runs[0].Status.End != v1alpha1.RunLost {
		t.Fatalf("runs = %+v", runs)
	}
	if n := len(e.events(v1alpha1.EventFactoryRunFailed)); n != 0 {
		t.Errorf("run.failed events = %d, want 0", n)
	}
}

// runEndStore is a store that calls onRunEnd once, right after the first write of a Run's end:
// inside a finish, between the run's write and the task's. Once it has fired, a Lock reports
// its name on locking (when there is room) before it waits.
type runEndStore struct {
	store.Store
	mu       sync.Mutex
	onRunEnd func()
	fired    bool
	locking  chan string
}

func (s *runEndStore) Update(ctx context.Context, obj store.Object) error {
	if err := s.Store.Update(ctx, obj); err != nil {
		return err
	}
	if run, ok := obj.(*v1alpha1.Run); ok && ended(run) {
		s.mu.Lock()
		f := s.onRunEnd
		s.onRunEnd, s.fired = nil, s.fired || f != nil
		s.mu.Unlock()
		if f != nil {
			f()
		}
	}
	return nil
}

func (s *runEndStore) Lock(ctx context.Context, name string, ttl time.Duration) (store.Unlock, error) {
	s.mu.Lock()
	fired := s.fired
	s.mu.Unlock()
	if fired {
		select {
		case s.locking <- name:
		default: // nobody waits for it
		}
	}
	return s.Store.Lock(ctx, name, ttl)
}

// TestOneRunPerAttempt: a run that ends while its task is reconciled gets no second run for
// its attempt (the ghost run of tsk_3e3cddca3a7663818730: no Result, a second run.failed),
// and an attempt after a requeue still gets its own.
func TestOneRunPerAttempt(t *testing.T) {
	failedResult := func() v1alpha1.Result {
		res := doneResult("", nil)
		res.Status, res.Summary = v1alpha1.ResultFailed, "the docs build is red"
		return res
	}
	tests := []struct {
		name string
		// drive leases the task and runs the scenario on rc, a run controller over s.
		drive func(t *testing.T, e *env, rc *RunController, s *runEndStore, id string)
		// The task's runs (ends, by name suffix and attempt), its end and the run.failed events.
		wantRuns   []v1alpha1.RunEnd
		wantNewest string
		wantPhase  v1alpha1.TaskPhase
		wantFailed int
	}{
		{
			name: "reconciled between the run's end and the task's phase",
			drive: func(t *testing.T, e *env, rc *RunController, s *runEndStore, id string) {
				e.lease(id)
				done := make(chan error, 1)
				var reconciled error // Reconcile's error, when it returned inside the finish
				returned := false
				s.mu.Lock()
				s.onRunEnd = func() {
					// The task is still Leased here; the run controller reconciles it (the Run
					// watch fired): it waits for the work item's lock the finish holds, or
					// (without that lock) returns having read the task between the writes.
					go func() {
						_, err := rc.Reconcile(e.ctx, id)
						done <- err
					}()
					select {
					case <-s.locking:
					case reconciled = <-done:
						returned = true
					case <-time.After(waitTimeout):
						t.Error("timed out waiting for Reconcile")
					}
				}
				s.mu.Unlock()
				if phase, err := rc.Complete(e.ctx, id, failedResult()); err != nil || phase != v1alpha1.TaskFailed {
					t.Fatalf("Complete = %s, %v; want Failed", phase, err)
				}
				if !returned {
					reconciled = recv(t, done, "Reconcile to return")
				}
				if reconciled != nil {
					t.Fatal(reconciled)
				}
				if _, err := rc.Reconcile(e.ctx, id); err != nil { // the task's watch
					t.Fatal(err)
				}
			},
			wantRuns: []v1alpha1.RunEnd{v1alpha1.RunCompleted}, wantNewest: "-1", wantPhase: v1alpha1.TaskFailed, wantFailed: 1,
		},
		{
			name: "the finish failed after the run's end",
			drive: func(t *testing.T, e *env, rc *RunController, s *runEndStore, id string) {
				e.lease(id)
				// The task's write lost to the lease's renewal: the run ended, the task is Leased.
				s.mu.Lock()
				s.onRunEnd = func() {
					tk := e.task(id)
					tk.Status.LeaseExpiresAt = e.clk.Now().Add(4 * time.Minute)
					if err := e.st.Update(e.ctx, tk); err != nil {
						t.Error(err)
					}
				}
				s.mu.Unlock()
				if _, err := rc.Complete(e.ctx, id, failedResult()); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("Complete = %v, want a conflict", err)
				}
				if res, err := rc.Reconcile(e.ctx, id); err != nil || res != (Result{}) {
					t.Fatalf("Reconcile = %+v, %v", res, err)
				}
				if st := e.task(id).Status; st.Phase != v1alpha1.TaskLeased {
					t.Fatalf("task = %+v, want still Leased", st)
				}
				// The agent retries its complete.
				if phase, err := rc.Complete(e.ctx, id, failedResult()); err != nil || phase != v1alpha1.TaskFailed {
					t.Fatalf("retried Complete = %s, %v; want Failed", phase, err)
				}
				if _, err := rc.Reconcile(e.ctx, id); err != nil {
					t.Fatal(err)
				}
			},
			wantRuns: []v1alpha1.RunEnd{v1alpha1.RunCompleted}, wantNewest: "-1", wantPhase: v1alpha1.TaskFailed, wantFailed: 1,
		},
		{
			name: "a retried attempt gets its own run",
			drive: func(t *testing.T, e *env, rc *RunController, _ *runEndStore, id string) {
				e.lease(id)
				if err := rc.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); err != nil {
					t.Fatal(err)
				}
				e.lease(id)
				if _, err := rc.Reconcile(e.ctx, id); err != nil {
					t.Fatal(err)
				}
				if phase, err := rc.Complete(e.ctx, id, failedResult()); err != nil || phase != v1alpha1.TaskFailed {
					t.Fatalf("Complete = %s, %v; want Failed", phase, err)
				}
			},
			wantRuns: []v1alpha1.RunEnd{v1alpha1.RunAbandoned, v1alpha1.RunCompleted}, wantNewest: "-2", wantPhase: v1alpha1.TaskFailed, wantFailed: 1,
		},
		{
			name: "a throttled requeue's lease gets a new run of the same attempt",
			drive: func(t *testing.T, e *env, rc *RunController, _ *runEndStore, id string) {
				e.lease(id)
				if err := rc.Abandon(e.ctx, id, Abandonment{Reason: AbandonThrottled, Until: e.clk.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
				e.lease(id)
				if _, err := rc.Reconcile(e.ctx, id); err != nil {
					t.Fatal(err)
				}
			},
			wantRuns: []v1alpha1.RunEnd{v1alpha1.RunAbandoned, ""}, wantNewest: "-2", wantPhase: v1alpha1.TaskLeased, wantFailed: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			s := &runEndStore{Store: e.st, locking: make(chan string, 1)}
			rc, err := NewRunController(s, staticCatalog{e.cat}, e.factory, RunOptions{Clock: e.clk, Logger: discardLogger()})
			if err != nil {
				t.Fatal(err)
			}
			e.run = rc // e.lease's LeaseIssued goes through s too
			wi, plan := e.startPlan()
			id := plan.Metadata.ID
			tt.drive(t, e, rc, s, id)

			runs := e.runs(id)
			if len(runs) != len(tt.wantRuns) {
				t.Fatalf("runs = %d (%+v), want %d", len(runs), runs, len(tt.wantRuns))
			}
			for i, r := range runs {
				var end v1alpha1.RunEnd
				if r.Status != nil {
					end = r.Status.End
				}
				if end != tt.wantRuns[i] {
					t.Errorf("run %s end = %q, want %q", r.Metadata.Name, end, tt.wantRuns[i])
				}
			}
			newest := runs[len(runs)-1]
			if !strings.HasSuffix(newest.Metadata.Name, tt.wantNewest) {
				t.Errorf("newest run = %s, want suffix %s", newest.Metadata.Name, tt.wantNewest)
			}
			st := e.task(id).Status
			if newest.Spec.Attempt != st.Attempts+1 {
				t.Errorf("newest run attempt = %d, task attempts = %d", newest.Spec.Attempt, st.Attempts)
			}
			if st.Phase != tt.wantPhase {
				t.Errorf("task = %+v, want %s", st, tt.wantPhase)
			}
			failed := e.events(v1alpha1.EventFactoryRunFailed)
			if len(failed) != tt.wantFailed {
				t.Fatalf("run.failed = %d (%+v), want %d", len(failed), failed, tt.wantFailed)
			}
			if tt.wantFailed == 0 {
				return
			}
			// The failure is the newest run's, with its Result.
			if newest.Status.Result == nil || newest.Status.Result.Summary != "the docs build is red" {
				t.Errorf("newest run result = %+v, want the agent's", newest.Status.Result)
			}
			if st.Reason != string(v1alpha1.ResultFailed) {
				t.Errorf("task reason = %q, want failed", st.Reason)
			}
			if id := failed[0].ID; id != FactoryEventPrefix+"run/"+newest.Metadata.Name {
				t.Errorf("run.failed id = %q, want %srun/%s", id, FactoryEventPrefix, newest.Metadata.Name)
			}
			if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
				t.Errorf("counter = %d, want 1", n)
			}
		})
	}
}

package scheduler

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/store"
)

const mintWarning = "git credential not minted; task left pending"

// backoffOf is the scheduler's mint backoff of a task (under its mutex).
func (e *env) backoffOf(task string) (mintBackoff, bool) {
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	b, ok := e.s.mintBackoff[task]
	return b, ok
}

// failMints makes the next n mints fail as a missing installation does.
func (e *env) failMints(n int) {
	for range n {
		e.gh.FailNext(fake.MethodMintGitCredential, errors.New("installation not found"))
	}
}

// TestMintBackoff: a task whose credentials fail to mint isn't tried again before its delay
// (30 s, doubled up to 10 min); each failed try warns once with the delay, a skip logs
// nothing, and the task keeps git-credential. A successful mint leases it and clears the
// backoff.
func TestMintBackoff(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fails  int
		delays []time.Duration
	}{
		{"one failure", 1, []time.Duration{30 * time.Second}},
		{"doubles", 3, []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}},
		{"capped at 10m", 7, []time.Duration{
			30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
			10 * time.Minute, 10 * time.Minute,
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
			e.task("tsk_a", "implement", nil)
			e.failMints(tt.fails)
			for i, delay := range tt.delays {
				if g := e.lease(a); g != nil {
					t.Fatalf("try %d: leased %s", i+1, g.Lease.Task)
				}
				if n := len(e.gh.Mints()); n != i+1 {
					t.Fatalf("try %d: %d mints, want %d", i+1, n, i+1)
				}
				warns := e.logs.find(slog.LevelWarn, mintWarning)
				if len(warns) != i+1 {
					t.Fatalf("try %d: %d warnings, want %d", i+1, len(warns), i+1)
				}
				if got := attr(warns[i], "retryIn"); got != delay.String() {
					t.Errorf("try %d: retryIn = %s, want %s", i+1, got, delay)
				}
				if b, ok := e.backoffOf("tsk_a"); !ok || b.delay != delay || !b.next.Equal(e.clk.Now().Add(delay)) {
					t.Fatalf("try %d: backoff = %+v, %v; want %s from now", i+1, b, ok, delay)
				}

				// Until the delay has passed: no mint, no log, and the reason stays.
				e.clk.Advance(delay - time.Second)
				if g := e.lease(a); g != nil {
					t.Fatalf("try %d: leased %s during the backoff", i+1, g.Lease.Task)
				}
				if err := e.s.Refresh(e.ctx); err != nil {
					t.Fatal(err)
				}
				if r := e.explain("tsk_a", a); r != ReasonGitCredential {
					t.Errorf("try %d: Explain during the backoff = %q, want git-credential", i+1, r)
				}
				if n := len(e.gh.Mints()); n != i+1 {
					t.Fatalf("try %d: %d mints during the backoff, want %d", i+1, n, i+1)
				}
				if n := len(e.logs.find(slog.LevelWarn, mintWarning)); n != i+1 {
					t.Fatalf("try %d: %d warnings during the backoff, want %d", i+1, n, i+1)
				}
				if got := e.getTask("tsk_a").Status; got.Phase != v1alpha1.TaskPending || got.Reason != ReasonGitCredential {
					t.Fatalf("try %d: task = %+v, want Pending git-credential", i+1, got)
				}
				e.clk.Advance(time.Second)
			}
			e.mustLease(a, "tsk_a")
			if n := len(e.gh.Mints()); n != tt.fails+1 {
				t.Errorf("%d mints, want %d failed and 1 good", n, tt.fails)
			}
			if b, ok := e.backoffOf("tsk_a"); ok {
				t.Errorf("backoff after the lease = %+v, want none", b)
			}
			if got := e.getTask("tsk_a").Status; got.Phase != v1alpha1.TaskLeased || got.Reason != "" {
				t.Errorf("task = %+v, want Leased with no reason", got)
			}
		})
	}
}

// TestMintBackoffOtherTasksLeased: while one task backs off, the others are still leased, and
// the tick keeps its git-credential reason even though another reason now applies.
func TestMintBackoffOtherTasksLeased(t *testing.T) {
	e := newEnv(t)
	e.cat.Pools[pocPool].Spec.Lease.MaxTasksPerAgent = 3
	e.cat.Queues[pocQueue].Spec.Concurrency = 5
	a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Queues: []string{pocQueue}, MaxConcurrent: 2})
	e.task("tsk_a", "implement", func(t *v1alpha1.Task) { t.Spec.Credentials.Git.Identity = "nobody" })
	e.task("tsk_b", "implement", nil)
	e.mustLease(a, "tsk_b") // tsk_a tried first and failed
	e.task("tsk_c", "implement", nil)
	e.mustLease(a, "tsk_c") // tsk_a backing off: skipped without a mint
	if n := len(e.gh.Mints()); n != 3 {
		t.Errorf("%d mints, want tsk_a's failed one and one each for tsk_b and tsk_c", n)
	}
	if err := e.s.Refresh(e.ctx); err != nil { // the agent is Busy now
		t.Fatal(err)
	}
	if got := e.getTask("tsk_a").Status; got.Phase != v1alpha1.TaskPending || got.Reason != ReasonGitCredential {
		t.Fatalf("task a = %+v, want Pending git-credential", got)
	}
	if n := len(e.logs.find(slog.LevelWarn, mintWarning)); n != 1 {
		t.Errorf("%d warnings, want 1", n)
	}
}

// TestMintBackoffDropped: the backoff goes when its task leaves Pending without a lease, and
// every backoff goes when the catalog changes (a fix applies at once).
func TestMintBackoffDropped(t *testing.T) {
	for _, tt := range []struct {
		name string
		act  func(e *env)
	}{
		{"cancelled", func(e *env) {
			if _, err := update(e.ctx, e.st, taskKey("tsk_a"), newTask, func(t *v1alpha1.Task) (bool, error) {
				t.Status.Phase = v1alpha1.TaskCancelled
				return true, nil
			}); err != nil {
				e.t.Fatal(err)
			}
			if err := e.s.Refresh(e.ctx); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"deleted", func(e *env) {
			if err := e.st.Delete(e.ctx, taskKey("tsk_a")); err != nil && !errors.Is(err, store.ErrNotFound) {
				e.t.Fatal(err)
			}
			if err := e.s.Refresh(e.ctx); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"catalog swapped", func(e *env) {
			c := *e.cat // a new catalog, as catalog-sync swaps in (or reloads on resync-catalog)
			e.cats.p.Store(&c)
			if err := e.s.Refresh(e.ctx); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
			e.task("tsk_a", "implement", nil)
			e.failMints(1)
			if g := e.lease(a); g != nil {
				t.Fatalf("leased %s", g.Lease.Task)
			}
			if _, ok := e.backoffOf("tsk_a"); !ok {
				t.Fatal("no backoff after a failed mint")
			}
			tt.act(e)
			if b, ok := e.backoffOf("tsk_a"); ok {
				t.Fatalf("backoff = %+v, want none", b)
			}
		})
	}
}

// TestMintBackoffCatalogSwapRetriesAtOnce: after a catalog swap a backing-off task is tried
// on the next poll, without waiting out its delay.
func TestMintBackoffCatalogSwapRetriesAtOnce(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	e.task("tsk_a", "implement", nil)
	e.failMints(1)
	if g := e.lease(a); g != nil {
		t.Fatalf("leased %s", g.Lease.Task)
	}
	if g := e.lease(a); g != nil {
		t.Fatalf("leased %s during the backoff", g.Lease.Task)
	}
	c := *e.cat
	e.cats.p.Store(&c)
	e.mustLease(a, "tsk_a")
}

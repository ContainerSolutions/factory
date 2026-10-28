package scheduler

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

// testSchedWait keeps the sched lock's waits short in these tests.
const testSchedWait = 100 * time.Millisecond

// twoInstances is two dispatchers' schedulers, each on its own object store over one bucket,
// sharing the catalog, the fake GitHub and the clock. Neither store syncs in the background,
// and the clock moves only when a test moves it, so each instance's reads stay as stale as
// the test makes them.
type twoInstances struct {
	clk    *clock.Fake
	sa, sb *objectstore.Store
	a, b   *env
}

func newTwoInstances(t *testing.T) *twoInstances {
	t.Helper()
	clk := clock.NewFake(t0)
	cat, errs := catalog.LoadDir(os.DirFS(pocCatalog), catalog.Options{})
	if cat == nil || catalog.HasErrors(errs) {
		t.Fatalf("load PoC catalog: %v", errs)
	}
	gh := fake.NewConn(*cat.Providers[ghProvider], fake.Options{Clock: clk})
	gh.SetOrgRole("compliance-framework", "admin", "gus", "ana", "bob")
	b := objectstore.NewFakeBucket(clk)
	open := func(id string) *objectstore.Store {
		opts := []objectstore.Option{objectstore.WithWriterID(id), objectstore.WithSyncInterval(time.Hour)}
		for kind, proto := range map[string]store.Object{
			KindLease: &Lease{}, KindAgentToken: &AgentToken{}, KindEnrollment: &Enrollment{}, KindAgentRefresh: &AgentRefresh{},
			controllers.KindTaskPrompt: &controllers.TaskPrompt{},
		} {
			opts = append(opts, objectstore.WithKind(kind, proto))
		}
		s, err := objectstore.Open(context.Background(), b, store.Options{Clock: clk, Logger: discardLogger()}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	sa, sb := open("ins_a"), open("ins_b")
	opts := Options{SchedWait: testSchedWait}
	return &twoInstances{
		clk: clk, sa: sa, sb: sb,
		a: newEnvOn(t, clk, sa, cat, gh, opts), b: newEnvOn(t, clk, sb, cat, gh, opts),
	}
}

// TestSchedLockTwoInstancesConcurrencyOne: two dispatchers, each behind on the other's writes,
// poll at once for two tasks of a queue with concurrency 1. Each pass matches against its own
// stale snapshot, but the commits re-check under the sched lock: exactly one task is leased.
func TestSchedLockTwoInstancesConcurrencyOne(t *testing.T) {
	ti := newTwoInstances(t)
	a, b := ti.a, ti.b
	a.cat.Queues[pocQueue].Spec.Concurrency = 1
	a.task("tsk_1", "implement", nil)
	a.task("tsk_2", "implement", nil)
	ana, _ := a.register("ana", "ana-mbp", lendQueues(pocQueue))
	bob, _ := a.register("bob", "bob-mbp", lendQueues(pocQueue))
	if err := ti.sb.Sync(b.ctx); err != nil { // b sees the agents and the tasks, then falls behind
		t.Fatal(err)
	}

	polls := []struct {
		e     *env
		agent string
	}{{a, ana}, {b, bob}}
	var wg sync.WaitGroup
	grants := make([]*Grant, 2)
	for i, poll := range polls {
		wg.Go(func() {
			g, err := poll.e.s.Lease(poll.e.ctx, poll.agent, 0)
			if err != nil {
				t.Errorf("lease for %s: %v", poll.agent, err)
			}
			grants[i] = g
		})
	}
	wg.Wait()
	leased := 0
	for _, g := range grants {
		if g != nil {
			leased++
		}
	}
	if leased != 1 {
		t.Fatalf("leased %d tasks (%s, %s) in a queue with concurrency 1", leased, grantTask(grants[0]), grantTask(grants[1]))
	}
	// Past the busy window (a pass that met the other's lock), both poll again, each still
	// behind: the one without a task matches against its stale snapshot and is refused by
	// the re-check under the lock.
	ti.clk.Advance(time.Second)
	for i, poll := range polls {
		if grants[i] != nil {
			continue
		}
		if g := poll.e.lease(poll.agent); g != nil {
			t.Fatalf("%s leased %s past the queue's concurrency of 1", poll.agent, g.Task.Metadata.ID)
		}
	}
	if err := ti.sa.Sync(a.ctx); err != nil {
		t.Fatal(err)
	}
	var tasks []*v1alpha1.Task
	if _, err := ti.sa.List(a.ctx, v1alpha1.KindTask, store.ListOptions{}, &tasks); err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, task := range tasks {
		if task.Status != nil && task.Status.Phase == v1alpha1.TaskLeased {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("%d tasks Leased in the store, want 1", active)
	}
}

// TestSchedLockDeadHolder: another instance took the sched lock and died inside its section.
// A pass mints once, waits for the lock once and ends without a grant; passes that start
// within SchedWait of that neither wait nor mint; once the lock's lease ends, the next commit
// goes ahead, and the task is leased once.
func TestSchedLockDeadHolder(t *testing.T) {
	ti := newTwoInstances(t)
	a, b := ti.a, ti.b
	a.task("tsk_1", "implement", nil)
	ana, _ := a.register("ana", "ana-mbp", lendQueues(pocQueue))
	if _, err := ti.sb.Lock(b.ctx, SchedLock, DefaultSchedTTL); err != nil { // b dies holding it
		t.Fatal(err)
	}
	if err := ti.sa.Sync(a.ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if g := a.lease(ana); g != nil {
		t.Fatalf("leased %s while a dead instance holds the sched lock", g.Task.Metadata.ID)
	}
	if waited := time.Since(start); waited < testSchedWait {
		t.Fatalf("the pass ended after %s, before its wait for the lock (%s)", waited, testSchedWait)
	}
	if n := len(a.gh.Mints()); n != 1 {
		t.Fatalf("%d mints in the first pass, want 1", n)
	}
	if busy := a.logs.find(slog.LevelInfo, "sched lock busy; the matching pass ends"); len(busy) != 1 {
		t.Fatalf("%d 'sched lock busy' logs, want 1", len(busy))
	}

	// Within SchedWait: no wait, no mint.
	start = time.Now()
	if g := a.lease(ana); g != nil {
		t.Fatalf("leased %s while the lock is known busy", g.Task.Metadata.ID)
	}
	if waited := time.Since(start); waited >= testSchedWait {
		t.Fatalf("a pass within the busy window waited %s for the lock", waited)
	}
	if n := len(a.gh.Mints()); n != 1 {
		t.Fatalf("%d mints after a pass within the busy window, want still 1", n)
	}

	// The dead holder's lease ends: the next commit takes the lock.
	ti.clk.Advance(DefaultSchedTTL + time.Second)
	a.mustLease(ana, "tsk_1")
	if g := a.lease(ana); g != nil {
		t.Fatalf("leased %s a second time", g.Task.Metadata.ID)
	}
	var leases []*Lease
	if _, err := ti.sa.List(a.ctx, KindLease, store.ListOptions{}, &leases); err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("%d leases recorded, want 1", len(leases))
	}
}

// TestSchedLockMemoryStore: on one dispatcher (the memory store) the sched lock is free and a
// commit takes it and gives it back.
func TestSchedLockMemoryStore(t *testing.T) {
	e := newEnv(t)
	e.task("tsk_1", "implement", nil)
	ana, _ := e.register("ana", "ana-mbp", lendQueues(pocQueue))
	e.mustLease(ana, "tsk_1")
	ctx, cancel := context.WithTimeout(e.ctx, 50*time.Millisecond)
	defer cancel()
	u, err := e.st.Lock(ctx, SchedLock, time.Second)
	if err != nil {
		t.Fatalf("the sched lock wasn't released after the commit: %v", err)
	}
	u.Unlock()
}

// TestSchedLeaderGatesRefresh: Refresh, which writes, runs on the leader only; the tick still
// wakes the parked polls elsewhere.
func TestSchedLeaderGatesRefresh(t *testing.T) {
	g := &switchGate{}
	clk := clock.NewFake(t0)
	cat, errs := catalog.LoadDir(os.DirFS(pocCatalog), catalog.Options{})
	if cat == nil || catalog.HasErrors(errs) {
		t.Fatalf("load PoC catalog: %v", errs)
	}
	gh := fake.NewConn(*cat.Providers[ghProvider], fake.Options{Clock: clk})
	gh.SetOrgRole("compliance-framework", "admin", "gus", "ana", "bob")
	e := newEnvOn(t, clk, memory.New(store.Options{Clock: clk, Logger: discardLogger()}), cat, gh, Options{Leader: g})
	e.task("tsk_1", "implement", nil)
	e.run()
	clk.BlockUntil(1)
	clk.Advance(DefaultTick)
	clk.BlockUntil(1)
	var task v1alpha1.Task
	if err := e.st.Get(e.ctx, taskKey("tsk_1"), &task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Reason != "" {
		t.Fatalf("a non-leader's tick refreshed the task's reason to %q", task.Status.Reason)
	}
	g.set(true)
	clk.Advance(DefaultTick)
	eventually(t, "the leader's Refresh to record the task's reason", func() bool {
		var task v1alpha1.Task
		return e.st.Get(e.ctx, taskKey("tsk_1"), &task) == nil && task.Status.Reason != ""
	})
}

// switchGate is a Gate a test opens and closes.
type switchGate struct {
	mu   sync.Mutex
	open bool
}

func (g *switchGate) set(open bool) {
	g.mu.Lock()
	g.open = open
	g.mu.Unlock()
}

func (g *switchGate) Held() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

func (g *switchGate) Wait(ctx context.Context) error {
	for !g.Held() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

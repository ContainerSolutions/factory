package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// racer makes the next create of a log entry lose: just before it, a queued function writes
// through another store (the hook passes the other store's own creates through).
type racer struct {
	mu      sync.Mutex
	pending []func()
	busy    atomic.Bool
}

// before queues f to run before the next log create.
func (r *racer) before(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, f)
}

func (r *racer) hook(req FakeRequest) Fault {
	if req.Op != FakeCreate || !strings.HasPrefix(req.Name, logPrefix) || !r.busy.CompareAndSwap(false, true) {
		return Pass
	}
	defer r.busy.Store(false)
	r.mu.Lock()
	var f func()
	if len(r.pending) > 0 {
		f, r.pending = r.pending[0], r.pending[1:]
	}
	r.mu.Unlock()
	if f != nil {
		f()
	}
	return Pass
}

// TestLostRaces: every kind of write that loses the race for its log entry syncs and runs
// again against the fresh state: an unrelated change lets it through at the next version; a
// change to the same object makes it the right error.
func TestLostRaces(t *testing.T) {
	type step struct {
		name string
		// setup prepares the object on a; other is what b writes first; write is a's write;
		// want is its error (nil: it goes through).
		setup func(t *testing.T, a, b *Store)
		other func(ctx context.Context, b *Store) error
		write func(ctx context.Context, a *Store) error
		want  error
	}
	var others atomic.Int32
	unrelated := func(ctx context.Context, b *Store) error {
		return b.Create(ctx, newRun(fmt.Sprintf("other-%d", others.Add(1))))
	}
	steps := []step{
		{name: "create/unrelated", other: unrelated,
			write: func(ctx context.Context, a *Store) error { return a.Create(ctx, newRun("r1")) }},
		{name: "create/same key", other: func(ctx context.Context, b *Store) error { return b.Create(ctx, newRun("r1")) },
			write: func(ctx context.Context, a *Store) error { return a.Create(ctx, newRun("r1")) }, want: store.ErrAlreadyExists},
		{name: "update/unrelated", setup: createR1, other: unrelated,
			write: func(ctx context.Context, a *Store) error { return bump(ctx, a, "r1") }},
		{name: "update/same object", setup: createR1, other: func(ctx context.Context, b *Store) error { return bump(ctx, b, "r1") },
			write: func(ctx context.Context, a *Store) error { return bump(ctx, a, "r1") }, want: store.ErrConflict},
		{name: "delete/unrelated", setup: createR1, other: unrelated,
			write: func(ctx context.Context, a *Store) error { return a.Delete(ctx, runKey("r1")) }},
		{name: "delete/same object", setup: createR1, other: func(ctx context.Context, b *Store) error { return b.Delete(ctx, runKey("r1")) },
			write: func(ctx context.Context, a *Store) error { return a.Delete(ctx, runKey("r1")) }, want: store.ErrNotFound},
		{name: "lock/unrelated", other: unrelated,
			write: func(ctx context.Context, a *Store) error {
				u, err := a.Lock(ctx, "l", time.Minute)
				if err == nil {
					u.Unlock()
				}
				return err
			}},
		{name: "transcript/unrelated", other: unrelated,
			write: func(ctx context.Context, a *Store) error { return a.PutTranscript(ctx, "r1", []byte("t")) }},
		{name: "sweep/unrelated", setup: func(t *testing.T, a, b *Store) {
			createR1(t, a, b)
			a.clk.(interface{ Advance(time.Duration) time.Time }).Advance(store.DefaultRetention)
		}, other: unrelated,
			write: func(ctx context.Context, a *Store) error {
				res, err := a.Sweep(ctx)
				if err == nil && res.Objects[v1alpha1.KindRun] != 1 {
					return fmt.Errorf("swept %v, want r1", res.Objects)
				}
				return err
			}},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			rc := &racer{}
			r.b.SetFaults(rc.hook)
			a, b := r.open(t, "A"), r.open(t, "B")
			ctx := t.Context()
			if tc.setup != nil {
				tc.setup(t, a, b)
			}
			var otherErr error
			rc.before(func() { otherErr = tc.other(ctx, b) })
			err := tc.write(ctx, a)
			must(t, otherErr)
			if tc.want == nil {
				must(t, err)
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("write = %v, want %v", err, tc.want)
			}
			assertSameState(t, r, a, b)
		})
	}
}

func createR1(t *testing.T, a, _ *Store) { t.Helper(); must(t, a.Create(t.Context(), newRun("r1"))) }

// bump reads run name on s (syncing when it isn't there) and updates it.
func bump(ctx context.Context, s *Store, name string) error {
	var r v1alpha1.Run
	if err := s.Get(ctx, runKey(name), &r); err != nil {
		return err
	}
	r.Spec.Attempt++
	return s.Update(ctx, &r)
}

// assertSameState checks that a, b and a fresh load hold the same objects at the same version
// as the log.
func assertSameState(t *testing.T, r *rig, stores ...*Store) {
	t.Helper()
	ctx := t.Context()
	fresh := r.open(t, "check")
	want, wantV := fresh.mem.Snapshot()
	for _, s := range stores {
		must(t, s.Sync(ctx))
		got, v := s.mem.Snapshot()
		if v != wantV || len(got) != len(want) {
			t.Fatalf("%s holds %d objects at %d, the log %d at %d", s.id, len(got), v, len(want), wantV)
		}
	}
}

// TestTooManyLostRaces: a write that loses MaxLostRaces races in a row fails with
// ErrContended and leaves memory as the log has it.
func TestTooManyLostRaces(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	var busy atomic.Bool
	races := 0
	r.b.SetFaults(func(req FakeRequest) Fault {
		if req.Op != FakeCreate || !strings.HasPrefix(req.Name, logPrefix) || !busy.CompareAndSwap(false, true) {
			return Pass
		}
		defer busy.Store(false)
		races++
		must(t, b.Create(ctx, newRun(fmt.Sprintf("b-%d", races)))) // takes the entry a is about to create
		return Pass
	})
	err := a.Create(ctx, newRun("a"))
	r.b.SetFaults(nil)
	if !errors.Is(err, ErrContended) || races != MaxLostRaces {
		t.Fatalf("Create after %d lost races = %v, want ErrContended after %d", races, err, MaxLostRaces)
	}
	assertSameState(t, r, a, b)
}

// TestCreateOutcomes: a log create whose outcome is unknown is read back and retried, so it is
// committed exactly once; an SDK retry that finds its own earlier attempt counts as committed
// (a BudgetCounter charged once, not ErrConflict).
func TestCreateOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults []Fault // per log create, in order
		want   error
	}{
		{name: "timed out, landed", faults: []Fault{LandedTimedOut}},
		{name: "timed out, didn't land", faults: []Fault{Fail}},
		{name: "SDK retry landed", faults: []Fault{LandedTaken}},
		{name: "failed twice", faults: []Fault{Fail, Fail}, want: ErrInjected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			s := r.open(t, "A")
			ctx := t.Context()
			c := newCounter("budget.global.all.2026-01-01")
			must(t, s.Create(ctx, c))
			before := len(r.b.Names(logPrefix))
			var i atomic.Int32
			r.b.SetFaults(func(req FakeRequest) Fault {
				if req.Op != FakeCreate || !strings.HasPrefix(req.Name, logPrefix) {
					return Pass
				}
				n := int(i.Add(1)) - 1
				if n < len(tc.faults) {
					return tc.faults[n]
				}
				return Pass
			})
			c.Status.Runs++
			err := s.Update(ctx, c)
			r.b.SetFaults(nil)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("Update = %v, want %v", err, tc.want)
				}
				if n := len(r.b.Names(logPrefix)); n != before {
					t.Fatalf("%d log entries after a failed write, want %d", n, before)
				}
				if c.Metadata.ResourceVersion != "1" {
					t.Fatalf("failed Update changed the object's rv to %s", c.Metadata.ResourceVersion)
				}
				must(t, bumpCounter(ctx, s, c.Metadata.Name)) // the store goes on
				return
			}
			must(t, err)
			if n := len(r.b.Names(logPrefix)); n != before+1 {
				t.Fatalf("%d log entries, want %d (committed once)", n, before+1)
			}
			var got v1alpha1.BudgetCounter
			must(t, s.Get(ctx, store.KeyOf(c), &got))
			if got.Status.Runs != 1 || got.Metadata.ResourceVersion != c.Metadata.ResourceVersion {
				t.Fatalf("counter %+v, want 1 run at %s", got.Status, c.Metadata.ResourceVersion)
			}
			assertSameState(t, r, s)
		})
	}
}

func bumpCounter(ctx context.Context, s *Store, name string) error {
	var c v1alpha1.BudgetCounter
	if err := s.Get(ctx, store.Key{Kind: v1alpha1.KindBudgetCounter, Name: name}, &c); err != nil {
		return err
	}
	c.Status.Runs++
	return s.Update(ctx, &c)
}

// TestUnlockFails: an Unlock whose write fails is logged at warn; the record stays until it
// expires, then anyone takes the lock and the sweeper deletes the dead record.
func TestUnlockFails(t *testing.T) {
	r := newRig()
	var logs bytes.Buffer
	r.opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := r.open(t, "A")
	ctx := t.Context()
	u, err := s.Lock(ctx, "wi/1", time.Minute)
	must(t, err)
	r.b.SetFaults(func(req FakeRequest) Fault {
		if req.Op == FakeCreate {
			return Fail
		}
		return Pass
	})
	u.Unlock()
	r.b.SetFaults(nil)
	if !strings.Contains(logs.String(), "unlock failed") {
		t.Fatalf("no warning for the failed unlock: %q", logs.String())
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = s.Lock(short, "wi/1", time.Minute)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock before the dead record expires = %v, want a deadline", err)
	}
	r.clk.Advance(time.Minute)
	res, err := s.Sweep(ctx)
	must(t, err)
	if res.Objects[KindStoreLock] != 1 {
		t.Fatalf("sweep = %v, want the dead lock record", res.Objects)
	}
	var locks []StoreLock
	_, err = s.List(ctx, KindStoreLock, store.ListOptions{}, &locks)
	must(t, err)
	if len(locks) != 0 {
		t.Fatalf("lock records after the sweep: %v", locks)
	}
	u2, err := s.Lock(ctx, "wi/1", time.Minute)
	must(t, err)
	u2.Unlock()
}

// TestSyncErrors: GET and LIST errors during a sync fail it, and the store doesn't write blind:
// a write that needs a reload fails, and Ready turns not-ready once the last good sync is
// older than MaxLag; once the bucket answers again it all recovers.
func TestSyncErrors(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	must(t, a.Create(ctx, newRun("r1")))

	fail := func(op FakeOp) {
		r.b.SetFaults(func(req FakeRequest) Fault {
			if req.Op == op {
				return Fail
			}
			return Pass
		})
	}
	fail(FakeGet)
	if err := b.Sync(ctx); !errors.Is(err, ErrInjected) {
		t.Fatalf("Sync with GETs failing = %v", err)
	}
	var got v1alpha1.Run
	if err := b.Get(ctx, runKey("r1"), &got); !errors.Is(err, ErrInjected) {
		t.Fatalf("Get that can't sync = %v, want the sync's error, not ErrNotFound", err)
	}
	must(t, b.Ready()) // within MaxLag

	r.clk.Advance(DefaultMaxLag + time.Second)
	if err := b.Ready(); err == nil || !strings.Contains(err.Error(), "maximum lag") {
		t.Fatalf("Ready past MaxLag = %v", err)
	}
	fail(FakeList)
	if err := b.Create(ctx, newRun("blind")); !errors.Is(err, ErrInjected) {
		t.Fatalf("a stale write with LISTs failing = %v, want the reload's error", err)
	}
	r.b.SetFaults(nil)
	must(t, b.Create(ctx, newRun("r2")))
	must(t, b.Ready())
	getRun(t, b, "r1")
	assertSameState(t, r, a, b)
}

// TestMaxLagReload: a writer paused past MaxLag reloads before writing. While the log it
// missed is still there it lists and applies it; once retention deleted entries it never saw
// (past SegmentRetention), it starts again from the newest snapshot, its watchers expire, and
// it never re-creates a deleted entry.
func TestMaxLagReload(t *testing.T) {
	r := newRig()
	a := r.open(t, "A")
	b := r.open(t, "B", WithCompactEvery(5))
	ctx := t.Context()
	must(t, a.Create(ctx, newRun("a1")))
	getRun(t, b, "a1")

	// Paused past MaxLag, the log intact: a lists and applies.
	for i := range 3 {
		must(t, b.Create(ctx, newRun(fmt.Sprintf("b%d", i))))
	}
	r.clk.Advance(DefaultMaxLag + time.Minute)
	lists := r.b.Calls(FakeList)
	must(t, a.Create(ctx, newRun("a2")))
	if r.b.Calls(FakeList) == lists {
		t.Fatal("a stale writer wrote without listing the log")
	}
	getRun(t, a, "b2")

	// Paused past the segment retention: b compacts and the old entries go.
	w, err := a.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
	must(t, err)
	before := a.mem.Version()
	for i := range 12 {
		must(t, b.Create(ctx, newRun(fmt.Sprintf("c%d", i))))
	}
	_, err = b.Sweep(ctx) // compacts
	must(t, err)
	r.clk.Advance(DefaultSegmentRetention + time.Hour)
	must(t, b.Create(ctx, newRun("late")))
	_, err = b.Sweep(ctx) // retention: entries covered by the snapshot and old enough go
	must(t, err)
	deleted := before + 1
	if _, ok := r.b.Objects()[logName(deleted)]; ok {
		t.Fatalf("entry %d survived retention", deleted)
	}

	must(t, a.Create(ctx, newRun("a3")))
	if _, ok := r.b.Objects()[logName(deleted)]; ok {
		t.Fatalf("the stale writer re-created deleted entry %d", deleted)
	}
	for _, n := range []string{"c0", "c11", "late", "a3"} {
		getRun(t, a, n)
	}
	select {
	case _, open := <-w.Events():
		for open {
			_, open = <-w.Events()
		}
		if !errors.Is(w.Err(), store.ErrWatchExpired) {
			t.Fatalf("watcher after a reload: %v, want ErrWatchExpired", w.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher not expired by the reload")
	}
	assertSameState(t, r, a, b)
}

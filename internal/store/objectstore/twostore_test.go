package objectstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
)

// rig is a fake bucket and a fake clock that several stores share.
type rig struct {
	b    *FakeBucket
	clk  *clock.Fake
	opts store.Options
}

func newRig() *rig {
	opts, clk := testOpts()
	return &rig{b: NewFakeBucket(clk), clk: clk, opts: opts}
}

// open opens a store on the rig's bucket with writer id id and no background sync, so the
// test decides when it syncs.
func (r *rig) open(t *testing.T, id string, options ...Option) *Store {
	t.Helper()
	return mustOpen(t, r.b, r.opts, append([]Option{WithWriterID(id), withoutBackground()}, options...)...)
}

func withoutBackground() Option { return func(c *config) { c.noBackground = true } }

// withSchema makes the store a release that writes schema and reads up to readable.
func withSchema(schema, readable int) Option {
	return func(c *config) { c.schema, c.readable = schema, readable }
}

func newRun(name string) *v1alpha1.Run {
	return &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.RunSpec{Task: "t1", Attempt: 1, Agent: "agent-1"},
	}
}

func newWorkItem(name string, links ...v1alpha1.Link) *v1alpha1.WorkItem {
	return &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.WorkItemSpec{Links: links},
	}
}

func newCounter(name string) *v1alpha1.BudgetCounter {
	return &v1alpha1.BudgetCounter{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter},
		Metadata: v1alpha1.ObjectMeta{Name: name},
	}
}

func runKey(name string) store.Key { return store.Key{Kind: v1alpha1.KindRun, Name: name} }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func getRun(t *testing.T, s *Store, name string) *v1alpha1.Run {
	t.Helper()
	var r v1alpha1.Run
	must(t, s.Get(t.Context(), runKey(name), &r))
	return &r
}

// TestTwoStoresCAS: updates of one object at one version from two stores at once: exactly one
// wins, the rest get ErrConflict, and both stores end on the winner's object.
func TestTwoStoresCAS(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	must(t, a.Create(ctx, newRun("r1")))
	base := getRun(t, b, "r1") // B finds it through the sync on a miss

	const writers = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	var other []error
	for i := range writers {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			mine := *base
			mine.Spec.Attempt = 100 + i
			err := s.Update(ctx, &mine)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, store.ErrConflict):
				conflicts++
			default:
				other = append(other, err)
			}
		})
	}
	wg.Wait()
	if wins != 1 || conflicts != writers-1 || len(other) > 0 {
		t.Fatalf("wins %d, conflicts %d, other %v; want 1 and %d", wins, conflicts, other, writers-1)
	}
	must(t, a.Sync(ctx))
	must(t, b.Sync(ctx))
	ra, rb := getRun(t, a, "r1"), getRun(t, b, "r1")
	if ra.Spec.Attempt != rb.Spec.Attempt || ra.Metadata.ResourceVersion != rb.Metadata.ResourceVersion || ra.Spec.Attempt < 100 {
		t.Fatalf("A has %d@%s, B has %d@%s", ra.Spec.Attempt, ra.Metadata.ResourceVersion, rb.Spec.Attempt, rb.Metadata.ResourceVersion)
	}
}

// TestTwoStoresCreate: creates of one key from two stores at once: one wins, the other gets
// ErrAlreadyExists.
func TestTwoStoresCreate(t *testing.T) {
	for i := range 20 {
		r := newRig()
		a, b := r.open(t, "A"), r.open(t, "B")
		ctx := t.Context()
		name := fmt.Sprintf("r%d", i)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j, s := range []*Store{a, b} {
			wg.Go(func() { errs[j] = s.Create(ctx, newRun(name)) })
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, store.ErrAlreadyExists):
				t.Fatalf("Create = %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("%d creates succeeded (%v), want 1", ok, errs)
		}
		if n := len(r.b.Names(logPrefix)); n != 1 {
			t.Fatalf("%d log entries, want 1", n)
		}
	}
}

// TestTwoStoresWatchOrder: a watcher on B sees A's and B's writes in version order once B
// syncs.
func TestTwoStoresWatchOrder(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	w, err := b.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
	must(t, err)
	defer w.Stop()

	r1 := newRun("r1")
	must(t, a.Create(ctx, r1))
	r1.Spec.Attempt = 2
	must(t, a.Update(ctx, r1))
	must(t, b.Create(ctx, newRun("r2"))) // a lost race: B syncs A's two first
	must(t, a.Delete(ctx, runKey("r1")))
	must(t, b.Sync(ctx))

	want := []struct {
		typ  store.WatchEventType
		name string
	}{{store.Added, "r1"}, {store.Modified, "r1"}, {store.Added, "r2"}, {store.Deleted, "r1"}}
	var last uint64
	for i, wt := range want {
		select {
		case ev := <-w.Events():
			v, err := store.ParseVersion("", store.Key{}, ev.Version)
			must(t, err)
			if ev.Type != wt.typ || ev.Object.GetName() != wt.name || v <= last {
				t.Fatalf("event %d = %s %s@%d, want %s %s after %d", i, ev.Type, ev.Object.GetName(), v, wt.typ, wt.name, last)
			}
			last = v
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d missing", i)
		}
	}
}

// TestTwoStoresLocks: a lock held on A excludes B; A's Unlock frees it for B (B sees it on its
// one-second sync); and a holder that dies leaves a lock B takes over once it has expired.
func TestTwoStoresLocks(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()

	ua, err := a.Lock(ctx, "wi/1", time.Minute)
	must(t, err)
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = b.Lock(short, "wi/1", time.Minute)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("B's Lock of A's lock = %v, want a deadline", err)
	}

	got := make(chan error, 1)
	go func() {
		u, err := b.Lock(ctx, "wi/1", time.Minute)
		if err == nil {
			defer u.Unlock()
		}
		got <- err
	}()
	r.clk.BlockUntil(2) // B waits on the expiry and its one-second sync
	ua.Unlock()
	select {
	case err := <-got:
		t.Fatalf("B took the lock without syncing: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	r.clk.Advance(time.Second)
	select {
	case err := <-got:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("B not woken by its sync after A's Unlock")
	}

	// A holder that dies: its lock expires, and B reclaims it.
	dead, err := a.Lock(ctx, "wi/2", time.Minute)
	must(t, err)
	r.clk.Advance(time.Minute)
	ub, err := b.Lock(ctx, "wi/2", time.Minute)
	must(t, err)
	if err := dead.Renew(ctx, time.Minute); !errors.Is(err, store.ErrLockLost) {
		t.Fatalf("dead holder's Renew = %v, want ErrLockLost", err)
	}
	dead.Unlock() // never removes B's
	short, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = a.Lock(short, "wi/2", time.Minute)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("A's Lock after B reclaimed = %v, want a deadline", err)
	}
	must(t, ub.Renew(ctx, time.Minute))
	ub.Unlock()
}

// TestTwoStoresSyncFindsLinked: an object linked on A is found by B's lookup once B syncs
// (store.SyncIfSupported), as the intake filter and workItemFor do before acting on an empty
// answer; and a Get on B finds it without one.
func TestTwoStoresSyncFindsLinked(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	link := v1alpha1.Link{Provider: "github-public", Ref: "o/r#7"}
	byLink := store.ListOptions{Field: map[string]string{store.FieldWorkItemLink: "github-public/o/r#7"}}
	var items []v1alpha1.WorkItem
	_, err := b.List(ctx, v1alpha1.KindWorkItem, byLink, &items)
	must(t, err) // B is current now: an empty bucket

	must(t, a.Create(ctx, newWorkItem("wi-1", link)))
	_, err = b.List(ctx, v1alpha1.KindWorkItem, byLink, &items)
	must(t, err)
	if len(items) != 0 {
		t.Fatalf("B saw A's write within the sync interval without a sync: %v", items)
	}
	must(t, store.SyncIfSupported(ctx, b))
	_, err = b.List(ctx, v1alpha1.KindWorkItem, byLink, &items)
	must(t, err)
	if len(items) != 1 || items[0].Metadata.Name != "wi-1" {
		t.Fatalf("B after Sync = %v, want wi-1", items)
	}

	// A heartbeat on B of what A leased: B's Get syncs on a miss, its update wins, and A's
	// next update of what it read before is a conflict.
	must(t, a.Create(ctx, newRun("r1")))
	onA := getRun(t, a, "r1")
	onB := getRun(t, b, "r1")
	onB.Spec.Attempt = 2
	must(t, b.Update(ctx, onB))
	onA.Spec.Attempt = 3
	if err := a.Update(ctx, onA); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("A's update of a stale read = %v, want ErrConflict", err)
	}
	if got := getRun(t, a, "r1"); got.Spec.Attempt != 2 {
		t.Fatalf("A reads attempt %d, want B's 2", got.Spec.Attempt)
	}

	// Writes are checked against the log's tail: B deletes, and A creates again, objects the
	// other wrote within the sync interval, without a Sync.
	must(t, a.Create(ctx, newRun("r2")))
	must(t, b.Delete(ctx, runKey("r2")))
	must(t, a.Create(ctx, newRun("r2")))
}

// TestTwoReleasesNewerSchema: an entry written by a newer schema than B reads (a deploy that
// skipped the expand step) stops B writing, with nothing lost: B's writes fail with
// ErrReadOnly and create no entry, B's reads go on, B isn't Ready, and a release that reads
// the schema loads everything.
func TestTwoReleasesNewerSchema(t *testing.T) {
	r := newRig()
	b := r.open(t, "B")
	ctx := t.Context()
	must(t, b.Create(ctx, newRun("old")))
	a := r.open(t, "A", withSchema(store.SchemaVersion+2, store.SchemaVersion+2))
	must(t, a.Create(ctx, newRun("new")))
	entries := r.b.Names(logPrefix)

	err := b.Create(ctx, newRun("mine"))
	if !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("B's write after a newer entry = %v, want ErrReadOnly", err)
	}
	if err := b.Ready(); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("B Ready = %v, want ErrReadOnly", err)
	}
	if err := b.Sync(ctx); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("B Sync = %v, want ErrReadOnly", err)
	}
	if _, err := b.Lock(ctx, "l", time.Minute); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("B Lock = %v, want ErrReadOnly", err)
	}
	if err := b.PutTranscript(ctx, "old", []byte("x")); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("B PutTranscript = %v, want ErrReadOnly", err)
	}
	getRun(t, b, "old") // reads go on
	var r2 v1alpha1.Run
	if err := b.Get(ctx, runKey("new"), &r2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B Get of the newer entry's object = %v, want ErrNotFound (it can't read it)", err)
	}
	if got := r.b.Names(logPrefix); len(got) != len(entries) {
		t.Fatalf("log after B's refused writes = %v, want %v", got, entries)
	}
	must(t, a.Create(ctx, newRun("after")))

	c := r.open(t, "C", withSchema(store.SchemaVersion+2, store.SchemaVersion+2))
	for _, n := range []string{"old", "new", "after"} {
		getRun(t, c, n)
	}
	if err := c.Ready(); err != nil {
		t.Fatalf("a release that reads the schema: Ready = %v", err)
	}

	// A store that starts on such a log loads what it can read and doesn't write.
	d := r.open(t, "D")
	if li := d.Loaded(); !li.ReadOnly || li.Version != 1 {
		t.Fatalf("D loaded %+v, want read-only at version 1", li)
	}
	if err := d.Create(ctx, newRun("d")); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("D's write = %v, want ErrReadOnly", err)
	}
}

// TestTwoReleasesRollback: within ReadableSchema, two releases share one log (a deploy, or a
// rollback from N+1 to N): release N reads N+1's entries, keeps writing, and keeps the fields
// N+1 wrote; N+1 reads N's.
func TestTwoReleasesRollback(t *testing.T) {
	r := newRig()
	n := store.SchemaVersion
	newer := r.open(t, "new", withSchema(n+1, n+1))
	older := r.open(t, "old", withSchema(n, n+1)) // release N: expanded, still writes N
	ctx := t.Context()

	wi := newWorkItem("wi-1", v1alpha1.Link{Provider: "p", Ref: "o/r#1"})
	wi.Status = &v1alpha1.WorkItemStatus{State: map[string]any{"phase": "triage"}} // "the new field"
	must(t, newer.Create(ctx, wi))

	var got v1alpha1.WorkItem
	must(t, older.Get(ctx, store.KeyOf(wi), &got))
	got.Metadata.Labels = map[string]string{"by": "old"}
	must(t, older.Update(ctx, &got))
	if err := older.Ready(); err != nil {
		t.Fatalf("release N not Ready on N+1's log: %v", err)
	}

	must(t, newer.Sync(ctx))
	var back v1alpha1.WorkItem
	must(t, newer.Get(ctx, store.KeyOf(wi), &back))
	if back.Metadata.Labels["by"] != "old" || back.Status == nil || back.Status.State["phase"] != "triage" {
		t.Fatalf("after N's update, N+1 reads %+v / %+v: want N's label and N+1's field", back.Metadata, back.Status)
	}
	must(t, older.Compact(ctx))
	fresh := r.open(t, "fresh", withSchema(n, n+1))
	if li := fresh.Loaded(); li.ReadOnly || li.Snapshot == 0 {
		t.Fatalf("release N on a snapshot it reads: %+v", li)
	}
}

// TestNewerSnapshotRefusesStart: a snapshot past ReadableSchema refuses Open, naming both
// versions.
func TestNewerSnapshotRefusesStart(t *testing.T) {
	r := newRig()
	a := r.open(t, "A", withSchema(store.SchemaVersion+2, store.SchemaVersion+2))
	must(t, a.Create(t.Context(), newRun("r1")))
	must(t, a.Compact(t.Context()))
	_, err := Open(t.Context(), r.b, r.opts, WithWriterID("B"), withoutBackground())
	want := fmt.Sprintf("schema %d and this release reads up to schema %d", store.SchemaVersion+2, store.ReadableSchema)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Open on a newer snapshot = %v, want ErrSchema saying %q", err, want)
	}
}

// TestTwoStoresDeleteIfUnchanged: a store behind another deletes at the version it read, so an
// object the other deleted and created again under the same name (a redelivered Delivery)
// survives: the stale delete syncs and answers ErrConflict.
func TestTwoStoresDeleteIfUnchanged(t *testing.T) {
	ctx := t.Context()
	r := newRig()
	a, b := r.open(t, "ins_a"), r.open(t, "ins_b")
	must(t, a.Create(ctx, newRun("run_1")))
	must(t, b.Sync(ctx))
	stale := getRun(t, b, "run_1") // what b's reconcile read
	must(t, a.Delete(ctx, runKey("run_1")))
	again := newRun("run_1")
	again.Spec.Agent = "agent-2"
	must(t, a.Create(ctx, again))

	if err := b.DeleteIfUnchanged(ctx, runKey("run_1"), stale.Metadata.ResourceVersion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a stale delete: %v, want ErrConflict", err)
	}
	must(t, a.Sync(ctx))
	if got := getRun(t, a, "run_1"); got.Spec.Agent != "agent-2" {
		t.Fatalf("the recreated run: %+v, want it kept", got.Spec)
	}
}

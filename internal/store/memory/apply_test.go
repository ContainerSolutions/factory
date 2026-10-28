package memory_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

var runType = reflect.TypeFor[v1alpha1.Run]()

// changesOf records what a hooked store commits, to apply them to another store.
func changesOf(t *testing.T, write func(s *memory.Store)) []memory.Change {
	t.Helper()
	var out []memory.Change
	src := memory.New(opts(), memory.WithCommitHook(func(_ context.Context, c memory.Change) error {
		out = append(out, c)
		return nil
	}))
	write(src)
	return out
}

func nextEvent(t *testing.T, w store.Watcher) store.WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if !ok {
			t.Fatalf("watch ended: %v", w.Err())
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return store.WatchEvent{}
}

// TestApply: changes committed elsewhere apply in order, as if written here: objects, indexes,
// versions and watch events; versions already here are skipped and a gap is an error.
func TestApply(t *testing.T) {
	ctx := t.Context()
	changes := changesOf(t, func(s *memory.Store) {
		r := run("r1")
		r.Metadata.Labels = map[string]string{"team": "a"}
		must(t, s.Create(ctx, r))
		r.Spec.Attempt = 2
		must(t, s.Update(ctx, r))
		must(t, s.Create(ctx, run("r2")))
		must(t, s.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "r1"}))
	})
	hooked := 0
	dst := memory.New(opts(), memory.WithCommitHook(func(context.Context, memory.Change) error {
		hooked++
		return nil
	}))
	w, err := dst.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
	must(t, err)
	defer w.Stop()
	for _, c := range changes {
		typ := runType
		if c.Op == memory.OpDelete {
			typ = nil
		}
		applied, err := dst.Apply(c, typ)
		must(t, err)
		if !applied {
			t.Fatalf("change %d not applied", c.Version)
		}
	}
	for i, want := range []struct {
		typ  store.WatchEventType
		name string
	}{{store.Added, "r1"}, {store.Modified, "r1"}, {store.Added, "r2"}, {store.Deleted, "r1"}} {
		ev := nextEvent(t, w)
		if ev.Type != want.typ || ev.Object.GetName() != want.name || ev.Version != store.FormatVersion(uint64(i+1)) {
			t.Fatalf("event %d = %s %s@%s", i, ev.Type, ev.Object.GetName(), ev.Version)
		}
	}
	if dst.Version() != 4 || hooked != 0 {
		t.Fatalf("version %d, want 4; the hook saw %d changes, want none", dst.Version(), hooked)
	}
	var runs []v1alpha1.Run
	if _, err := dst.List(ctx, v1alpha1.KindRun, store.ListOptions{Field: map[string]string{store.FieldRunTask: "t1"}}, &runs); err != nil || len(runs) != 1 || runs[0].Metadata.Name != "r2" {
		t.Fatalf("index after apply = %v, %v", runs, err)
	}
	if applied, err := dst.Apply(changes[0], runType); err != nil || applied {
		t.Fatalf("re-applying version 1 = %v, %v; want skipped", applied, err)
	}
	for _, tc := range []struct {
		name string
		c    memory.Change
		typ  reflect.Type
		want string
	}{
		{"gap", memory.Change{Op: memory.OpCreate, Key: store.Key{Kind: v1alpha1.KindRun, Name: "x"}, Version: 6}, runType, "missing"},
		{"create existing", withVersion(changes[2], 5), runType, "exists"},
		{"update missing", withVersion(changes[1], 5), runType, "doesn't exist"},
		{"delete missing", withVersion(changes[3], 5), nil, "doesn't exist"},
		{"bad data", memory.Change{Op: memory.OpCreate, Key: store.Key{Kind: v1alpha1.KindRun, Name: "x"}, Version: 5, Data: []byte("{")}, runType, "decode"},
		{"unknown op", memory.Change{Op: "rename", Key: store.Key{Kind: v1alpha1.KindRun, Name: "x"}, Version: 5}, runType, "unknown op"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := dst.Apply(tc.c, tc.typ); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Apply = %v, want %q", err, tc.want)
			}
			if dst.Version() != 4 {
				t.Fatalf("a refused change moved the version to %d", dst.Version())
			}
		})
	}
	// The store writes on from there.
	r := run("r3")
	must(t, dst.Create(ctx, r))
	if r.Metadata.ResourceVersion != "5" {
		t.Fatalf("next write at %s, want 5", r.Metadata.ResourceVersion)
	}
}

// withVersion moves c to version v (the checks below fail before its data is decoded).
func withVersion(c memory.Change, v uint64) memory.Change {
	c.Version = v
	return c
}

// TestDeleteAt deletes only at the given version, through the hook.
func TestDeleteAt(t *testing.T) {
	ctx := t.Context()
	var hooked []memory.Op
	s := memory.New(opts(), memory.WithCommitHook(func(_ context.Context, c memory.Change) error {
		hooked = append(hooked, c.Op)
		return nil
	}))
	r := run("r1")
	must(t, s.Create(ctx, r))
	k := store.KeyOf(r)
	if err := s.DeleteAt(ctx, k, 7); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteAt a stale version = %v, want ErrConflict", err)
	}
	if err := s.DeleteAt(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "nope"}, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteAt nothing = %v", err)
	}
	if err := s.DeleteAt(ctx, store.Key{}, 1); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("DeleteAt no key = %v", err)
	}
	must(t, s.DeleteAt(ctx, k, 1))
	if len(hooked) != 2 || hooked[1] != memory.OpDelete {
		t.Fatalf("hook saw %v", hooked)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.DeleteAt(cancelled, k, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteAt cancelled = %v", err)
	}
}

// TestReplace swaps the state, expires every watcher and starts the history at the new
// version.
func TestReplace(t *testing.T) {
	ctx := t.Context()
	src := memory.New(opts())
	must(t, src.Create(ctx, run("a")))
	must(t, src.Create(ctx, run("b")))
	recs, v := src.Snapshot()

	s := memory.New(opts())
	must(t, s.Create(ctx, run("old")))
	w, err := s.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
	must(t, err)
	if err := s.Replace(recs, 0); err == nil {
		t.Fatal("Replace below the store's version succeeded")
	}
	must(t, s.Replace(recs, v+3))
	for range w.Events() {
	}
	if !errors.Is(w.Err(), store.ErrWatchExpired) {
		t.Fatalf("watcher after Replace: %v", w.Err())
	}
	var got v1alpha1.Run
	if err := s.Get(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "old"}, &got); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get old = %v", err)
	}
	must(t, s.Get(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "b"}, &got))
	if _, err := s.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "1"}); !errors.Is(err, store.ErrWatchExpired) {
		t.Fatalf("Watch from before the replace = %v", err)
	}
	r := run("c")
	must(t, s.Create(ctx, r))
	if r.Metadata.ResourceVersion != store.FormatVersion(v+4) {
		t.Fatalf("next write at %s, want %d", r.Metadata.ResourceVersion, v+4)
	}
	if err := s.Replace([]memory.Record{recs[0], recs[0]}, v+10); err == nil {
		t.Fatal("Replace with a duplicate succeeded")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

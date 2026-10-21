package memory_test

import (
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
	"github.com/compliance-framework/factory/internal/store/storetest"
)

// The conformance suite also passes with a hook that accepts everything.
func TestConformanceWithHook(t *testing.T) {
	storetest.Run(t, func(o store.Options) storetest.Subject {
		return memory.New(o, memory.WithCommitHook(func(memory.Change) error { return nil }))
	})
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func opts() store.Options {
	return store.Options{Clock: clock.NewFake(epoch), Logger: slog.New(slog.DiscardHandler)}
}

func run(name string) *v1alpha1.Run {
	return &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.RunSpec{Task: "t1"},
	}
}

func TestCommitHookSeesEveryWrite(t *testing.T) {
	ctx := t.Context()
	var got []memory.Change
	s := memory.New(opts(), memory.WithCommitHook(func(c memory.Change) error {
		got = append(got, c)
		return nil
	}))
	r := run("r1")
	if err := s.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Spec.Attempt = 2
	if err := s.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.KeyOf(r)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, "not-journaled", time.Minute); err != nil {
		t.Fatal(err)
	}
	k := store.Key{Kind: v1alpha1.KindRun, Name: "r1"}
	for i, want := range []struct {
		op      memory.Op
		version uint64
		data    string
	}{{memory.OpCreate, 1, `"resourceVersion":"1"`}, {memory.OpUpdate, 2, `"attempt":2`}, {memory.OpDelete, 3, ""}} {
		if i >= len(got) {
			t.Fatalf("hook saw %d changes, want 3", len(got))
		}
		c := got[i]
		if c.Op != want.op || c.Key != k || c.Version != want.version || !c.At.Equal(epoch) {
			t.Fatalf("change %d = %+v", i, c)
		}
		if want.data == "" && c.Data != nil || !strings.Contains(string(c.Data), want.data) {
			t.Fatalf("change %d data = %s, want it to contain %s", i, c.Data, want.data)
		}
	}
	if len(got) != 3 {
		t.Fatalf("hook saw %d changes, want 3", len(got))
	}
}

func TestCommitHookFailureLeavesNoTrace(t *testing.T) {
	boom := errors.New("disk on fire")
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, s *memory.Store, fail *bool) error
		check func(t *testing.T, s *memory.Store)
	}{
		{
			name: "create",
			write: func(t *testing.T, s *memory.Store, fail *bool) error {
				*fail = true
				r := run("new")
				r.Metadata.ResourceVersion, r.Metadata.CreatedAt = "keep", epoch.Add(-time.Hour)
				err := s.Create(t.Context(), r)
				if r.Metadata.ResourceVersion != "keep" || !r.Metadata.CreatedAt.Equal(epoch.Add(-time.Hour)) {
					t.Fatalf("failed Create changed the object's metadata: %+v", r.Metadata)
				}
				return err
			},
			check: func(t *testing.T, s *memory.Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "new"}, &got); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("Get = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "update",
			write: func(t *testing.T, s *memory.Store, fail *bool) error {
				var r v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &r); err != nil {
					t.Fatal(err)
				}
				*fail = true
				r.Spec.Attempt = 9
				err := s.Update(t.Context(), &r)
				if r.Metadata.ResourceVersion != "1" {
					t.Fatalf("failed Update changed the object's rv to %s", r.Metadata.ResourceVersion)
				}
				return err
			},
			check: func(t *testing.T, s *memory.Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &got); err != nil || got.Spec.Attempt != 0 {
					t.Fatalf("Get = %+v, %v", got.Spec, err)
				}
			},
		},
		{
			name: "delete",
			write: func(t *testing.T, s *memory.Store, fail *bool) error {
				*fail = true
				return s.Delete(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"})
			},
			check: func(t *testing.T, s *memory.Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &got); err != nil {
					t.Fatalf("Get = %v", err)
				}
			},
		},
		{
			name: "sweep",
			write: func(t *testing.T, s *memory.Store, fail *bool) error {
				*fail = true
				_, err := s.Sweep(t.Context())
				return err
			},
			check: func(t *testing.T, s *memory.Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &got); err != nil {
					t.Fatalf("Get = %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			fail := false
			o := opts()
			s := memory.New(o, memory.WithCommitHook(func(memory.Change) error {
				if fail {
					return boom
				}
				return nil
			}))
			if err := s.Create(ctx, run("r1")); err != nil {
				t.Fatal(err)
			}
			o.Clock.(*clock.Fake).Advance(store.DefaultRetention) // r1 is sweepable
			w, err := s.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Stop()

			if err := tc.write(t, s, &fail); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the hook's", err)
			}
			tc.check(t, s)
			var list []v1alpha1.Run
			meta, err := s.List(ctx, v1alpha1.KindRun, store.ListOptions{}, &list)
			if err != nil || meta.ResourceVersion != "1" {
				t.Fatalf("store version %q after a failed write, want 1 (%v)", meta.ResourceVersion, err)
			}
			select {
			case ev := <-w.Events():
				t.Fatalf("watcher saw %s %s", ev.Type, store.KeyOf(ev.Object))
			case <-time.After(20 * time.Millisecond):
			}

			// The store is usable afterwards, and the next write takes the next version.
			fail = false
			r := run("after")
			if err := s.Create(ctx, r); err != nil || r.Metadata.ResourceVersion != "2" {
				t.Fatalf("Create after = %s, %v", r.Metadata.ResourceVersion, err)
			}
		})
	}
}

func TestSnapshotRestore(t *testing.T) {
	ctx := t.Context()
	o := opts()
	src := memory.New(o)
	task := &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{ID: "t1", WorkItem: "wi-1", Labels: map[string]string{"a": "b"}},
		Spec:     v1alpha1.TaskRunSpec{Queue: "q"},
		Status:   &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}
	ev := &v1alpha1.Event{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "e1", Source: "s"}
	for _, obj := range []store.Object{task, ev, run("r1"), run("gone")} {
		if err := src.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	o.Clock.(*clock.Fake).Advance(time.Hour)
	task.Status.Phase = v1alpha1.TaskSucceeded
	if err := src.Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := src.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "gone"}); err != nil {
		t.Fatal(err)
	}
	recs, version := src.Snapshot()
	if version != 6 || len(recs) != 3 {
		t.Fatalf("Snapshot = %d records at %d, want 3 at 6", len(recs), version)
	}

	dst := memory.New(o)
	if err := dst.Restore(recs, version); err != nil {
		t.Fatal(err)
	}
	got, gotVersion := dst.Snapshot()
	sortRecs := func(r []memory.Record) {
		slices.SortFunc(r, func(a, b memory.Record) int { return strings.Compare(a.Key.String(), b.Key.String()) })
	}
	sortRecs(recs)
	sortRecs(got)
	if gotVersion != version || !reflect.DeepEqual(got, recs) {
		t.Fatalf("restored %+v at %d, want %+v at %d", got, gotVersion, recs, version)
	}
	var tasks []v1alpha1.Task
	if _, err := dst.List(ctx, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskPhase: "Succeeded"}}, &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("index after restore: %v, %v", tasks, err)
	}
	if _, err := dst.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "5"}); !errors.Is(err, store.ErrWatchExpired) {
		t.Fatalf("Watch from before the restore = %v, want ErrWatchExpired", err)
	}
	w, err := dst.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "6"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	r := run("next")
	if err := dst.Create(ctx, r); err != nil || r.Metadata.ResourceVersion != "7" {
		t.Fatalf("Create after restore = %s, %v", r.Metadata.ResourceVersion, err)
	}
	if e := <-w.Events(); e.Object.GetName() != "next" {
		t.Fatalf("event %v", e)
	}
	// Terminal tasks age out from their restored last write (an hour after the others).
	o.Clock.(*clock.Fake).Advance(store.DefaultRetention - time.Minute)
	if res, err := dst.Sweep(ctx); err != nil || res.Objects[v1alpha1.KindTask] != 0 || res.Objects[v1alpha1.KindRun] != 1 {
		t.Fatalf("Sweep = %v, %v", res, err)
	}
	o.Clock.(*clock.Fake).Advance(time.Minute)
	if res, err := dst.Sweep(ctx); err != nil || res.Objects[v1alpha1.KindTask] != 1 {
		t.Fatalf("Sweep = %v, %v", res, err)
	}
}

func TestRestoreRejects(t *testing.T) {
	runType := reflect.TypeFor[v1alpha1.Run]()
	good := memory.Record{
		Key: store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, Type: runType, Version: 1,
		Data: []byte(`{"kind":"Run","metadata":{"name":"r1","resourceVersion":"1"},"spec":{"task":"t"}}`),
	}
	with := func(f func(*memory.Record)) []memory.Record {
		r := good
		f(&r)
		return []memory.Record{r}
	}
	for _, tc := range []struct {
		name    string
		recs    []memory.Record
		version uint64
		want    string
	}{
		{"no type", with(func(r *memory.Record) { r.Type = nil }), 1, "not a struct"},
		{"not an object", with(func(r *memory.Record) { r.Type = reflect.TypeFor[struct{ A int }]() }), 1, "not a store.Object"},
		{"bad json", with(func(r *memory.Record) { r.Data = []byte("{") }), 1, "decode"},
		{"key mismatch", with(func(r *memory.Record) { r.Key.Name = "r2" }), 1, "key is Run/r1"},
		{"rv mismatch", with(func(r *memory.Record) { r.Version = 2 }), 2, "resourceVersion"},
		{"beyond version", []memory.Record{good}, 0, "outside"},
		{"duplicate", []memory.Record{good, good}, 1, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := memory.New(opts()).Restore(tc.recs, tc.version)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Restore = %v, want %q", err, tc.want)
			}
		})
	}
	s := memory.New(opts())
	if err := s.Create(t.Context(), run("r0")); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore([]memory.Record{good}, 1); err == nil {
		t.Fatal("Restore into a used store succeeded")
	}
}

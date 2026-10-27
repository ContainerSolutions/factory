package objectstore

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// writeHistory writes n changes through s: creates, updates and deletes (every third run is
// deleted), compacting after the changes listed in compactAt.
func writeHistory(t *testing.T, s *Store, n int, compactAt ...int) {
	t.Helper()
	ctx := t.Context()
	for i := range n {
		name := fmt.Sprintf("r%02d", i/3)
		switch i % 3 {
		case 0:
			must(t, s.Create(ctx, newRun(name)))
		case 1:
			must(t, bump(ctx, s, name))
		case 2:
			if i%2 == 0 {
				must(t, s.Delete(ctx, runKey(name)))
			} else {
				must(t, bump(ctx, s, name))
			}
		}
		if slices.Contains(compactAt, i+1) {
			must(t, s.Compact(ctx))
		}
	}
}

// sameRecords compares two stores' contents, record by record.
func sameRecords(t *testing.T, got, want *Store) {
	t.Helper()
	norm := func(s *Store) ([]memory.Record, uint64) {
		recs, v := s.mem.Snapshot()
		slices.SortFunc(recs, func(a, b memory.Record) int { return strings.Compare(a.Key.String(), b.Key.String()) })
		for i := range recs {
			recs[i].Created, recs[i].Updated = recs[i].Created.UTC(), recs[i].Updated.UTC()
		}
		return recs, v
	}
	g, gv := norm(got)
	w, wv := norm(want)
	if gv != wv || !reflect.DeepEqual(g, w) {
		t.Fatalf("loaded %d records at %d, want %d at %d", len(g), gv, len(w), wv)
	}
}

// TestColdStart loads every combination of snapshot and log tail, with deletes in the history
// and the entries a snapshot covers deleted, and gets the writer's state back exactly.
func TestColdStart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		changes    int
		compactAt  []int
		dropCovers bool // delete the log entries the newest snapshot covers
		want       LoadInfo
	}{
		{name: "empty bucket", want: LoadInfo{}},
		{name: "log only", changes: 20, want: LoadInfo{Version: 20, LogEntries: 20}},
		{name: "snapshot only", changes: 20, compactAt: []int{20}, want: LoadInfo{Version: 20, Snapshot: 20}},
		{name: "snapshot and tail", changes: 20, compactAt: []int{12}, want: LoadInfo{Version: 20, Snapshot: 12, LogEntries: 8}},
		{name: "two snapshots and tail", changes: 30, compactAt: []int{9, 21}, want: LoadInfo{Version: 30, Snapshot: 21, LogEntries: 9}},
		{name: "covered entries deleted", changes: 30, compactAt: []int{9, 21}, dropCovers: true, want: LoadInfo{Version: 30, Snapshot: 21, LogEntries: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			w := r.open(t, "W")
			writeHistory(t, w, tc.changes, tc.compactAt...)
			if tc.dropCovers {
				for _, n := range r.b.Names(logPrefix) {
					if v, _ := versionOf(n, logPrefix, logSuffix); v <= tc.want.Snapshot {
						must(t, r.b.Delete(t.Context(), n))
					}
				}
			}
			s := r.open(t, "R")
			li := s.Loaded()
			if li.Version != tc.want.Version || li.Snapshot != tc.want.Snapshot || li.LogEntries != tc.want.LogEntries || li.ReadOnly || li.Writer != "R" {
				t.Fatalf("Loaded = %+v, want %+v", li, tc.want)
			}
			sameRecords(t, s, w)
			if err := s.Ready(); err != nil {
				t.Fatalf("Ready after the load: %v", err)
			}
			// The watch history starts at the loaded version.
			if li.Version > 0 {
				if _, err := s.Watch(t.Context(), v1alpha1.KindRun, store.WatchOptions{SinceVersion: store.FormatVersion(li.Version - 1)}); !errors.Is(err, store.ErrWatchExpired) {
					t.Fatalf("Watch from before the load = %v, want ErrWatchExpired", err)
				}
			}
			// And it writes on from there.
			must(t, s.Create(t.Context(), newRun("next")))
		})
	}
}

// TestColdStartRetries: a compaction and retention that delete the snapshot or entries a load
// is reading make it start again, and it still gets the whole state.
func TestColdStartRetries(t *testing.T) {
	for _, step := range []FakeOp{FakeList, FakeGet} {
		t.Run(string(step), func(t *testing.T) {
			r := newRig()
			w := r.open(t, "W")
			writeHistory(t, w, 15, 6)
			moved := false
			r.b.SetFaults(func(req FakeRequest) Fault {
				hit := (step == FakeList && req.Name == logPrefix) || (step == FakeGet && strings.HasPrefix(req.Name, snapshotPrefix))
				if !hit || moved {
					return Pass
				}
				moved = true
				// Meanwhile: more writes, a newer snapshot, and retention of everything older.
				r.b.SetFaults(nil)
				defer r.b.SetFaults(func(FakeRequest) Fault { return Pass })
				for i := range 6 {
					must(t, w.Create(t.Context(), newRun(fmt.Sprintf("meanwhile-%d", i))))
				}
				must(t, w.Compact(t.Context()))
				_, wv := w.mem.Snapshot()
				for _, n := range append(r.b.Names(logPrefix), r.b.Names(snapshotPrefix)...) {
					v, ok := versionOf(n, logPrefix, logSuffix)
					if !ok {
						v, _ = versionOf(n, snapshotPrefix, snapshotSuffix)
					}
					if v < wv {
						must(t, r.b.Delete(t.Context(), n))
					}
				}
				return Pass
			})
			s := r.open(t, "R")
			if !moved {
				t.Fatal("the load never reached the step")
			}
			sameRecords(t, s, w)
		})
	}
}

// TestLoadErrors: a log with a gap no snapshot covers, a corrupt entry and a corrupt snapshot
// fail Open with ErrCorrupt, saying where.
func TestLoadErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(r *rig)
		want  string
	}{
		{name: "gap", plant: func(r *rig) { must(t, r.b.Delete(t.Context(), logName(2))) }, want: "goes from version 1 to 3"},
		{name: "bad entry", plant: func(r *rig) { r.b.SetObject(logName(2), []byte(`{"v":"2"}`)) }, want: logName(2)},
		{name: "wrong version", plant: func(r *rig) { r.b.SetObject(logName(4), mustRead(t, r, logName(3))) }, want: "holds version 3"},
		{name: "unknown kind", plant: func(r *rig) {
			r.b.SetObject(logName(4), []byte(`{"v":"4","at":"2026-01-01T00:00:00Z","op":"create","kind":"Nope","name":"n","obj":{}}`))
		}, want: `kind "Nope"`},
		{name: "bad snapshot", plant: func(r *rig) { r.b.SetObject(snapshotName(3), []byte("not gzip")) }, want: snapshotName(3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			writeHistory(t, r.open(t, "W"), 3)
			tc.plant(r)
			_, err := Open(t.Context(), r.b, r.opts, withoutBackground())
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open = %v, want ErrCorrupt mentioning %q", err, tc.want)
			}
		})
	}
}

func mustRead(t *testing.T, r *rig, name string) []byte {
	t.Helper()
	b, ok := r.b.Objects()[name]
	if !ok {
		t.Fatalf("no %s", name)
	}
	return b
}

// TestSyncCost: a current store's sync is one GET of a missing log entry, and reads within the
// sync interval don't sync.
func TestSyncCost(t *testing.T) {
	r := newRig()
	s := r.open(t, "A")
	ctx := t.Context()
	must(t, s.Create(ctx, newRun("r1")))
	gets := r.b.Calls(FakeGet)
	must(t, s.Sync(ctx))
	if n := r.b.Calls(FakeGet) - gets; n != 1 {
		t.Fatalf("a current store's sync made %d GETs, want 1", n)
	}
	gets = r.b.Calls(FakeGet)
	getRun(t, s, "r1")
	var runs []v1alpha1.Run
	_, err := s.List(ctx, v1alpha1.KindRun, store.ListOptions{}, &runs)
	must(t, err)
	if n := r.b.Calls(FakeGet) - gets; n != 0 {
		t.Fatalf("reads within the sync interval made %d GETs", n)
	}
	r.clk.Advance(DefaultSyncInterval)
	getRun(t, s, "r1")
	if n := r.b.Calls(FakeGet) - gets; n != 1 {
		t.Fatalf("a read past the sync interval made %d GETs, want 1", n)
	}
}

// TestBackgroundSync: the background loop syncs every sync interval, so a watcher sees
// another store's writes without any read.
func TestBackgroundSync(t *testing.T) {
	r := newRig()
	a := r.open(t, "A")
	b := mustOpen(t, r.b, r.opts, WithWriterID("B"))
	w, err := b.Watch(t.Context(), v1alpha1.KindRun, store.WatchOptions{})
	must(t, err)
	defer w.Stop()
	must(t, a.Create(t.Context(), newRun("r1")))
	r.clk.BlockUntil(1)
	r.clk.Advance(DefaultSyncInterval)
	select {
	case ev := <-w.Events():
		if ev.Object.GetName() != "r1" {
			t.Fatalf("event %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the background sync didn't bring A's write")
	}
	must(t, b.Close())
	must(t, b.Close()) // idempotent
	if err := b.Create(t.Context(), newRun("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after Close = %v, want ErrClosed", err)
	}
	getRun(t, b, "r1") // reads go on
	if err := b.Ready(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ready after Close = %v, want ErrClosed", err)
	}
}

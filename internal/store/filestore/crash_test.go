package filestore

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// journalStates writes a sequence of changes, one journal line each, and returns the store's
// state after every line (states[n] is the state after n lines) and the journal.
func journalStates(t *testing.T) ([]dump, []byte) {
	t.Helper()
	ctx := t.Context()
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	states := []dump{takeDump(t, s)}
	step := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
		states = append(states, takeDump(t, s))
	}
	tk := task("tsk_1", "wi-1", v1alpha1.TaskPending)
	step(s.Create(ctx, run("run_1")))
	step(s.Create(ctx, tk))
	tk.Status.Phase = v1alpha1.TaskLeased
	step(s.Update(ctx, tk))
	ev := &v1alpha1.Event{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "e1", Source: "github-public", Time: epoch}
	step(s.Create(ctx, ev))
	ev.Subject = "edited"
	step(s.Update(ctx, ev))
	step(s.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "run_1"}))
	step(s.Create(ctx, lease("lse_1")))
	tk.Status.Phase = v1alpha1.TaskSucceeded
	step(s.Update(ctx, tk))
	// The last two lines are short, so every offset in them is cheap to try.
	step(s.Create(ctx, &v1alpha1.BudgetCounter{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter}, Metadata: v1alpha1.ObjectMeta{Name: "b"}}))
	step(s.Delete(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: "e1"}))
	s.crash()
	journal, err := os.ReadFile(filepath.Join(dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(journal, []byte("\n")); n != len(states)-1 {
		t.Fatalf("journal has %d lines for %d changes", n, len(states)-1)
	}
	return states, journal
}

// openJournal opens a fresh state dir holding only journal.
func openJournal(t *testing.T, journal []byte) (*Store, string, error) {
	t.Helper()
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, journalFile), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	opts, _ := testOpts()
	s, err := Open(dir, opts, withInternalKinds())
	return s, dir, err
}

// TestTornJournal cuts the journal at every offset within its last two lines and at random
// earlier ones (a crash mid-append): the reopened store holds exactly the complete lines.
func TestTornJournal(t *testing.T) {
	states, journal := journalStates(t)
	var ends []int
	for i, b := range journal {
		if b == '\n' {
			ends = append(ends, i+1)
		}
	}
	lastTwo := ends[len(ends)-3]
	offsets := []int{0}
	for off := lastTwo; off <= len(journal); off++ {
		offsets = append(offsets, off)
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test offsets, not crypto
	for range 40 {
		offsets = append(offsets, rng.IntN(lastTwo))
	}
	for _, off := range offsets {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
			t.Parallel()
			s, dir, err := openJournal(t, journal[:off])
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.crash()
			complete := bytes.Count(journal[:off], []byte("\n"))
			sameDump(t, "after the torn journal", takeDump(t, s), states[complete])
			good := 0
			if complete > 0 {
				good = ends[complete-1]
			}
			if fi, err := os.Stat(filepath.Join(dir, journalFile)); err != nil || fi.Size() != int64(good) {
				t.Fatalf("journal not truncated to the last good line (%d): %v %v", good, fi.Size(), err)
			}
			// The next write appends after the good lines.
			if err := s.Create(t.Context(), run("after")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJournalCorruption(t *testing.T) {
	states, journal := journalStates(t)
	lines := bytes.SplitAfter(journal, []byte("\n"))
	lines = lines[:len(lines)-1] // the empty tail after the last newline
	n := len(lines)
	edit := func(f func(ls [][]byte) [][]byte) []byte {
		ls := f(slices.Clone(lines))
		return bytes.Join(ls, nil)
	}
	for _, tc := range []struct {
		name    string
		journal []byte
		want    string // the error, or "" for a dropped torn line
		state   int
	}{
		{"garbage mid-file", edit(func(ls [][]byte) [][]byte { ls[1] = []byte("not json\n"); return ls }), "line 2: invalid JSON", 0},
		{"cut mid-file line", edit(func(ls [][]byte) [][]byte { ls[2] = append(ls[2][:20:20], '\n'); return ls }), "line 3: invalid JSON", 0},
		{"missing line", edit(func(ls [][]byte) [][]byte { return slices.Delete(ls, 1, 2) }), "line 2: version 3, want 2 (a change is missing)", 0},
		{"repeated line", edit(func(ls [][]byte) [][]byte { return slices.Insert(ls, 1, ls[0]) }), "line 2: version 1, want 2", 0},
		{"unknown op", edit(func(ls [][]byte) [][]byte {
			ls[3] = bytes.Replace(ls[3], []byte(`"op":"create"`), []byte(`"op":"upsert"`), 1)
			return ls
		}), `line 4: unknown op "upsert"`, 0},
		{"update of a missing object", edit(func(ls [][]byte) [][]byte {
			ls[2] = bytes.Replace(ls[2], []byte(`"name":"tsk_1"`), []byte(`"name":"tsk_9"`), 1)
			return ls
		}), "line 3: update Task/tsk_9: it doesn't exist", 0},
		{"invalid last line", edit(func(ls [][]byte) [][]byte { ls[n-1] = []byte("{oops\n"); return ls }), "", n - 1},
		{"NULs after the last line", append(slices.Clone(journal), 0, 0, 0, 0), "", n},
		{"last line without its newline", journal[:len(journal)-1], "", n - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, err := openJournal(t, tc.journal)
			if tc.want != "" {
				if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), journalFile+" "+tc.want) {
					t.Fatalf("Open = %v, want ErrCorrupt with %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.crash()
			sameDump(t, "after dropping the torn line", takeDump(t, s), states[tc.state])
		})
	}
}

// compact runs one compaction, as maybeCompact does.
func compact(s *Store) error {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()
	return s.compactLocked()
}

func journalLines(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

// TestCompactionCrash crashes a compaction after each step: every reopen has the same state.
func TestCompactionCrash(t *testing.T) {
	boom := errors.New("injected crash")
	for _, step := range []string{stepSnapshotWritten, stepSnapshotRenamed, stepJournalTruncated} {
		t.Run(step, func(t *testing.T) {
			ctx := t.Context()
			dir := stateDir(t)
			opts, clk := testOpts()
			s := mustOpen(t, dir, opts)
			writeMix(t, s, clk)
			if err := compact(s); err != nil { // an old snapshot to replace
				t.Fatal(err)
			}
			for _, name := range []string{"run_a", "run_b", "run_c"} {
				if err := s.Create(ctx, run(name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "run_a"}); err != nil {
				t.Fatal(err)
			}
			want := takeDump(t, s)
			oldSnap, err := os.ReadFile(filepath.Join(dir, snapshotFile))
			if err != nil {
				t.Fatal(err)
			}

			s.setFaults(faults{compact: func(st string) error {
				if st == step {
					return boom
				}
				return nil
			}})
			if err := compact(s); !errors.Is(err, boom) {
				t.Fatalf("compaction = %v, want the injected crash", err)
			}
			s.crash()

			// What the crash left.
			snap, err := os.ReadFile(filepath.Join(dir, snapshotFile))
			if err != nil {
				t.Fatal(err)
			}
			_, tmpErr := os.Stat(filepath.Join(dir, snapshotFile+tmpSuffix))
			switch step {
			case stepSnapshotWritten:
				if !bytes.Equal(snap, oldSnap) || tmpErr != nil || journalLines(t, dir) != 4 {
					t.Fatalf("after %s: want the old snapshot, a tmp and the full journal", step)
				}
			case stepSnapshotRenamed:
				if bytes.Equal(snap, oldSnap) || journalLines(t, dir) != 4 {
					t.Fatalf("after %s: want the new snapshot and the full journal", step)
				}
			case stepJournalTruncated:
				if bytes.Equal(snap, oldSnap) || journalLines(t, dir) != 0 {
					t.Fatalf("after %s: want the new snapshot and an empty journal", step)
				}
			}

			s2 := mustOpen(t, dir, opts)
			sameDump(t, "after the crash", takeDump(t, s2), want)
			if _, err := os.Stat(filepath.Join(dir, snapshotFile+tmpSuffix)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot tmp left after Open: %v", err)
			}
			// A write and a clean Close fold everything (skipped lines included) into the snapshot.
			if err := s2.Create(ctx, run("run_d")); err != nil {
				t.Fatal(err)
			}
			want2 := takeDump(t, s2)
			if err := s2.Close(); err != nil {
				t.Fatal(err)
			}
			if n := journalLines(t, dir); n != 0 {
				t.Fatalf("journal after Close has %d lines", n)
			}
			s3 := mustOpen(t, dir, opts)
			defer s3.crash()
			sameDump(t, "after Close", takeDump(t, s3), want2)
		})
	}
}

// TestCompactionKeepsConcurrentWrites: writes made while the snapshot is being written land
// in the journal that replaces the old one.
func TestCompactionKeepsConcurrentWrites(t *testing.T) {
	ctx := t.Context()
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	writeMix(t, s, clk)
	s.setFaults(faults{compact: func(st string) error {
		if st != stepSnapshotWritten {
			return nil
		}
		for _, name := range []string{"run_x", "run_y", "run_z"} {
			if err := s.Create(ctx, run(name)); err != nil {
				return err
			}
		}
		return nil
	}})
	if err := compact(s); err != nil {
		t.Fatal(err)
	}
	s.setFaults(faults{})
	if n := journalLines(t, dir); n != 3 {
		t.Fatalf("journal after compaction has %d lines, want the 3 written during it", n)
	}
	want := takeDump(t, s)
	s.crash()
	s2 := mustOpen(t, dir, opts)
	defer s2.crash()
	sameDump(t, "after reopen", takeDump(t, s2), want)
}

// TestCompactionThresholds: writers past either limit compact, concurrently with each other.
func TestCompactionThresholds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int64
		lines int
	}{
		{"lines", 1 << 30, 7},
		{"bytes", 2048, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := stateDir(t)
			opts, _ := testOpts()
			s := mustOpen(t, dir, opts, WithCompaction(tc.bytes, tc.lines))
			var wg sync.WaitGroup
			errs := make(chan error, 200)
			for g := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range 15 {
						r := run(fmt.Sprintf("run_%d_%d", g, i))
						if err := s.Create(ctx, r); err != nil {
							errs <- err
							return
						}
						r.Spec.Attempt = 2
						if err := s.Update(ctx, r); err != nil {
							errs <- err
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, snapshotFile)); err != nil {
				t.Fatalf("no snapshot after 240 writes: %v", err)
			}
			if n := journalLines(t, dir); n >= 240 {
				t.Fatalf("journal has %d lines: never compacted", n)
			}
			want := takeDump(t, s)
			s.crash()
			s2 := mustOpen(t, dir, opts)
			defer s2.crash()
			sameDump(t, "after reopen", takeDump(t, s2), want)
		})
	}
}

// TestCompactionFailure: a compaction that keeps failing doesn't fail writes; it is retried,
// and succeeds once the cause is gone.
func TestCompactionFailure(t *testing.T) {
	ctx := t.Context()
	dir := stateDir(t)
	opts, _ := testOpts()
	s := mustOpen(t, dir, opts, WithCompaction(1<<30, 4))
	var tries atomic.Int32
	s.setFaults(faults{compact: func(st string) error {
		if st == stepSnapshotWritten {
			tries.Add(1)
			return errors.New("disk full")
		}
		return nil
	}})
	for i := range 12 {
		if err := s.Create(ctx, run(fmt.Sprint("run_", i))); err != nil {
			t.Fatalf("write %d failed with the compaction: %v", i, err)
		}
	}
	if n := tries.Load(); n < 2 {
		t.Fatalf("compaction tried %d times", n)
	}
	if n := journalLines(t, dir); n != 12 {
		t.Fatalf("journal has %d lines, want all 12", n)
	}
	s.setFaults(faults{})
	if err := s.Create(ctx, run("run_ok")); err != nil {
		t.Fatal(err)
	}
	if n := journalLines(t, dir); n != 0 {
		t.Fatalf("journal has %d lines after a working compaction", n)
	}
	want := takeDump(t, s)
	s.crash()
	s2 := mustOpen(t, dir, opts)
	defer s2.crash()
	sameDump(t, "after reopen", takeDump(t, s2), want)
}

// TestSweepJournaled: the sweeper's deletes survive a restart, transcripts included.
func TestSweepJournaled(t *testing.T) {
	ctx := t.Context()
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	writeMix(t, s, clk)
	clk.Advance(store.DefaultRetention)
	res, err := s.Sweep(ctx)
	if err != nil || res.Objects[v1alpha1.KindRun] != 1 || res.Transcripts != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	want := takeDump(t, s)
	s.crash()
	s2 := mustOpen(t, dir, store.Options{Clock: clock.NewFake(clk.Now()), Logger: opts.Logger})
	defer s2.crash()
	sameDump(t, "after reopen", takeDump(t, s2), want)
}

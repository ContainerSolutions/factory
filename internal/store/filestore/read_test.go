package filestore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// viewDump is a View as a dump, with the transcripts as run, size and sha256, sorted (a View
// doesn't index their creation or put order).
func viewDump(t *testing.T, v *View) dump {
	t.Helper()
	d := memDump(t, v.mem)
	for _, run := range v.Transcripts() {
		data, err := v.Transcript(run)
		if err != nil {
			t.Fatalf("Transcript %s: %v", run, err)
		}
		d.Transcripts = append(d.Transcripts, fmt.Sprintf("%s %d %x", run, len(data), sha256.Sum256(data)))
	}
	slices.Sort(d.Transcripts)
	return d
}

// unordered drops the creation times and put order of a dump's transcripts, as viewDump has
// them.
func unordered(d dump) dump {
	var trs []string
	for _, tr := range d.Transcripts {
		trs = append(trs, tr[:strings.LastIndexByte(tr, ' ')])
	}
	slices.Sort(trs)
	d.Transcripts = trs
	return d
}

// files returns every file under dir with its mode and bytes, to show that nothing was
// written.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		data := []byte(nil)
		if !d.IsDir() {
			if data, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		out[path] = fmt.Sprintf("%v %s %x", fi.Mode(), fi.ModTime().Format(time.RFC3339Nano), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustRead(t *testing.T, dir string) *View {
	t.Helper()
	opts, _ := testOpts()
	v, err := Read(dir, opts, withInternalKinds())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return v
}

// TestReadWhileOpen reads a dir a store has open (snapshot plus journal), and a copy of it:
// the View is the store's state, and nothing in the dir changes.
func TestReadWhileOpen(t *testing.T) {
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	writeMix(t, s, clk)
	if err := compact(s); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(t.Context(), run("run_after")); err != nil { // a journal line above the snapshot
		t.Fatal(err)
	}
	defer s.crash()
	before := files(t, dir)

	v := mustRead(t, dir)
	sameDump(t, "the view", viewDump(t, v), unordered(takeDump(t, s)))
	if li := v.Loaded(); !li.Snapshot || li.JournalLines != 1 || li.TornLine || li.Transcripts != 2 || li.Objects[v1alpha1.KindRun] != 2 {
		t.Errorf("Loaded() = %+v", li)
	}
	if j := v.Journal(); len(j) != 1 || j[0].Op != memory.OpCreate || j[0].Name != "run_after" || j[0].Version != v.Loaded().Version {
		t.Errorf("Journal() = %+v", j)
	}
	if w := v.Warnings(); len(w) != 0 {
		t.Errorf("Warnings() = %q", w)
	}
	if !maps.Equal(files(t, dir), before) {
		t.Fatal("Read changed the state dir")
	}
	if _, err := v.Transcript("run_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Transcript of a run without one = %v, want ErrNotFound", err)
	}
	var tk v1alpha1.Task
	if err := v.Get(t.Context(), store.Key{Kind: v1alpha1.KindTask, Name: "tsk_1"}, &tk); err != nil || tk.Status.Phase != v1alpha1.TaskSucceeded {
		t.Errorf("Get tsk_1 = %v, %+v", err, tk.Status)
	}

	// A copy (os.CopyFS makes it 0755/0644, which a store refuses) reads with a warning.
	cp := filepath.Join(t.TempDir(), "copy")
	if err := os.CopyFS(cp, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	cv := mustRead(t, cp)
	sameDump(t, "the copy", viewDump(t, cv), viewDump(t, v))
	if w := cv.Warnings(); len(w) != 1 || !strings.Contains(w[0], "group or others") || !strings.Contains(w[0], "a dispatcher refuses") {
		t.Errorf("Warnings() of the copy = %q", w)
	}
}

// TestReadTornLine cuts the journal within its last line (an append in flight, or a crash):
// Read holds the complete lines and leaves the file as it was.
func TestReadTornLine(t *testing.T) {
	states, journal := journalStates(t)
	last := bytes.LastIndexByte(journal[:len(journal)-1], '\n') + 1
	for off := last; off <= len(journal); off++ {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
			t.Parallel()
			dir := stateDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, journalFile)
			if err := os.WriteFile(path, journal[:off], 0o600); err != nil {
				t.Fatal(err)
			}
			before := files(t, dir)
			v := mustRead(t, dir)
			complete := bytes.Count(journal[:off], []byte("\n"))
			want := states[complete]
			want.Transcripts = nil
			sameDump(t, "the view", viewDump(t, v), want)
			torn := off != last && off != len(journal)
			if li := v.Loaded(); li.TornLine != torn || li.JournalLines != complete {
				t.Errorf("Loaded() = %+v, want torn=%v and %d lines", li, torn, complete)
			}
			if w := v.Warnings(); torn != (len(w) == 1 && strings.Contains(w[0], "incomplete")) {
				t.Errorf("Warnings() = %q", w)
			}
			if len(v.Journal()) != complete {
				t.Errorf("Journal() has %d lines, want %d", len(v.Journal()), complete)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, journal[:off]) {
				t.Fatalf("the journal changed: %v", err)
			}
			if !maps.Equal(files(t, dir), before) {
				t.Fatal("Read changed the state dir")
			}
		})
	}
}

// TestReadConcurrent reads while writers append and compact (small limits): every Read
// succeeds, holds a state the store went through (versions only move forward, and the
// counter's runs exist), and the last one matches the store.
//
// The whole test compacts fewer times than readAttempts. A Read starts over once per
// compaction that overlaps it, so it can't run out of attempts however the goroutines are
// scheduled, as with a dispatcher, which compacts every DefaultCompactLines lines. Compacting
// every few lines (as this test once did) let a writer with Linux's fast fsync replace the
// snapshot during ten attempts in a row. TestReadRacesCompaction covers running out.
func TestReadConcurrent(t *testing.T) {
	const writes, compactLines = 150, 64 // 2*writes+1 lines: 4 compactions
	if (2*writes+1)/compactLines >= readAttempts {
		t.Fatal("the test compacts as often as a Read starts over")
	}
	ctx := t.Context()
	dir := stateDir(t)
	opts, _ := testOpts()
	s := mustOpen(t, dir, opts, WithCompaction(1<<20, compactLines))
	defer s.crash()
	counter := &v1alpha1.BudgetCounter{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter}, Metadata: v1alpha1.ObjectMeta{Name: "c"}}
	if err := s.Create(ctx, counter); err != nil {
		t.Fatal(err)
	}

	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for i := range writes {
			// The run first, then the counter: a reader never sees a count above the runs.
			if err := s.Create(ctx, run(fmt.Sprintf("run_%03d", i))); err != nil {
				t.Error(err)
				return
			}
			counter.Status.Runs = i + 1
			if err := s.Update(ctx, counter); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var reads atomic.Int64
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lastV uint64
			for !done.Load() {
				v, err := Read(dir, opts, withInternalKinds())
				if err != nil {
					t.Errorf("Read while writing: %v", err)
					return
				}
				li := v.Loaded()
				if li.Version < lastV {
					t.Errorf("version went back: %d after %d", li.Version, lastV)
				}
				lastV = li.Version
				var c v1alpha1.BudgetCounter
				if err := v.Get(ctx, store.Key{Kind: v1alpha1.KindBudgetCounter, Name: "c"}, &c); err != nil {
					t.Errorf("Get the counter: %v", err)
					return
				}
				if n := li.Objects[v1alpha1.KindRun]; n < c.Status.Runs || n > c.Status.Runs+1 {
					t.Errorf("version %d: %d runs for a count of %d", li.Version, n, c.Status.Runs)
				}
				reads.Add(1)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if reads.Load() == 0 {
		t.Fatal("no read finished while writing")
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotFile)); err != nil {
		t.Fatalf("no compaction while reading: %v", err)
	}
	sameDump(t, "the last read", viewDump(t, mustRead(t, dir)), unordered(takeDump(t, s)))
}

// TestReadRacesCompaction compacts between Read's snapshot and journal reads: the journal
// read no longer fits the snapshot read, so Read starts over, and gives up after
// readAttempts compactions in a row.
func TestReadRacesCompaction(t *testing.T) {
	ctx := t.Context()
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	defer s.crash()
	writeMix(t, s, clk)
	n := 0
	compactNow := func() {
		n++
		for i := range 3 {
			if err := s.Create(ctx, run(fmt.Sprintf("run_race_%d_%d", n, i))); err != nil {
				t.Fatal(err)
			}
		}
		if err := compact(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, first := range []bool{true, false} { // the first compaction creates the snapshot
		n0 := n
		v, err := Read(dir, opts, withInternalKinds(), func(c *config) {
			c.afterSnapshot = func() {
				if n == n0 {
					compactNow()
				}
			}
		})
		if err != nil {
			t.Fatalf("Read (first=%v): %v", first, err)
		}
		sameDump(t, "the view", viewDump(t, v), unordered(takeDump(t, s)))
	}

	_, err := Read(dir, opts, withInternalKinds(), func(c *config) { c.afterSnapshot = compactNow })
	if err == nil || !strings.Contains(err.Error(), "replaced 10 times") {
		t.Fatalf("Read with a compaction every time = %v", err)
	}
}

func TestReadErrors(t *testing.T) {
	opts, _ := testOpts()
	if _, err := Read(filepath.Join(t.TempDir(), "missing"), opts); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read of a missing dir = %v, want ErrNotExist", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(file, opts); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("Read of a file = %v", err)
	}
	for _, tc := range []struct {
		name, file, data string
		want             error
	}{
		{"unknown format", snapshotFile, `{"format":2,"storeVersion":"1","objects":[]}`, ErrFormat},
		{"corrupt line", journalFile, "not json\n" + `{"v":"1"}` + "\n", ErrCorrupt},
		{"unknown kind", snapshotFile, `{"format":1,"storeVersion":"1","objects":[{"kind":"Nope","name":"n","v":"1","obj":{}}]}`, ErrCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stateDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir, opts); !errors.Is(err, tc.want) {
				t.Fatalf("Read = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMoveAside(t *testing.T) {
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	writeMix(t, s, clk)
	dst := dir + ".old"
	if err := MoveAside(dir, dst); !errors.Is(err, ErrLocked) {
		t.Fatalf("MoveAside while open = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the dir moved while locked: %v", err)
	}
	want := viewDump(t, mustRead(t, dir))
	s.crash()

	taken := dir + ".taken"
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MoveAside(dir, taken); !errors.Is(err, os.ErrExist) {
		t.Fatalf("MoveAside onto an existing path = %v, want ErrExist", err)
	}
	if err := MoveAside(dir, dst); err != nil {
		t.Fatalf("MoveAside: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dir is still there: %v", err)
	}
	sameDump(t, "the moved dir", viewDump(t, mustRead(t, dst)), want)
	if err := MoveAside(dir, dir+".again"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MoveAside of a missing dir = %v, want ErrNotExist", err)
	}
}

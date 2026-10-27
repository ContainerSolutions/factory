package filestore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
	"github.com/ContainerSolutions/factory/internal/store/storetest"
)

var epoch = storetest.Epoch

// testOpts returns options on a fake clock that logs nothing.
func testOpts() (store.Options, *clock.Fake) {
	clk := clock.NewFake(epoch)
	return store.Options{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, clk
}

// stateDir returns a path for a new state dir (Open creates it 0700; t.TempDir's own dirs are
// 0755, which Open refuses).
func stateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

func mustOpen(t *testing.T, dir string, opts store.Options, options ...Option) *Store {
	t.Helper()
	s, err := Open(dir, opts, append([]Option{withInternalKinds()}, options...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// crash drops the store the way a killed process would: no compaction, the handles closed and
// the flock released.
func (s *Store) crash() {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()
	s.jmu.Lock()
	defer s.jmu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.isClosed.Store(true)
	_ = s.jf.Close()
	_ = s.lock.Close()
}

func (s *Store) setFaults(f faults) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.faults = f
}

// TestConformance runs the store suite on file stores, each on a fresh dir, then audits what
// the run stored for secrets (secrets_test.go).
func TestConformance(t *testing.T) {
	root := t.TempDir()
	var (
		mu     sync.Mutex
		stores []*Store
		n      atomic.Int64
	)
	t.Run("Suite", func(t *testing.T) {
		storetest.Run(t, func(o store.Options) storetest.Subject {
			dir := filepath.Join(root, fmt.Sprint(n.Add(1)))
			s, err := Open(dir, o)
			if err != nil {
				t.Errorf("Open: %v", err)
				return memory.New(o) // keeps the suite running; the test has failed
			}
			mu.Lock()
			stores = append(stores, s)
			mu.Unlock()
			return s
		})
	})
	if len(stores) == 0 {
		t.Fatal("no store opened")
	}

	// What the run wrote: the journals (no compaction at the default limits), then the
	// snapshots Close writes.
	var files []storedFile
	for _, s := range stores {
		files = append(files, readStored(t, s.dir)...)
	}
	for _, s := range stores {
		if err := s.Close(); err != nil {
			t.Errorf("Close %s: %v", s.dir, err)
		}
		files = append(files, readStored(t, s.dir)...)
	}
	t.Run("KindAudit", func(t *testing.T) { checkKindAudit(t, files) })
	t.Run("TokenScan", func(t *testing.T) { checkNoTokens(t, files) })
}

// ---- state dumps -----------------------------------------------------------------------------

// dump is everything a reopen must keep, as comparable strings.
type dump struct {
	Version     uint64
	Records     []string // kind/name, version, created, updated, JSON
	Fields      []string // kind field=value: names, from List by field
	Transcripts []string // run, size, sha256, created, in put order
}

func takeDump(t *testing.T, s *Store) dump {
	t.Helper()
	d := memDump(t, s.mem)

	s.tmu.Lock()
	type tr struct {
		run string
		b   tblob
	}
	var trs []tr
	for run, b := range s.blobs {
		trs = append(trs, tr{run, *b})
	}
	s.tmu.Unlock()
	slices.SortFunc(trs, func(a, b tr) int { return int(a.b.seq) - int(b.b.seq) })
	for _, x := range trs {
		data, err := s.GetTranscript(t.Context(), x.run)
		if err != nil {
			t.Fatalf("GetTranscript %s: %v", x.run, err)
		}
		d.Transcripts = append(d.Transcripts, fmt.Sprintf("%s %d %x %s", x.run, len(data), sha256.Sum256(data), x.b.created.UTC().Format(time.RFC3339Nano)))
	}
	return d
}

// memDump is the records and fields part of a dump, from a memory store.
func memDump(t *testing.T, mem *memory.Store) dump {
	t.Helper()
	recs, v := mem.Snapshot()
	slices.SortFunc(recs, func(a, b memory.Record) int { return strings.Compare(a.Key.String(), b.Key.String()) })
	d := dump{Version: v}
	fields := map[string]map[string]bool{} // "kind\x00field" → values
	for _, r := range recs {
		d.Records = append(d.Records, fmt.Sprintf("%s v%d created=%s updated=%s %s",
			r.Key, r.Version, r.Created.Format(time.RFC3339Nano), r.Updated.Format(time.RFC3339Nano), r.Data))
		obj := reflect.New(r.Type).Interface().(store.Object)
		if err := jsonUnmarshal(r.Data, obj); err != nil {
			t.Fatal(err)
		}
		for f, vals := range store.FieldValues(obj) {
			key := r.Key.Kind + "\x00" + f
			if fields[key] == nil {
				fields[key] = map[string]bool{}
			}
			for _, v := range vals {
				fields[key][v] = true
			}
		}
	}
	for key, vals := range fields {
		kind, f, _ := strings.Cut(key, "\x00")
		for v := range vals {
			var objs []store.Object
			if _, err := mem.List(t.Context(), kind, store.ListOptions{Field: map[string]string{f: v}}, &objs); err != nil {
				t.Fatalf("List %s %s=%s: %v", kind, f, v, err)
			}
			names := make([]string, len(objs))
			for i, o := range objs {
				names[i] = o.GetName()
			}
			d.Fields = append(d.Fields, fmt.Sprintf("%s %s=%s: %v", kind, f, v, names))
		}
	}
	slices.Sort(d.Fields)
	return d
}

func sameDump(t *testing.T, what string, got, want dump) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %s\nwant %s", what, fmtDump(got), fmtDump(want))
	}
}

func fmtDump(d dump) string {
	return fmt.Sprintf("version %d\n  records:\n    %s\n  fields:\n    %s\n  transcripts:\n    %s", d.Version,
		strings.Join(d.Records, "\n    "), strings.Join(d.Fields, "\n    "), strings.Join(d.Transcripts, "\n    "))
}

// ---- tests -----------------------------------------------------------------------------------

func TestRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*Store) error
	}{
		{"Close", (*Store).Close},
		{"Crash", func(s *Store) error { s.crash(); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stateDir(t)
			opts, clk := testOpts()
			s := mustOpen(t, dir, opts)
			writeMix(t, s, clk)
			before := takeDump(t, s)
			if err := tc.stop(s); err != nil {
				t.Fatal(err)
			}
			if tc.name == "Close" {
				// Close compacted: the snapshot holds everything and the journal is empty.
				if fi, err := os.Stat(filepath.Join(dir, journalFile)); err != nil || fi.Size() != 0 {
					t.Fatalf("journal after Close: %v, %v", fi, err)
				}
			}

			s2 := mustOpen(t, dir, opts)
			defer s2.Close()
			sameDump(t, "after reopen", takeDump(t, s2), before)
			li := s2.Loaded()
			n := 0
			for _, c := range li.Objects {
				n += c
			}
			closed := tc.name == "Close"
			if li.Version != before.Version || n != len(before.Records) || li.Snapshot != closed || (li.JournalLines == 0) == !closed ||
				li.TornLine || li.Transcripts != len(before.Transcripts) {
				t.Fatalf("Loaded() = %+v; want version %d, %d objects, %d transcripts, snapshot %v", li, before.Version, n, len(before.Transcripts), closed)
			}

			// Watches start from the restored version; older ones have expired.
			ctx := t.Context()
			since := store.FormatVersion(before.Version)
			if _, err := s2.Watch(ctx, v1alpha1.KindTask, store.WatchOptions{SinceVersion: store.FormatVersion(before.Version - 1)}); !errors.Is(err, store.ErrWatchExpired) {
				t.Fatalf("Watch from before the restart = %v, want ErrWatchExpired", err)
			}
			w, err := s2.Watch(ctx, v1alpha1.KindTask, store.WatchOptions{SinceVersion: since})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			// Writes continue the version counter.
			tk := task("t-after", "wi-1", v1alpha1.TaskPending)
			if err := s2.Create(ctx, tk); err != nil {
				t.Fatal(err)
			}
			if tk.Metadata.ResourceVersion != store.FormatVersion(before.Version+1) {
				t.Fatalf("rv after restart %s, want %d", tk.Metadata.ResourceVersion, before.Version+1)
			}
			select {
			case ev := <-w.Events():
				if ev.Object.GetName() != "t-after" {
					t.Fatalf("event %s", ev.Object.GetName())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no event after restart")
			}
			// Locks are not persisted: one held before the restart is free.
			u, err := s2.Lock(ctx, "held-before", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			u.Unlock()
		})
	}
}

func TestSecondOpenFails(t *testing.T) {
	dir := stateDir(t)
	opts, _ := testOpts()
	s := mustOpen(t, dir, opts)
	_, err := Open(dir, opts)
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "another dispatcher") {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpen(t, dir, opts)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPermissions(t *testing.T) {
	ctx := t.Context()
	opts, _ := testOpts()

	// What Open creates is private.
	dir := filepath.Join(t.TempDir(), "state")
	s := mustOpen(t, dir, opts)
	if err := s.Create(ctx, run("r1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTranscript(ctx, "r1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		dir: 0o700, filepath.Join(dir, transcriptDir): 0o700,
		filepath.Join(dir, snapshotFile): 0o600, filepath.Join(dir, journalFile): 0o600, filepath.Join(dir, lockFile): 0o600,
		filepath.Join(dir, transcriptDir, "r1"+transcriptSuffix): 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %o", path, fi.Mode(), err, want)
		}
	}

	for _, tc := range []struct {
		name  string
		path  string
		chmod os.FileMode
		want  string
	}{
		{"dir 0755", "", 0o755, "chmod 700"},
		{"dir 0710", "", 0o710, "chmod 700"},
		{"journal 0644", journalFile, 0o644, "chmod 600"},
		{"snapshot 0660", snapshotFile, 0o660, "chmod 600"},
		{"transcript 0604", filepath.Join(transcriptDir, "r1"+transcriptSuffix), 0o604, "chmod 600"},
		{"transcripts dir 0750", transcriptDir, 0o750, "chmod 700"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.path)
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.chmod); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(path, fi.Mode().Perm()) //nolint:errcheck // restores the mode for the next case
			_, err = Open(dir, opts)
			if !errors.Is(err, ErrInsecure) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("Open = %v, want ErrInsecure naming %s and %q", err, path, tc.want)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(dir, "elsewhere")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(link) //nolint:errcheck // test cleanup
		if _, err := Open(dir, opts); !errors.Is(err, ErrInsecure) {
			t.Fatalf("Open = %v, want ErrInsecure", err)
		}
	})
	// And the dir opens again once fixed.
	mustOpen(t, dir, opts).crash()
}

func TestWriteFailure(t *testing.T) {
	boom := errors.New("injected journal failure")
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, s *Store) error
		check func(t *testing.T, s *Store)
	}{
		{
			name:  "Create",
			write: func(t *testing.T, s *Store) error { return s.Create(t.Context(), run("new")) },
			check: func(t *testing.T, s *Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "new"}, &got); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("Get = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "Update",
			write: func(t *testing.T, s *Store) error {
				var r v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &r); err != nil {
					t.Fatal(err)
				}
				r.Spec.Attempt = 7
				return s.Update(t.Context(), &r)
			},
			check: func(t *testing.T, s *Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &got); err != nil || got.Spec.Attempt != 1 {
					t.Fatalf("Get = %+v, %v; want the old attempt 1", got.Spec, err)
				}
			},
		},
		{
			name: "Delete",
			write: func(t *testing.T, s *Store) error {
				return s.Delete(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"})
			},
			check: func(t *testing.T, s *Store) {
				var got v1alpha1.Run
				if err := s.Get(t.Context(), store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &got); err != nil {
					t.Fatalf("Get = %v, want r1 still there", err)
				}
			},
		},
	} {
		for _, fault := range []struct {
			name   string
			f      faults
			broken bool
		}{
			{"write", faults{write: func(f *os.File, line []byte) (int, error) {
				n, _ := f.Write(line[:len(line)/2]) // a short write: rolled back by truncation
				return n, boom
			}}, false},
			{"fsync", faults{sync: func(*os.File) error { return boom }}, true},
		} {
			t.Run(tc.name+"/"+fault.name, func(t *testing.T) {
				ctx := t.Context()
				dir := stateDir(t)
				opts, _ := testOpts()
				s := mustOpen(t, dir, opts)
				if err := s.Create(ctx, run("r1")); err != nil {
					t.Fatal(err)
				}
				before := takeDump(t, s)

				s.setFaults(fault.f)
				if err := tc.write(t, s); !errors.Is(err, boom) {
					t.Fatalf("err = %v, want the journal's", err)
				}
				s.setFaults(faults{})
				tc.check(t, s)
				sameDump(t, "memory after the failed write", takeDump(t, s), before)

				err := s.Create(ctx, run("next"))
				if fault.broken {
					if !errors.Is(err, ErrJournal) || !errors.Is(err, boom) {
						t.Fatalf("write after a failed fsync = %v, want ErrJournal", err)
					}
				} else if err != nil {
					t.Fatalf("write after a failed append = %v", err)
				}
				want := takeDump(t, s)
				s.crash()
				s2 := mustOpen(t, dir, opts)
				defer s2.crash()
				sameDump(t, "after reopen", takeDump(t, s2), want)
			})
		}
	}
}

func TestKinds(t *testing.T) {
	ctx := t.Context()
	opts, _ := testOpts()
	dir := stateDir(t)

	// A kind without a registered type can't be written.
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, lease("l1")); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "WithKind") {
		t.Fatalf("Create of an unregistered kind = %v, want ErrInvalid", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Nor loaded: a store written with it fails to open without it, saying why.
	s = mustOpen(t, dir, opts)
	if err := s.Create(ctx, lease("l1")); err != nil {
		t.Fatal(err)
	}
	s.crash()
	if _, err := Open(dir, opts); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "WithKind") {
		t.Fatalf("Open without the kind = %v", err)
	}
	s = mustOpen(t, dir, opts)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, opts); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), snapshotFile) || !strings.Contains(err.Error(), "WithKind") {
		t.Fatalf("Open without the kind = %v", err)
	}

	// The registered type is enforced.
	s = mustOpen(t, dir, opts)
	defer s.crash()
	wrong := &otherLease{TypeMeta: v1alpha1.TypeMeta{Kind: "Lease"}, Metadata: v1alpha1.ObjectMeta{Name: "l2"}}
	if err := s.Create(ctx, wrong); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Create with another Go type = %v, want ErrInvalid", err)
	}
	for _, bad := range []Option{WithKind("", &otherLease{}), WithKind("Pool", &otherLease{}), WithKind("X", nil), WithKind(v1alpha1.KindRun, &otherLease{})} {
		if _, err := Open(stateDir(t), opts, bad); err == nil {
			t.Fatal("Open with a bad kind registration succeeded")
		}
	}
}

type otherLease struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
}

func (o *otherLease) GetName() string                     { return o.Metadata.Name }
func (o *otherLease) GetObjectMeta() *v1alpha1.ObjectMeta { return &o.Metadata }

func TestSnapshotErrors(t *testing.T) {
	opts, _ := testOpts()
	for _, tc := range []struct {
		name, snapshot string
		want           error
		text           string
	}{
		{"future format", `{"format":2,"storeVersion":"1","objects":[]}`, ErrFormat, "move the state dir aside"},
		{"no format", `{"storeVersion":"1","objects":[]}`, ErrFormat, "format 0"},
		{"not JSON", `{"format":1,`, ErrCorrupt, snapshotFile},
		{"bad version", `{"format":1,"storeVersion":"x","objects":[]}`, ErrCorrupt, "storeVersion"},
		{"object above the store", `{"format":1,"storeVersion":"1","objects":[{"kind":"Run","name":"r","v":"2","obj":{}}]}`, ErrCorrupt, "outside 1..1"},
		{"duplicate", `{"format":1,"storeVersion":"2","objects":[` + snapRun("r", 1) + `,` + snapRun("r", 2) + `]}`, ErrCorrupt, "duplicate"},
		{"key mismatch", `{"format":1,"storeVersion":"1","objects":[{"kind":"Run","name":"other","v":"1","obj":{"kind":"Run","metadata":{"name":"r","resourceVersion":"1"}}}]}`, ErrCorrupt, "key is Run/r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stateDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, snapshotFile), []byte(tc.snapshot), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir, opts)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Open = %v, want %v with %q", err, tc.want, tc.text)
			}
		})
	}
}

func snapRun(name string, v int) string {
	return fmt.Sprintf(`{"kind":"Run","name":%q,"v":"%d","obj":{"kind":"Run","metadata":{"name":%q,"resourceVersion":"%d"}}}`, name, v, name, v)
}

func TestTranscripts(t *testing.T) {
	ctx := t.Context()
	dir := stateDir(t)
	opts, clk := testOpts()
	opts.TranscriptMaxBytes, opts.TranscriptTotalBytes = 10, 25
	s := mustOpen(t, dir, opts)
	put := func(s *Store, run string, n int) {
		t.Helper()
		if err := s.PutTranscript(ctx, run, []byte(strings.Repeat(run[:1], n))); err != nil {
			t.Fatalf("Put %s: %v", run, err)
		}
		clk.Advance(time.Second)
	}
	has := func(s *Store, runs ...string) {
		t.Helper()
		for _, r := range []string{"a", "b/../../c", "d"} {
			_, err := s.GetTranscript(ctx, r)
			if want := slices.Contains(runs, r); want != (err == nil) {
				t.Fatalf("transcript %s present=%v, want %v (%v)", r, err == nil, want, err)
			}
		}
	}
	put(s, "a", 10)
	put(s, "b/../../c", 10) // not a safe file name: stored encoded, inside transcripts/
	s.crash()

	// After a restart the eviction order is the put order.
	s = mustOpen(t, dir, opts)
	has(s, "a", "b/../../c")
	put(s, "d", 10) // 30 > 25: a, the oldest, goes
	has(s, "b/../../c", "d")
	entries, err := os.ReadDir(filepath.Join(dir, transcriptDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "d"+transcriptSuffix || !strings.HasPrefix(entries[1].Name(), encodedPrefix) {
		t.Fatalf("transcripts/ = %v", entries)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Leftover tmp files go at load; a smaller total cap evicts the oldest; strays are kept.
	if err := os.WriteFile(filepath.Join(dir, transcriptDir, "x"+transcriptSuffix+tmpSuffix), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, transcriptDir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.TranscriptTotalBytes = 15
	s = mustOpen(t, dir, opts)
	has(s, "d")
	for _, gone := range []string{"x" + transcriptSuffix + tmpSuffix} {
		if _, err := os.Stat(filepath.Join(dir, transcriptDir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, transcriptDir, "notes.txt")); err != nil {
		t.Fatalf("stray file: %v", err)
	}

	// Deleted with the Run, and by the sweep.
	r := run("d")
	if err := s.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.KeyOf(r)); err != nil {
		t.Fatal(err)
	}
	has(s)
	if _, err := os.Stat(filepath.Join(dir, transcriptDir, "d"+transcriptSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transcript file left after its Run was deleted: %v", err)
	}
	put(s, "a", 5)
	clk.Advance(store.DefaultRetention)
	if res, err := s.Sweep(ctx); err != nil || res.Transcripts != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	has(s)

	if err := s.PutTranscript(ctx, strings.Repeat("x", 300), []byte("y")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Put with a long name = %v, want ErrInvalid", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTranscript(ctx, "a", []byte("y")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close = %v, want ErrClosed", err)
	}
	if err := s.Create(ctx, run("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Create after Close = %v, want ErrClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestTranscriptNames(t *testing.T) {
	for _, run := range []string{"run_01", "a.b-c_d", "x/y", "..", ".hidden", "-flag", "~tilde", "é", "a b"} {
		file := transcriptFile(run)
		if strings.ContainsRune(strings.TrimSuffix(file, transcriptSuffix), '/') || file == "" {
			t.Fatalf("transcriptFile(%q) = %q", run, file)
		}
		if got, ok := runOfFile(file); !ok || got != run {
			t.Fatalf("runOfFile(%q) = %q, %v; want %q", file, got, ok, run)
		}
	}
	for _, f := range []string{"x", transcriptSuffix, "~!!" + transcriptSuffix, "~" + "cnVuXzAx" + transcriptSuffix, ".x" + transcriptSuffix} {
		if run, ok := runOfFile(f); ok {
			t.Fatalf("runOfFile(%q) = %q, want not a transcript", f, run)
		}
	}
}

// TestRefusesObjectStoreDir: a dir an object store uses as its file:// bucket (log/ or
// snapshots/ in it) is never opened or read as a state dir.
func TestRefusesObjectStoreDir(t *testing.T) {
	for _, sub := range []string{"log", "snapshots"} {
		t.Run(sub, func(t *testing.T) {
			dir := stateDir(t)
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
				t.Fatal(err)
			}
			opts, _ := testOpts()
			if _, err := Open(dir, opts); !errors.Is(err, ErrObjectStoreDir) || !strings.Contains(err.Error(), sub+"/") {
				t.Fatalf("Open = %v, want ErrObjectStoreDir naming %s/", err, sub)
			}
			if _, err := Read(dir, opts); !errors.Is(err, ErrObjectStoreDir) {
				t.Fatalf("Read = %v, want ErrObjectStoreDir", err)
			}
			if _, err := os.Stat(LockPath(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Open left a LOCK file in the object store's dir: %v", err)
			}
		})
	}
}

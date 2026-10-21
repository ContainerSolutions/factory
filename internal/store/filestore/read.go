package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// readAttempts bounds how often Read starts over when a compaction replaced the snapshot while
// it was reading.
const readAttempts = 10

// View is a state dir loaded read-only, by Read: a memory store holding what the snapshot and
// the journal held when they were read, the journal lines above the snapshot, and the
// transcripts, read from their files on demand. Nothing in the dir is written, locked or
// removed, so a View can be taken while a store (a running serve) has the dir open, and of a
// copy of one.
type View struct {
	dir      string
	mem      *memory.Store
	loaded   LoadInfo
	journal  []JournalEntry
	runs     []string // runs with a transcript file, sorted
	warnings []string
}

// JournalEntry is one journal line: one committed change, in version order.
type JournalEntry struct {
	Version uint64
	At      time.Time
	Op      memory.Op
	Kind    string
	Name    string
	// Obj is the object as stored by the change (nil for delete).
	Obj json.RawMessage
}

// Read loads the state dir read-only: the snapshot, then the journal lines above its version.
// It writes nothing and takes no lock, so it works while a store has the dir open and on a
// copied dir:
//
//   - a journal line being appended (or torn by a crash) is the last one and has no trailing
//     newline: it is left out, and the file left as it is;
//   - a compaction that replaces the snapshot while Read reads makes it start over (the
//     snapshot it holds open is checked against the file's after the journal is read), so the
//     snapshot and the journal it loads always fit together.
//
// A dir that isn't private is read, with a warning (Warnings): only a store that writes
// refuses it. A missing dir is an error wrapping os.ErrNotExist; an unknown format is
// ErrFormat and corruption ErrCorrupt, as for Open. options register the internal kinds
// (WithKind), as for Open; WithCompaction is ignored.
func Read(dir string, opts store.Options, options ...Option) (*View, error) {
	cfg := config{kinds: map[string]store.Object{}}
	for _, o := range options {
		o(&cfg)
	}
	types, err := registry(cfg.kinds)
	if err != nil {
		return nil, err
	}
	opts = opts.WithDefaults()
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("filestore: %s is not a directory", dir)
	}
	var warnings []string
	if err := checkTree(dir); err != nil {
		if !errors.Is(err, ErrInsecure) {
			return nil, err
		}
		warnings = append(warnings, strings.TrimPrefix(err.Error(), "filestore: ")+" (a dispatcher refuses to open it)")
	}
	for range readAttempts {
		v, again, err := readOnce(dir, opts, cfg, types)
		if again {
			continue
		}
		if err != nil {
			return nil, err
		}
		v.warnings = append(warnings, v.warnings...)
		return v, nil
	}
	return nil, fmt.Errorf("filestore: the snapshot of %s was replaced %d times while it was read (compactions in a row): try again", dir, readAttempts)
}

// readOnce loads dir once. again is set when the snapshot was replaced meanwhile.
func readOnce(dir string, opts store.Options, cfg config, types map[string]reflect.Type) (v *View, again bool, err error) {
	// A throwaway Store, for the parsing Open uses: no lock, no journal handle.
	s := &Store{dir: dir, opts: opts, clk: opts.Clock, log: slog.New(slog.DiscardHandler), cfg: cfg, types: types}
	state := map[store.Key]*entry{}

	snapPath := filepath.Join(dir, snapshotFile)
	var snapV uint64
	snap, err := os.Open(snapPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		snap = nil
	case err != nil:
		return nil, false, fmt.Errorf("filestore: read snapshot: %w", err)
	}
	var snapFI os.FileInfo
	if snap != nil {
		// Kept open until the check below, so its inode can't be reused by a later snapshot.
		defer snap.Close() //nolint:errcheck // read-only
		if snapFI, err = snap.Stat(); err != nil {
			return nil, false, fmt.Errorf("filestore: stat snapshot: %w", err)
		}
		data, err := io.ReadAll(snap)
		if err != nil {
			return nil, false, fmt.Errorf("filestore: read snapshot: %w", err)
		}
		if snapV, err = s.parseSnapshot(data, state); err != nil {
			return nil, false, err
		}
	}

	if cfg.afterSnapshot != nil {
		cfg.afterSnapshot()
	}
	data, err := os.ReadFile(filepath.Join(dir, journalFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("filestore: read journal: %w", err)
	}
	var journal []JournalEntry
	version, _, replayErr := s.replayData(state, snapV, data, func(jl journalLine, v uint64) {
		journal = append(journal, JournalEntry{Version: v, At: jl.At, Op: jl.Op, Kind: jl.Kind, Name: jl.Name, Obj: jl.Obj})
	})

	// The journal fits the snapshot read only if no compaction renamed a new snapshot over it
	// before the journal was read; a gap in the versions (replayErr) is the same race.
	now, err := os.Stat(snapPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if snap != nil {
			return nil, true, nil
		}
	case err != nil:
		return nil, false, fmt.Errorf("filestore: stat snapshot: %w", err)
	case snap == nil || !os.SameFile(snapFI, now):
		return nil, true, nil
	}
	if replayErr != nil {
		return nil, false, replayErr
	}

	recs := make([]memory.Record, 0, len(state))
	s.loaded.Version, s.loaded.Snapshot, s.loaded.Objects = version, s.haveSnapshot, map[string]int{}
	for k, e := range state {
		recs = append(recs, memory.Record{Key: k, Type: types[k.Kind], Data: e.data, Version: e.v, Created: e.created, Updated: e.updated})
		s.loaded.Objects[k.Kind]++
	}
	mem := memory.New(opts)
	if err := mem.Restore(recs, version); err != nil {
		return nil, false, fmt.Errorf("filestore: %w: %w", ErrCorrupt, err)
	}
	v = &View{dir: dir, mem: mem, journal: journal}
	if s.loaded.TornLine {
		v.warnings = append(v.warnings, journalFile+": the last line is incomplete (a dispatcher is writing it, or crashed mid-append): read up to the line before it")
	}
	if v.runs, err = transcriptRuns(dir); err != nil {
		return nil, false, err
	}
	s.loaded.Transcripts = len(v.runs)
	v.loaded = s.loaded
	return v, false, nil
}

// transcriptRuns lists the runs with a transcript file in dir, sorted. Tmp files (a write in
// flight) and unexpected names are skipped.
func transcriptRuns(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, transcriptDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("filestore: read transcripts: %w", err)
	}
	var runs []string
	for _, e := range entries {
		if run, ok := runOfFile(e.Name()); ok && e.Type().IsRegular() {
			runs = append(runs, run)
		}
	}
	slices.Sort(runs)
	return runs, nil
}

// Dir returns the state dir read.
func (v *View) Dir() string { return v.dir }

// Loaded returns what was read: the version, the objects per kind, whether there was a
// snapshot, the journal lines above it, whether a torn last line was left out and the
// transcripts.
func (v *View) Loaded() LoadInfo {
	li := v.loaded
	li.Objects = maps.Clone(li.Objects)
	return li
}

// Warnings returns what Read noticed but read anyway: a dir that isn't private, a torn last
// journal line.
func (v *View) Warnings() []string { return slices.Clone(v.warnings) }

// Journal returns the journal lines above the snapshot, in version order. Changes folded into
// the snapshot by a compaction are no longer there.
func (v *View) Journal() []JournalEntry { return slices.Clone(v.journal) }

// Get reads one object, as store.Store's Get.
func (v *View) Get(ctx context.Context, k store.Key, out store.Object) error {
	return v.mem.Get(ctx, k, out)
}

// List lists a kind, as store.Store's List.
func (v *View) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	return v.mem.List(ctx, kind, opts, out)
}

// Transcripts returns the runs with a transcript, sorted.
func (v *View) Transcripts() []string { return slices.Clone(v.runs) }

// Transcript reads run's transcript file (gzip'd JSON Lines, as the agent uploaded it):
// store.ErrNotFound when there is none.
func (v *View) Transcript(run string) ([]byte, error) {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	file := transcriptFile(run)
	if run == "" || file == "" {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
	}
	data, err := os.ReadFile(filepath.Join(v.dir, transcriptDir, file))
	if errors.Is(err, os.ErrNotExist) {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
	}
	if err != nil {
		return nil, store.Errorf("transcript", k, err, "read")
	}
	return data, nil
}

// LockPath returns the lock file of the state dir dir: a store holds an exclusive flock on it
// while it has the dir open.
func LockPath(dir string) string { return filepath.Join(dir, lockFile) }

// MoveAside renames the state dir dir to dst (which must not exist), for a clean start that
// keeps the old state as evidence. It holds the dir's lock while it renames, so it fails with
// ErrLocked while a store has the dir open, and no store opens the dir midway.
func MoveAside(dir, dst string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("filestore: %s is not a directory", dir)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("filestore: %s: %w", dst, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("filestore: %w", err)
	}
	lock, err := lockDir(LockPath(dir))
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // closing releases the flock; nothing was written
	if err := os.Rename(dir, dst); err != nil {
		return fmt.Errorf("filestore: move the state dir aside: %w", err)
	}
	return nil
}

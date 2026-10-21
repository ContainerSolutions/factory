package filestore

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// Compaction steps, for the crash tests' fault injection.
const (
	stepSnapshotWritten  = "snapshot-written"  // snapshot.json.tmp written and fsync'd
	stepSnapshotRenamed  = "snapshot-renamed"  // renamed over snapshot.json, dir fsync'd
	stepJournalTruncated = "journal-truncated" // journal rewritten with the lines above the snapshot
)

// snapshotObject is one object in snapshot.json. created, at and v are kept beside obj
// because kinds without metadata (Event) don't carry them.
type snapshotObject struct {
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	V       string          `json:"v"`
	Created time.Time       `json:"created"`
	At      time.Time       `json:"at"`
	Obj     json.RawMessage `json:"obj"`
}

// readSnapshot loads snapshot.json into state and returns its version (0 when there is none).
func (s *Store) readSnapshot(state map[store.Key]*entry) (uint64, error) {
	path := filepath.Join(s.dir, snapshotFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("filestore: read snapshot: %w", err)
	}
	s.haveSnapshot = true
	var hdr struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(data, &hdr); err != nil {
		return 0, fmt.Errorf("filestore: %w: %s: %w", ErrCorrupt, snapshotFile, err)
	}
	if hdr.Format != Format {
		return 0, fmt.Errorf("filestore: %w: %s has format %d and this dispatcher reads format %d: move the state dir aside (mv %s %s.old) to start with an empty one",
			ErrFormat, path, hdr.Format, Format, s.dir, s.dir)
	}
	var snap struct {
		StoreVersion string           `json:"storeVersion"`
		Objects      []snapshotObject `json:"objects"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return 0, fmt.Errorf("filestore: %w: %s: %w", ErrCorrupt, snapshotFile, err)
	}
	version, err := store.ParseVersion("load", store.Key{}, snap.StoreVersion)
	if err != nil {
		return 0, fmt.Errorf("filestore: %w: %s: storeVersion %q", ErrCorrupt, snapshotFile, snap.StoreVersion)
	}
	for i, o := range snap.Objects {
		k := store.Key{Kind: o.Kind, Name: o.Name}
		bad := func(format string, args ...any) error {
			return fmt.Errorf("filestore: %w: %s object %d (%s): %s", ErrCorrupt, snapshotFile, i, k, fmt.Sprintf(format, args...))
		}
		if k.Kind == "" || k.Name == "" {
			return 0, bad("missing kind or name")
		}
		if s.types[k.Kind] == nil {
			return 0, bad("%v", unknownKind(k.Kind))
		}
		v, err := store.ParseVersion("load", k, o.V)
		if err != nil || v == 0 || v > version {
			return 0, bad("version %q outside 1..%d", o.V, version)
		}
		if len(o.Obj) == 0 {
			return 0, bad("missing obj")
		}
		if state[k] != nil {
			return 0, bad("duplicate")
		}
		state[k] = &entry{v: v, created: o.Created, updated: o.At, data: o.Obj}
	}
	return version, nil
}

// maybeCompact compacts when a write took the journal past its limits. It runs on the writer's
// goroutine, after the write returned from the memory store; writers that find a compaction
// running don't wait for it. A failure is logged (the write itself succeeded) and retried
// after more lines.
func (s *Store) maybeCompact() {
	if !s.needCompact.Load() || !s.compactMu.TryLock() {
		return
	}
	defer s.compactMu.Unlock()
	if !s.needCompact.Swap(false) {
		return // another writer just did it
	}
	if err := s.compactLocked(); err != nil {
		s.jmu.Lock()
		s.retryAt = len(s.marks) + max(1, s.cfg.compactLines/16)
		s.jmu.Unlock()
		s.log.Error("store: compaction failed; the journal keeps growing until it works", "err", err)
	}
}

// compactLocked folds the journal into the snapshot. The caller holds compactMu. Writes go on
// meanwhile: the snapshot is the memory store at version V, and the journal keeps the lines
// above V. A crash at any step leaves a loadable dir:
//
//  1. snapshot.json.tmp written and fsync'd: the old snapshot and the full journal; the tmp is
//     removed at load.
//  2. renamed over snapshot.json and the dir fsync'd: the new snapshot and the full journal,
//     whose lines at or below V are skipped at load.
//  3. the lines above V written to journal.jsonl.tmp, fsync'd, renamed over journal.jsonl and
//     the dir fsync'd: the new snapshot and the short journal.
func (s *Store) compactLocked() error {
	recs, v := s.mem.Snapshot()
	slices.SortFunc(recs, func(a, b memory.Record) int {
		return cmp.Or(cmp.Compare(a.Key.Kind, b.Key.Kind), cmp.Compare(a.Key.Name, b.Key.Name))
	})
	path := filepath.Join(s.dir, snapshotFile)
	tmp := path + tmpSuffix
	if err := writeSnapshot(tmp, recs, v); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := s.fault(stepSnapshotWritten); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("compaction: rename snapshot: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	s.jmu.Lock()
	s.haveSnapshot = true
	s.jmu.Unlock()
	if err := s.fault(stepSnapshotRenamed); err != nil {
		return err
	}

	s.jmu.Lock()
	err := s.truncateJournalLocked(v)
	s.jmu.Unlock()
	if err != nil {
		return err
	}
	if err := s.fault(stepJournalTruncated); err != nil {
		return err
	}
	s.log.Debug("store: compacted", "version", v, "objects", len(recs))
	return nil
}

// writeSnapshot writes the snapshot to path and fsyncs it: one object per line, so it greps.
func writeSnapshot(path string, recs []memory.Record, v uint64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("compaction: create snapshot: %w", err)
	}
	defer f.Close() //nolint:errcheck // closed explicitly below on the success path
	w := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintf(w, `{"format":%d,"storeVersion":"%d","objects":[`, Format, v)
	for i, r := range recs {
		b, err := json.Marshal(snapshotObject{
			Kind: r.Key.Kind, Name: r.Key.Name, V: store.FormatVersion(r.Version),
			Created: r.Created, At: r.Updated, Obj: r.Data,
		})
		if err != nil {
			return fmt.Errorf("compaction: encode %s: %w", r.Key, err)
		}
		if i > 0 {
			_ = w.WriteByte(',')
		}
		_ = w.WriteByte('\n')
		_, _ = w.Write(b)
	}
	_, _ = w.WriteString("\n]}\n")
	if err := w.Flush(); err != nil {
		return fmt.Errorf("compaction: write snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("compaction: fsync snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("compaction: close snapshot: %w", err)
	}
	return nil
}

// truncateJournalLocked drops the journal lines at or below v (they are in the snapshot), by
// writing the rest to a tmp file and renaming it over the journal. The caller holds jmu.
func (s *Store) truncateJournalLocked(v uint64) error {
	i := sort.Search(len(s.marks), func(i int) bool { return s.marks[i].v > v })
	if i == 0 {
		return nil
	}
	start := s.marks[i-1].end
	tail := make([]byte, s.jsize-start)
	if _, err := s.jf.ReadAt(tail, start); err != nil {
		return fmt.Errorf("compaction: read journal tail: %w", err)
	}
	path := filepath.Join(s.dir, journalFile)
	tmp := path + tmpSuffix
	if err := writeFileSync(tmp, tail); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compaction: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compaction: rename journal: %w", err)
	}
	// From here the open handle is the old, unlinked journal: switch, or stop writing.
	nf, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		s.broken = fmt.Errorf("reopen the journal after compaction: %w", err)
		return fmt.Errorf("compaction: %w", s.broken)
	}
	_ = s.jf.Close()
	s.jf = nf
	s.jsize -= start
	marks := make([]mark, 0, len(s.marks)-i)
	for _, m := range s.marks[i:] {
		marks = append(marks, mark{v: m.v, end: m.end - start})
	}
	s.marks = marks
	s.retryAt = 0
	if err := syncDir(s.dir); err != nil {
		// Appends to the new journal might not survive a crash: stop writing.
		s.broken = err
		return fmt.Errorf("compaction: %w", err)
	}
	return nil
}

// writeFileSync writes data to a new 0600 file at path and fsyncs it.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

// syncDir fsyncs a directory, so the renames and creates in it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s for fsync: %w", dir, err)
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

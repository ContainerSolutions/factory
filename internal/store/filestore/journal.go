package filestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// journalLine is one line of journal.jsonl: one committed change.
type journalLine struct {
	V    string          `json:"v"`
	At   time.Time       `json:"at"`
	Op   memory.Op       `json:"op"`
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Obj  json.RawMessage `json:"obj,omitzero"`
}

// journalChange is the memory store's commit hook: it appends c to the journal and fsyncs it,
// so the write is applied (and seen by readers and watchers) only once it is on disk. It runs
// under the memory store's write lock, so lines are in version order.
func (s *Store) journalChange(c memory.Change) error {
	line, err := json.Marshal(journalLine{
		V: store.FormatVersion(c.Version), At: c.At, Op: c.Op, Kind: c.Key.Kind, Name: c.Key.Name, Obj: c.Data,
	})
	if err != nil {
		return fmt.Errorf("journal: encode: %w", err)
	}
	if err := s.append(append(line, '\n'), c.Version); err != nil {
		return err
	}
	if c.Op == memory.OpDelete && c.Key.Kind == v1alpha1.KindRun {
		s.removeTranscript(c.Key.Name)
	}
	return nil
}

// append writes one line and fsyncs the journal (one fsync per write). A failed write is
// rolled back by truncating to the last good line; a failed fsync, or a rollback that fails,
// leaves the journal untrusted (ErrJournal) until the store is reopened.
//
// Group commit would go here: append under the lock, then wait for one fsync shared by the
// lines appended within a few milliseconds. Not built: one fsync per write is enough at PoC
// scale.
func (s *Store) append(line []byte, v uint64) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.broken != nil {
		return fmt.Errorf("%w: %w", ErrJournal, s.broken)
	}
	s.fmu.Lock()
	write, sync := s.faults.write, s.faults.sync
	s.fmu.Unlock()
	if write == nil {
		write = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	}
	if sync == nil {
		sync = func(f *os.File) error { return f.Sync() }
	}

	if _, err := write(s.jf, line); err != nil {
		if terr := s.jf.Truncate(s.jsize); terr != nil {
			s.broken = fmt.Errorf("roll back a failed append: %w", terr)
		}
		return fmt.Errorf("journal: append: %w", err)
	}
	if err := sync(s.jf); err != nil {
		// The line may or may not reach the disk, and after a failed fsync the page cache
		// can't be trusted either: drop the line and stop writing.
		_ = s.jf.Truncate(s.jsize)
		s.broken = fmt.Errorf("fsync: %w", err)
		return fmt.Errorf("journal: fsync: %w", err)
	}
	s.jsize += int64(len(line))
	s.marks = append(s.marks, mark{v: v, end: s.jsize})
	if n := len(s.marks); (s.jsize >= s.cfg.compactBytes || n >= s.cfg.compactLines) && n >= s.retryAt {
		s.needCompact.Store(true)
	}
	return nil
}

// entry is one object while loading.
type entry struct {
	v                uint64
	created, updated time.Time
	data             []byte
}

// load reads the snapshot and replays the journal into the memory store, truncates a torn
// last line, opens the journal for appending and indexes the transcripts.
func (s *Store) load() error {
	for _, f := range []string{snapshotFile + tmpSuffix, journalFile + tmpSuffix} {
		// Leftovers of a compaction that crashed: the files they would replace are intact.
		if err := os.Remove(filepath.Join(s.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("filestore: remove %s: %w", f, err)
		}
	}
	state := map[store.Key]*entry{}
	snapV, err := s.readSnapshot(state)
	if err != nil {
		return err
	}
	version, good, err := s.replay(state, snapV)
	if err != nil {
		return err
	}

	recs := make([]memory.Record, 0, len(state))
	for k, e := range state {
		recs = append(recs, memory.Record{Key: k, Type: s.types[k.Kind], Data: e.data, Version: e.v, Created: e.created, Updated: e.updated})
	}
	if err := s.mem.Restore(recs, version); err != nil {
		return fmt.Errorf("filestore: %w: %w", ErrCorrupt, err)
	}

	path := filepath.Join(s.dir, journalFile)
	_, statErr := os.Stat(path)
	s.jf, err = os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("filestore: open journal: %w", err)
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := syncDir(s.dir); err != nil {
			return err
		}
	}
	if fi, err := s.jf.Stat(); err != nil {
		return fmt.Errorf("filestore: stat journal: %w", err)
	} else if fi.Size() != good {
		if err := s.jf.Truncate(good); err != nil {
			return fmt.Errorf("filestore: truncate the torn journal line: %w", err)
		}
		if err := s.jf.Sync(); err != nil {
			return fmt.Errorf("filestore: fsync journal: %w", err)
		}
	}
	s.jsize = good
	if n := len(s.marks); s.jsize >= s.cfg.compactBytes || n >= s.cfg.compactLines {
		s.needCompact.Store(true)
	}
	return s.loadTranscripts()
}

// replay applies the journal lines above snapV to state. It returns the store version and the
// length of the journal's good prefix (shorter than the file when the last line is torn), and
// sets s.marks.
func (s *Store) replay(state map[store.Key]*entry, snapV uint64) (version uint64, good int64, err error) {
	data, err := os.ReadFile(filepath.Join(s.dir, journalFile))
	if errors.Is(err, os.ErrNotExist) {
		return snapV, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("filestore: read journal: %w", err)
	}
	version = snapV
	off := 0
	for n := 1; off < len(data); n++ {
		end, complete := len(data), false
		line := data[off:]
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			end, complete, line = off+i+1, true, line[:i]
		}
		jl, v, perr := decodeLine(line)
		if !complete && perr == nil {
			perr = errors.New("no trailing newline")
		}
		if perr != nil {
			if end == len(data) {
				s.log.Warn("store: dropping a torn last journal line (a crash mid-append)",
					"line", n, "bytes", end-off, "err", perr)
				break
			}
			return 0, 0, fmt.Errorf("filestore: %w: %s line %d: %w", ErrCorrupt, journalFile, n, perr)
		}
		s.marks = append(s.marks, mark{v: v, end: int64(end)})
		off = end
		if v <= snapV {
			continue // in the snapshot already (a compaction crashed before truncating)
		}
		if v != version+1 {
			return 0, 0, fmt.Errorf("filestore: %w: %s line %d: version %d, want %d (a change is missing)", ErrCorrupt, journalFile, n, v, version+1)
		}
		if err := s.apply(state, jl, v); err != nil {
			return 0, 0, fmt.Errorf("filestore: %w: %s line %d: %w", ErrCorrupt, journalFile, n, err)
		}
		version = v
	}
	return version, int64(off), nil
}

// decodeLine parses and checks one journal line.
func decodeLine(line []byte) (journalLine, uint64, error) {
	var jl journalLine
	if err := json.Unmarshal(line, &jl); err != nil {
		return jl, 0, fmt.Errorf("invalid JSON: %w", err)
	}
	v, err := store.ParseVersion("load", store.Key{}, jl.V)
	if err != nil || v == 0 {
		return jl, 0, fmt.Errorf("bad version %q", jl.V)
	}
	if jl.Kind == "" || jl.Name == "" {
		return jl, 0, errors.New("missing kind or name")
	}
	switch jl.Op {
	case memory.OpCreate, memory.OpUpdate:
		if len(jl.Obj) == 0 {
			return jl, 0, fmt.Errorf("%s without obj", jl.Op)
		}
	case memory.OpDelete:
	default:
		return jl, 0, fmt.Errorf("unknown op %q", jl.Op)
	}
	return jl, v, nil
}

// apply replays one change.
func (s *Store) apply(state map[store.Key]*entry, jl journalLine, v uint64) error {
	k := store.Key{Kind: jl.Kind, Name: jl.Name}
	if s.types[k.Kind] == nil {
		return unknownKind(k.Kind)
	}
	old := state[k]
	switch jl.Op {
	case memory.OpCreate:
		if old != nil {
			return fmt.Errorf("create %s: it exists", k)
		}
		state[k] = &entry{v: v, created: jl.At, updated: jl.At, data: jl.Obj}
	case memory.OpUpdate:
		if old == nil {
			return fmt.Errorf("update %s: it doesn't exist", k)
		}
		state[k] = &entry{v: v, created: old.created, updated: jl.At, data: jl.Obj}
	case memory.OpDelete:
		if old == nil {
			return fmt.Errorf("delete %s: it doesn't exist", k)
		}
		delete(state, k)
	}
	return nil
}

func unknownKind(kind string) error {
	return fmt.Errorf("kind %q has no Go type registered (filestore.WithKind)", kind)
}

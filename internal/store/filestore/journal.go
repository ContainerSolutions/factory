package filestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// journalLine is one line of journal.jsonl: one committed change (P2's format, without the
// object store's writer and schema).
type journalLine = journal.Line

// journalChange is the memory store's commit hook: it appends c to the journal and fsyncs it,
// so the write is applied (and seen by readers and watchers) only once it is on disk. It runs
// under the memory store's write lock, so lines are in version order.
func (s *Store) journalChange(_ context.Context, c memory.Change) error {
	line, err := json.Marshal(journal.LineOf(c))
	if err != nil {
		return fmt.Errorf("journal: encode: %w", err)
	}
	if err := s.append(append(line, '\n'), c.Version); err != nil {
		return err
	}
	if c.Op == memory.OpDelete && c.Key.Kind == v1alpha1.KindRun {
		s.removeTranscript(c.Key.Name)
		s.removeTranscript(store.EnvironmentLogBlob(c.Key.Name))
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

// load reads the snapshot and replays the journal into the memory store, truncates a torn
// last line, opens the journal for appending and indexes the transcripts.
func (s *Store) load() error {
	for _, f := range []string{snapshotFile + tmpSuffix, journalFile + tmpSuffix} {
		// Leftovers of a compaction that crashed: the files they would replace are intact.
		if err := os.Remove(filepath.Join(s.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("filestore: remove %s: %w", f, err)
		}
	}
	state := journal.State{}
	snapV, err := s.readSnapshot(state)
	if err != nil {
		return err
	}
	version, good, err := s.replay(state, snapV)
	if err != nil {
		return err
	}

	recs, counts := state.Records(s.types)
	s.loaded.Version, s.loaded.Snapshot, s.loaded.Objects = version, s.haveSnapshot, counts
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
	if err := s.loadTranscripts(); err != nil {
		return err
	}
	s.loaded.Transcripts = len(s.blobs)
	return nil
}

// replay applies the journal lines above snapV to state. It returns the store version and the
// length of the journal's good prefix (shorter than the file when the last line is torn), and
// sets s.marks.
func (s *Store) replay(state journal.State, snapV uint64) (version uint64, good int64, err error) {
	data, err := os.ReadFile(filepath.Join(s.dir, journalFile))
	if errors.Is(err, os.ErrNotExist) {
		return snapV, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("filestore: read journal: %w", err)
	}
	return s.replayData(state, snapV, data, nil)
}

// replayData is replay on the journal's bytes. visit, when not nil, is called with each line
// it applies (those above snapV), in order.
func (s *Store) replayData(state journal.State, snapV uint64, data []byte, visit func(journalLine, uint64)) (version uint64, good int64, err error) {
	version = snapV
	off := 0
	for n := 1; off < len(data); n++ {
		end, complete := len(data), false
		line := data[off:]
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			end, complete, line = off+i+1, true, line[:i]
		}
		jl, v, perr := journal.DecodeLine(line)
		if !complete && perr == nil {
			perr = errors.New("no trailing newline")
		}
		if perr != nil {
			if end == len(data) {
				s.log.Warn("store: dropping a torn last journal line (a crash mid-append)",
					"line", n, "bytes", end-off, "err", perr)
				s.loaded.TornLine = true
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
		if err := state.Apply(s.types, jl, v); err != nil {
			return 0, 0, fmt.Errorf("filestore: %w: %s line %d: %w", ErrCorrupt, journalFile, n, err)
		}
		version = v
		s.loaded.JournalLines++
		if visit != nil {
			visit(jl, v)
		}
	}
	return version, int64(off), nil
}

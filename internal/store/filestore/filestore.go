// Package filestore is the single-node durable store: the in-memory store (store/memory) is
// the engine, and every committed change is appended to an fsync'd JSON Lines journal before it
// is applied. A compaction folds the journal into a snapshot; Open loads the snapshot, replays
// the journal and restores the version counter. Transcripts are files.
//
//	<dir>/                        0700
//	  snapshot.json               {"format":1,"storeVersion":"N","objects":[…]} (atomic rename)
//	  journal.jsonl               one line per change above the snapshot, fsync'd
//	  transcripts/<run>.jsonl.gz  one file per run
//	  LOCK                        flock: one store per dir
//
// Design: design/workstreams/dispatcher-runtime/LLD/store.md §File store.
package filestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// Files in the state dir.
const (
	snapshotFile  = journal.FileStoreSnapshot
	journalFile   = journal.FileStoreJournal
	lockFile      = journal.FileStoreLock
	transcriptDir = journal.TranscriptDir
	tmpSuffix     = ".tmp"
)

// Format is the snapshot format this package reads and writes.
const Format = journal.Format

// Compaction defaults: the journal is folded into the snapshot once it passes either.
const (
	DefaultCompactBytes = 64 << 20
	DefaultCompactLines = 50_000
)

// Errors Open and the writes return (test with errors.Is).
var (
	// ErrLocked: another store has the state dir open.
	ErrLocked = errors.New("state dir is locked by another store")
	// ErrInsecure: the state dir or a file in it is readable or writable by group or others,
	// or owned by another user.
	ErrInsecure = errors.New("state dir is not private")
	// ErrCorrupt: the snapshot or a journal line (other than a torn last one) can't be loaded.
	ErrCorrupt = errors.New("state is corrupt")
	// ErrFormat: the snapshot has a format this package doesn't read.
	ErrFormat = errors.New("unknown state format")
	// ErrClosed: a write after Close.
	ErrClosed = errors.New("store is closed")
	// ErrObjectStoreDir: the dir holds an object store's log (log/ or snapshots/: the file://
	// bucket of an object store, P3), which the file store must never share a dir with.
	ErrObjectStoreDir = errors.New("dir holds an object store's state")
	// ErrJournal: an earlier journal fsync (or rollback) failed, so the journal can't be
	// trusted; every write fails until the store is reopened.
	ErrJournal = errors.New("journal unusable after an earlier failure; reopen the store")
)

// Option configures Open.
type Option func(*config)

type config struct {
	kinds        map[string]store.Object
	compactBytes int64
	compactLines int
	// afterSnapshot, when not nil, is called by Read between reading the snapshot and the
	// journal (tests race a compaction there).
	afterSnapshot func()
}

// WithKind registers the Go type of kind, a stored kind outside v1alpha1 (the internal Lease,
// Session, Delivery and the like): proto is a pointer to its zero value. Open needs the type to
// load the kind's objects, and writes of a kind without one are ErrInvalid, so a kind that
// could not be loaded back is never stored. The v1alpha1 runtime kinds are built in.
func WithKind(kind string, proto store.Object) Option {
	return func(c *config) { c.kinds[kind] = proto }
}

// WithCompaction sets the journal size, in bytes, and length, in lines, past which a write
// compacts it into the snapshot (defaults DefaultCompactBytes and DefaultCompactLines).
func WithCompaction(maxBytes int64, maxLines int) Option {
	return func(c *config) {
		if maxBytes > 0 {
			c.compactBytes = maxBytes
		}
		if maxLines > 0 {
			c.compactLines = maxLines
		}
	}
}

// builtinKinds are the v1alpha1 kinds the store holds.
var builtinKinds = journal.BuiltinKinds

// Store is the file-backed store. Use Open, and Close when done.
type Store struct {
	dir    string
	opts   store.Options
	clk    clock.Clock
	log    *slog.Logger
	cfg    config
	types  map[string]reflect.Type
	mem    *memory.Store
	lock   *os.File
	loaded LoadInfo // set by Open, read-only after

	// jmu guards the journal. Lock order: memory's write lock (the commit hook runs under
	// it), then jmu, then tmu.
	jmu          sync.Mutex
	jf           *os.File
	jsize        int64
	marks        []mark // one per journal line, in order
	closed       bool
	broken       error // set when the journal can't be trusted (see ErrJournal)
	retryAt      int   // after a failed compaction, the line count at which to try again
	haveSnapshot bool

	compactMu   sync.Mutex // one compaction at a time
	needCompact atomic.Bool
	isClosed    atomic.Bool

	tmu       sync.Mutex
	blobs     map[string]*tblob
	blobTotal int64
	blobSeq   uint64

	fmu    sync.Mutex
	faults faults
}

// LoadInfo is what Open loaded from the state dir, for the caller's start-up log.
type LoadInfo struct {
	// Version is the store version restored (0 for a new dir).
	Version uint64
	// Objects counts the objects loaded, by kind.
	Objects map[string]int
	// Snapshot is whether there was a snapshot; JournalLines counts the journal lines
	// replayed above it.
	Snapshot     bool
	JournalLines int
	// TornLine is whether a torn last journal line (a crash mid-append) was dropped.
	TornLine bool
	// Transcripts counts the transcript files found.
	Transcripts int
}

// Loaded returns what Open loaded.
func (s *Store) Loaded() LoadInfo {
	li := s.loaded
	li.Objects = maps.Clone(li.Objects)
	return li
}

// mark is a journal line's version and the offset just past it.
type mark struct {
	v   uint64
	end int64
}

// faults inject failures in tests. Nil fields don't.
type faults struct {
	write   func(f *os.File, line []byte) (int, error) // replaces the journal write
	sync    func(f *os.File) error                     // replaces the journal fsync
	compact func(step string) error                    // called after each compaction step
}

var (
	_ store.Store           = (*Store)(nil)
	_ store.Sweeper         = (*Store)(nil)
	_ store.TranscriptStore = (*Store)(nil)
	_ io.Closer             = (*Store)(nil)
)

// Open opens (creating it if missing) the state dir and loads it: the snapshot, then the
// journal lines above the snapshot's version. A torn last journal line (a crash mid-append) is
// dropped with a warning and the journal truncated to the last good line; anything else that
// can't be loaded fails Open with ErrCorrupt and where. The dir must be private (0700, files
// 0600, owned by this user: ErrInsecure says how to fix it) and not open in another store
// (ErrLocked). opts are the memory store's.
func Open(dir string, opts store.Options, options ...Option) (*Store, error) {
	cfg := config{kinds: map[string]store.Object{}, compactBytes: DefaultCompactBytes, compactLines: DefaultCompactLines}
	for _, o := range options {
		o(&cfg)
	}
	types, err := registry(cfg.kinds)
	if err != nil {
		return nil, err
	}
	opts = opts.WithDefaults()
	s := &Store{
		dir: dir, opts: opts, clk: opts.Clock, log: opts.Logger, cfg: cfg, types: types,
		blobs: map[string]*tblob{},
	}
	s.mem = memory.New(opts, memory.WithCommitHook(s.journalChange))

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: create state dir: %w", err)
	}
	if err := checkNotObjectStore(dir); err != nil {
		return nil, err
	}
	if err := checkTree(dir); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(dir, transcriptDir), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("filestore: create transcripts dir: %w", err)
	}
	if s.lock, err = lockDir(filepath.Join(dir, lockFile)); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		_ = s.lock.Close() // releases the flock
		if s.jf != nil {
			_ = s.jf.Close()
		}
		return nil, err
	}
	return s, nil
}

// registry returns the Go type of every kind the store may hold.
func registry(extra map[string]store.Object) (map[string]reflect.Type, error) {
	types, err := journal.Registry(extra)
	if err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	return types, nil
}

// checkType rejects a write of a kind the store couldn't load back. Writes that are invalid
// anyway are left to the memory store, so its errors stay the conformant ones.
func (s *Store) checkType(op string, obj store.Object) error {
	return journal.CheckType(s.types, op, obj, "file store")
}

// Get implements store.Store.
func (s *Store) Get(ctx context.Context, k store.Key, out store.Object) error {
	return s.mem.Get(ctx, k, out)
}

// List implements store.Store.
func (s *Store) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	return s.mem.List(ctx, kind, opts, out)
}

// Create implements store.Store. The change is journaled before it is applied.
func (s *Store) Create(ctx context.Context, obj store.Object) error {
	if err := s.checkType("create", obj); err != nil {
		return err
	}
	if err := s.mem.Create(ctx, obj); err != nil {
		return err
	}
	s.maybeCompact()
	return nil
}

// Update implements store.Store. The change is journaled before it is applied.
func (s *Store) Update(ctx context.Context, obj store.Object) error {
	if err := s.checkType("update", obj); err != nil {
		return err
	}
	if err := s.mem.Update(ctx, obj); err != nil {
		return err
	}
	s.maybeCompact()
	return nil
}

// Delete implements store.Store. The change is journaled before it is applied; a Run's
// transcript goes with it.
func (s *Store) Delete(ctx context.Context, k store.Key) error {
	if err := s.mem.Delete(ctx, k); err != nil {
		return err
	}
	s.maybeCompact()
	return nil
}

// Watch implements store.Store. After a reopen, watches start from the restored version: the
// history isn't persisted, so an older SinceVersion is ErrWatchExpired.
func (s *Store) Watch(ctx context.Context, kind string, opts store.WatchOptions) (store.Watcher, error) {
	return s.mem.Watch(ctx, kind, opts)
}

// Lock implements store.Store. Locks are not persisted: they are advisory and lease-based,
// and a restart frees them.
func (s *Store) Lock(ctx context.Context, name string, ttl time.Duration) (store.Unlock, error) {
	return s.mem.Lock(ctx, name, ttl)
}

// Sweep implements store.Sweeper. Its deletes are journaled like any other.
func (s *Store) Sweep(ctx context.Context) (store.SweepResult, error) {
	res, err := s.mem.Sweep(ctx)
	if err != nil {
		return res, err
	}
	if n := s.sweepTranscripts(s.clk.Now()); n > 0 {
		res.Transcripts += n
		s.log.InfoContext(ctx, "store: swept transcripts", "transcripts", n, "retention", s.opts.Retention.String())
	}
	s.maybeCompact()
	return res, nil
}

// Close compacts the journal into the snapshot and releases the state dir. Writes after Close
// fail with ErrClosed; reads still answer from memory. It is idempotent.
func (s *Store) Close() error {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()
	s.jmu.Lock()
	if s.closed {
		s.jmu.Unlock()
		return nil
	}
	s.closed = true
	compact := s.jsize > 0 || !s.haveSnapshot
	s.jmu.Unlock()
	s.tmu.Lock() // after a transcript write in flight
	s.isClosed.Store(true)
	s.tmu.Unlock()

	var errs []error
	if compact {
		if err := s.compactLocked(); err != nil {
			errs = append(errs, fmt.Errorf("filestore: final compaction: %w", err))
		}
	}
	s.jmu.Lock()
	if err := s.jf.Close(); err != nil {
		errs = append(errs, fmt.Errorf("filestore: close journal: %w", err))
	}
	s.jmu.Unlock()
	if err := s.lock.Close(); err != nil { // releases the flock
		errs = append(errs, fmt.Errorf("filestore: release lock: %w", err))
	}
	return errors.Join(errs...)
}

// fault returns the injected failure for step, if any.
func (s *Store) fault(step string) error {
	s.fmu.Lock()
	f := s.faults.compact
	s.fmu.Unlock()
	if f == nil {
		return nil
	}
	return f(step)
}

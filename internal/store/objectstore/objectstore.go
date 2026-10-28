// Package objectstore is the store several dispatchers share: the in-memory store
// (store/memory) is the engine in each of them, and the bucket holds the authoritative
// history, an append-only log of create-only objects, one per change, plus periodic
// snapshots. A change is committed when its log object is created, and creation is
// conditional on the name being free, so two dispatchers can't both take one version.
//
//	<prefix>/
//	  log/00000000000000001043.json            one change: P2's journal line, the writer and its schema
//	  snapshots/00000000000000001000.json.gz   the whole store at version 1000 (P2's snapshot, gzip'd)
//	  transcripts/<run>.jsonl.gz               one blob per transcript or environment log (P2's naming)
//
// Reads are served from memory, which syncs (reads the log from its version + 1 until an entry
// is missing) before a read older than the sync interval, on a Get that finds nothing, after a
// lost race, and in the background. Locks are StoreLock records and transcript sizes are
// TranscriptBlob records, both written through the log.
//
// Design: proposal P3, "The dispatcher keeps its state in object storage", and
// design/workstreams/dispatcher-runtime/LLD/store.md §Object store.
package objectstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// Defaults (P3).
const (
	// DefaultSyncInterval is how stale a read may be before it syncs, and the background
	// sync's period (--state-sync-interval).
	DefaultSyncInterval = 2 * time.Second
	// DefaultMaxLag: a store whose last successful sync started longer ago reloads from a
	// listing before it writes, and isn't Ready.
	DefaultMaxLag = time.Hour
	// DefaultSegmentRetention is how old a log entry covered by a snapshot, and a snapshot that
	// isn't one of the newest two, must be before they are deleted.
	DefaultSegmentRetention = 7 * 24 * time.Hour
	// DefaultCompactEvery is how many log entries past the newest snapshot make the sweep
	// compact.
	DefaultCompactEvery = 1000
	// DefaultRequestTimeout bounds each bucket request.
	DefaultRequestTimeout = 5 * time.Second
	// MaxLostRaces is how many lost races in a row fail a write (ErrContended).
	MaxLostRaces = 20
)

// Internal timings.
const (
	// lockSync is how often a Lock waiting on a held lock syncs, to see other dispatchers'
	// releases.
	lockSync = time.Second
	// maintenanceEvery is how often the sweep runs the log and snapshot retention and the
	// transcript orphan scan.
	maintenanceEvery = 24 * time.Hour
	// orphanAge is how long a transcript blob may have no TranscriptBlob record before the
	// orphan scan deletes it (a crash between the blob's put and its record).
	orphanAge = 24 * time.Hour
	// createAttempts bounds the creates of one log entry whose outcome stays unknown.
	createAttempts = 2
	// loadAttempts bounds the cold starts that begin again because a snapshot or an entry was
	// deleted while they read.
	loadAttempts = 5
	// loadParallel is how many log entries a cold start reads at once.
	loadParallel = 16
	// closeTimeout bounds Close's final compaction.
	closeTimeout = time.Minute
)

// Errors (test with errors.Is).
var (
	// ErrClosed: a write after Close.
	ErrClosed = errors.New("store is closed")
	// ErrContended: a write lost MaxLostRaces races in a row for the next log entry.
	ErrContended = errors.New("lost too many races for the next log entry")
	// ErrSchema: the newest snapshot was written by a newer schema than this release reads, so
	// Open refuses to start.
	ErrSchema = errors.New("state written by a newer schema")
	// ErrCorrupt: a log entry or snapshot can't be loaded, or the log has a gap no snapshot
	// covers.
	ErrCorrupt = errors.New("state is corrupt")
	// errBehind: another dispatcher committed the version this write took (a lost race). The
	// write syncs and runs again.
	errBehind = errors.New("another dispatcher committed this version first")
	// errMoved: a snapshot or log entry was deleted while a load read it; it starts again.
	errMoved = errors.New("the log moved while it was read")
)

// Option configures Open.
type Option func(*config)

type config struct {
	kinds            map[string]store.Object
	syncInterval     time.Duration
	maxLag           time.Duration
	segmentRetention time.Duration
	compactEvery     uint64
	requestTimeout   time.Duration
	writerID         string
	// schema and readable are store.SchemaVersion and store.ReadableSchema, set otherwise by
	// the tests of two releases on one log.
	schema, readable int
	// noBackground stops Open from starting the background sync (tests drive it).
	noBackground bool
}

// WithKind registers the Go type of kind, a stored kind outside v1alpha1 (the internal Lease,
// Session, Delivery and the like), as filestore.WithKind does: syncs and loads need each
// kind's type, and a write of a kind without one is ErrInvalid. StoreLock and TranscriptBlob
// are always registered.
func WithKind(kind string, proto store.Object) Option {
	return func(c *config) { c.kinds[kind] = proto }
}

// WithSyncInterval sets how stale a read may be before it syncs, and the background sync's
// period (default DefaultSyncInterval).
func WithSyncInterval(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.syncInterval = d
		}
	}
}

// WithMaxLag sets the lag past which a store reloads before writing and isn't Ready (default
// DefaultMaxLag). It must be under the segment retention.
func WithMaxLag(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.maxLag = d
		}
	}
}

// WithSegmentRetention sets how long covered log entries and old snapshots are kept (default
// DefaultSegmentRetention).
func WithSegmentRetention(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.segmentRetention = d
		}
	}
}

// WithCompactEvery sets how many log entries past the newest snapshot make the sweep compact
// (default DefaultCompactEvery).
func WithCompactEvery(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.compactEvery = uint64(n)
		}
	}
}

// WithRequestTimeout sets each bucket request's deadline (default DefaultRequestTimeout).
func WithRequestTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.requestTimeout = d
		}
	}
}

// WithWriterID sets the store's instance id, which every log entry it writes and every lock it
// holds carry (default random). Two stores on one bucket must not share one.
func WithWriterID(id string) Option {
	return func(c *config) { c.writerID = id }
}

// LoadInfo is what Open loaded, for the caller's start-up log.
type LoadInfo struct {
	// Writer is this store's instance id.
	Writer string
	// Version is the store version loaded (0 for an empty bucket).
	Version uint64
	// Objects counts the objects loaded, by kind.
	Objects map[string]int
	// Snapshot is the version of the snapshot loaded (0 when there was none); LogEntries
	// counts the log entries replayed above it.
	Snapshot   uint64
	LogEntries int
	// Transcripts counts the transcript blobs indexed.
	Transcripts int
	// ReadOnly says the log holds an entry past this release's ReadableSchema: the store
	// stopped writing (Ready says why).
	ReadOnly bool
}

// Store is the object store. Use Open, and Close when done.
type Store struct {
	b     Bucket
	opts  store.Options
	clk   clock.Clock
	log   *slog.Logger
	cfg   config
	types map[string]reflect.Type
	mem   *memory.Store
	id    string

	// syncMu serialises syncs and reloads.
	syncMu sync.Mutex

	// mu guards the fields below. Lock order: syncMu, then memory's lock, then mu.
	mu          sync.Mutex
	syncGen     uint64    // syncs started
	okGen       uint64    // the last successful sync's number
	lastOK      time.Time // when the last successful sync started, on the store's clock
	synced      bool      // a sync (or the load) has succeeded
	syncFailing bool      // the last background or read-path sync failed (logged once)
	readOnly    error     // why the store stopped writing (wraps store.ErrReadOnly), once it has
	maxSchema   int       // the newest schema among the entries applied
	snapV       uint64    // the newest snapshot this store knows of
	closed      bool
	lastMaint   time.Time // the last log retention and orphan scan (zero: the first sweep runs one)
	lockWake    chan struct{}
	loaded      LoadInfo

	// tmu serialises this store's transcript puts (the cap is read, then written).
	tmu sync.Mutex

	// collectLog keeps the log entries a load applies (Read's).
	collectLog bool

	stopLoop context.CancelFunc
	loopDone chan struct{}
}

var (
	_ store.Store           = (*Store)(nil)
	_ store.Sweeper         = (*Store)(nil)
	_ store.TranscriptStore = (*Store)(nil)
	_ store.Syncer          = (*Store)(nil)
	_ io.Closer             = (*Store)(nil)
)

// Open loads the state from b (the newest snapshot, then the log above it) and returns the
// store, with its background sync running. An empty bucket is a new, empty state. Open refuses
// a snapshot written by a newer schema than this release reads (ErrSchema) and a log it can't
// load (ErrCorrupt, saying where); an entry past ReadableSchema in the log loads what comes
// before it and leaves the store read-only (Ready says so). opts are the memory store's.
func Open(ctx context.Context, b Bucket, opts store.Options, options ...Option) (*Store, error) {
	cfg := config{
		kinds:            ownKinds(),
		syncInterval:     DefaultSyncInterval,
		maxLag:           DefaultMaxLag,
		segmentRetention: DefaultSegmentRetention,
		compactEvery:     DefaultCompactEvery,
		requestTimeout:   DefaultRequestTimeout,
		schema:           store.SchemaVersion,
		readable:         store.ReadableSchema,
	}
	for _, o := range options {
		o(&cfg)
	}
	for k, proto := range ownKinds() {
		cfg.kinds[k] = proto
	}
	if cfg.maxLag >= cfg.segmentRetention {
		return nil, fmt.Errorf("objectstore: MaxLag %s must be under the segment retention %s, or a lagging writer could re-create a deleted log entry", cfg.maxLag, cfg.segmentRetention)
	}
	types, err := journal.Registry(cfg.kinds)
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	opts = opts.WithDefaults()
	s := &Store{
		b: b, opts: opts, clk: opts.Clock, log: opts.Logger, cfg: cfg, types: types,
		id: cfg.writerID, lockWake: make(chan struct{}),
	}
	if s.id == "" {
		s.id = "ins_" + randomHex(8)
	}
	s.mem = memory.New(opts, memory.WithCommitHook(s.commit))
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stopLoop, s.loopDone = cancel, make(chan struct{})
	if cfg.noBackground {
		close(s.loopDone)
	} else {
		go s.syncLoop(loopCtx)
	}
	return s, nil
}

// Loaded returns what Open loaded.
func (s *Store) Loaded() LoadInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	li := s.loaded
	li.Objects = maps.Clone(li.Objects)
	return li
}

// Ready says whether the store can serve: nil once its first sync (the load) has succeeded,
// while it still writes and while its last successful sync started within MaxLag. /readyz
// answers 503 with the error otherwise.
func (s *Store) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return fmt.Errorf("objectstore: %w", ErrClosed)
	case !s.synced:
		return errors.New("objectstore: not synced with the bucket yet")
	case s.readOnly != nil:
		return s.readOnly
	}
	if lag := s.clk.Now().Sub(s.lastOK); lag > s.cfg.maxLag {
		return fmt.Errorf("objectstore: the last successful sync started %s ago, past the maximum lag of %s", lag.Round(time.Second), s.cfg.maxLag)
	}
	return nil
}

// Get implements store.Store. It syncs first when the store's last sync is older than the
// sync interval, and once more before answering ErrNotFound, so an object another dispatcher
// just wrote is found.
func (s *Store) Get(ctx context.Context, k store.Key, out store.Object) error {
	asked := s.gen()
	s.freshen(ctx)
	err := s.mem.Get(ctx, k, out)
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if serr := s.syncAfter(ctx, asked); serr != nil {
		if errors.Is(serr, store.ErrReadOnly) {
			return err // reads go on from what the store has
		}
		return store.Errorf("get", k, serr, "sync before answering not found")
	}
	return s.mem.Get(ctx, k, out)
}

// List implements store.Store. It syncs first when the store's last sync is older than the
// sync interval. A caller that acts on an empty answer syncs and asks again first
// (store.SyncIfSupported).
func (s *Store) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	s.freshen(ctx)
	return s.mem.List(ctx, kind, opts, out)
}

// Watch implements store.Store. Changes applied from the log fire like local writes, in
// version order. After a cold start the history begins at the loaded version, so an older
// SinceVersion is ErrWatchExpired; a reload from a snapshot expires every watcher.
func (s *Store) Watch(ctx context.Context, kind string, opts store.WatchOptions) (store.Watcher, error) {
	return s.mem.Watch(ctx, kind, opts)
}

// Create implements store.Store. The change is committed by creating its log entry.
func (s *Store) Create(ctx context.Context, obj store.Object) error {
	if err := journal.CheckType(s.types, "create", obj, "object store"); err != nil {
		return err
	}
	k := keyOf(obj)
	return s.write(ctx, "create", k, func(ctx context.Context) error { return s.mem.Create(ctx, obj) })
}

// Update implements store.Store. The compare-and-swap is checked against memory, which a lost
// race brings up to the log's tail before the update runs again.
func (s *Store) Update(ctx context.Context, obj store.Object) error {
	if err := journal.CheckType(s.types, "update", obj, "object store"); err != nil {
		return err
	}
	k := keyOf(obj)
	return s.write(ctx, "update", k, func(ctx context.Context) error { return s.mem.Update(ctx, obj) })
}

// Delete implements store.Store. A Run's transcript blobs and their records go after the
// Run's own delete commits.
func (s *Store) Delete(ctx context.Context, k store.Key) error {
	return s.write(ctx, "delete", k, func(ctx context.Context) error { return s.mem.Delete(ctx, k) })
}

// deleteAt deletes k only at version rv (memory.DeleteAt), through the log.
func (s *Store) deleteAt(ctx context.Context, k store.Key, rv string) error {
	v, err := store.ParseVersion("delete", k, rv)
	if err != nil {
		return err
	}
	return s.write(ctx, "delete", k, func(ctx context.Context) error { return s.mem.DeleteAt(ctx, k, v) })
}

// Close stops the background sync and compacts the log into a snapshot (when there are
// entries past the newest one this store knows of). Writes after Close fail with ErrClosed;
// reads still answer from memory. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	readOnly := s.readOnly != nil
	s.mu.Unlock()
	s.stopLoop()
	<-s.loopDone
	if readOnly {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := s.compact(ctx); err != nil {
		return fmt.Errorf("objectstore: final compaction: %w (the log holds every change; the next start replays it)", err)
	}
	return nil
}

// write runs fn, a memory write whose commit hook creates the log entry, until it doesn't lose
// a race: a lost race syncs and runs fn again against the fresh state, so a Create of a key
// now taken is ErrAlreadyExists, an Update of an object that changed is ErrConflict, and
// anything else goes in at the next free version. A write memory refuses without reaching the
// log (ErrNotFound, ErrAlreadyExists, ErrConflict) syncs once and runs again, unless a sync
// started since the write began: so every write is checked against the log's tail.
//
// fn gets a context carrying the write's collector of deleted Runs: the commit hook adds a
// Run it commits the delete of, and once fn is done the Run's transcripts go.
func (s *Store) write(ctx context.Context, op string, k store.Key, fn func(ctx context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return store.Errorf(op, k, err, "")
	}
	if err := s.writable(op, k); err != nil {
		return err
	}
	asked := s.gen()
	if s.stale() {
		// A writer that hasn't seen the log for MaxLag reloads first (from a listing, and from
		// the newest snapshot if entries it hasn't seen were deleted), so it never re-creates
		// a deleted entry.
		if err := s.syncAfter(ctx, asked); err != nil {
			return store.Errorf(op, k, err, "reload before writing")
		}
	}
	runs := &deletedRuns{}
	defer s.dropRunTranscripts(ctx, runs)
	wctx := context.WithValue(ctx, deletedRunsKey{}, runs)
	checked := false
	for range MaxLostRaces {
		err := fn(wctx)
		switch {
		case errors.Is(err, errBehind):
			if serr := s.syncAfter(ctx, s.gen()); serr != nil {
				return store.Errorf(op, k, serr, "sync after a lost race")
			}
		case !checked && (errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrConflict)):
			checked = true
			if s.syncedSince(asked) {
				return err
			}
			if serr := s.syncAfter(ctx, asked); serr != nil {
				return store.Errorf(op, k, serr, "sync to check %v", err)
			}
		default:
			return err
		}
	}
	return store.Errorf(op, k, ErrContended, "%d lost races in a row", MaxLostRaces)
}

// syncedSince reports whether a sync numbered above asked has succeeded.
func (s *Store) syncedSince(asked uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.okGen > asked
}

// writable fails a write when the store is closed or has stopped writing.
func (s *Store) writable(op string, k store.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.Errorf(op, k, ErrClosed, "")
	}
	if s.readOnly != nil {
		return store.Errorf(op, k, s.readOnly, "")
	}
	return nil
}

// stale reports whether the last successful sync started more than MaxLag ago.
func (s *Store) stale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.synced || s.clk.Now().Sub(s.lastOK) > s.cfg.maxLag
}

// deletedRunsKey is the context key of a write's deletedRuns.
type deletedRunsKey struct{}

// deletedRuns collects the Runs one write deleted (a Sweep can delete several).
type deletedRuns struct {
	mu   sync.Mutex
	runs []string
}

// noteDeletedRun adds run to the collector in ctx, if there is one.
func noteDeletedRun(ctx context.Context, run string) {
	if dr, ok := ctx.Value(deletedRunsKey{}).(*deletedRuns); ok {
		dr.mu.Lock()
		dr.runs = append(dr.runs, run)
		dr.mu.Unlock()
	}
}

// dropRunTranscripts drops the transcripts of the Runs the write deleted, after their deletes
// committed.
func (s *Store) dropRunTranscripts(ctx context.Context, dr *deletedRuns) {
	dr.mu.Lock()
	runs := dr.runs
	dr.mu.Unlock()
	if len(runs) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	for _, run := range runs {
		for _, blob := range []string{run, store.EnvironmentLogBlob(run)} {
			// The Run's delete has committed: what is left behind is swept by retention (a
			// record) or the orphan scan (a blob). Each blob gets its own budget.
			bctx, cancel := context.WithTimeout(detached, 2*s.cfg.requestTimeout)
			_, err := s.dropTranscript(bctx, blob)
			cancel()
			if err != nil {
				s.log.Warn("store: drop a deleted Run's transcript", "run", run, "err", err)
			}
		}
	}
}

// keyOf is store.KeyOf for objects CheckObject accepts, and the zero key for the rest (the
// memory store reports those).
func keyOf(obj store.Object) store.Key {
	k, err := store.CheckObject("", obj)
	if err != nil {
		return store.Key{}
	}
	return k
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}

// requestCtx bounds one bucket request.
func (s *Store) requestCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.requestTimeout)
}

// wakeLocks wakes the Locks waiting for a lock record to change.
func (s *Store) wakeLocks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.lockWake)
	s.lockWake = make(chan struct{})
}

func (s *Store) lockWakeCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockWake
}

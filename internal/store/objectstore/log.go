package objectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// commit is the memory store's commit hook: it creates log/<v> for the change at version v,
// under memory's write lock, so memory applies the change (and tells the watchers) only once
// it is in the log. A version another dispatcher took first is errBehind: memory is left as it
// was, and write syncs and runs the operation again.
func (s *Store) commit(ctx context.Context, c memory.Change) error {
	s.mu.Lock()
	closed, readOnly := s.closed, s.readOnly
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if readOnly != nil {
		return readOnly
	}
	nonce := randomHex(8)
	line := journal.LineOf(c)
	line.W = &journal.Writer{ID: s.id, Nonce: nonce}
	line.Schema = s.cfg.schema
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("log: encode: %w", err)
	}
	if err := s.createEntry(ctx, logName(c.Version), data, nonce); err != nil {
		return err
	}
	s.mu.Lock()
	s.maxSchema = max(s.maxSchema, s.cfg.schema)
	s.mu.Unlock()
	if c.Op == memory.OpDelete && c.Key.Kind == v1alpha1.KindRun {
		noteDeletedRun(ctx, c.Key.Name)
	}
	return nil
}

// createEntry creates the log entry name if it is free. Created: committed. Taken: the entry
// is read and its nonce compared with this write's; its own nonce (an SDK retry whose first
// attempt landed) is committed, another is errBehind. An unknown outcome (a timeout or a 5xx
// after the request was sent) reads the entry the same way, and when it is missing creates it
// again, which is safe because the create is conditional.
func (s *Store) createEntry(ctx context.Context, name string, data []byte, nonce string) error {
	var lastErr error
	for range createAttempts {
		rctx, cancel := s.requestCtx(ctx)
		err := s.b.Create(rctx, name, data)
		cancel()
		if err == nil {
			return nil
		}
		taken := errors.Is(err, ErrExists)
		// Read back with a context of its own: a write whose caller gave up still learns
		// whether it landed.
		rctx, cancel = s.requestCtx(context.WithoutCancel(ctx))
		own, found, gerr := s.entryNonce(rctx, name, nonce)
		cancel()
		switch {
		case gerr != nil && taken:
			return fmt.Errorf("log: %s is taken and can't be read: %w", name, gerr)
		case gerr != nil:
			return fmt.Errorf("log: create %s: outcome unknown (%w), and reading it back failed: %w", name, err, gerr)
		case found && own:
			s.log.Debug("store: log entry created by an earlier attempt of this write", "entry", name)
			return nil
		case found:
			return errBehind
		case taken:
			// Taken, then gone: only a deletion of a covered entry does that, which a writer
			// within MaxLag never races. Report it rather than create it again.
			return fmt.Errorf("log: %s was taken, then deleted: %w", name, ErrCorrupt)
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("log: create %s: %w", name, lastErr)
}

// entryNonce reads the log entry name: found is false when it doesn't exist, own whether it
// carries nonce.
func (s *Store) entryNonce(ctx context.Context, name, nonce string) (own, found bool, err error) {
	data, err := s.b.Get(ctx, name)
	if errors.Is(err, ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var line journal.Line
	if err := json.Unmarshal(data, &line); err != nil {
		return false, true, fmt.Errorf("%w: %s: %w", ErrCorrupt, name, err)
	}
	return line.W != nil && line.W.ID == s.id && line.W.Nonce == nonce, true, nil
}

// gen returns how many syncs have started: a caller that needs a sync that started after now
// passes it to syncAfter.
func (s *Store) gen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncGen
}

// Sync implements store.Syncer: it brings the store up to the log's tail, reading the entries
// after its version until one is missing (one GET of a missing object when it is current). A
// sync already running when Sync is called doesn't count; one that starts after does, so
// concurrent callers share one. ErrReadOnly when the log holds an entry past ReadableSchema:
// the store can't read on.
func (s *Store) Sync(ctx context.Context) error {
	return s.syncAfter(ctx, s.gen())
}

// syncAfter makes sure a sync numbered above asked has succeeded, running one if none has.
// A store whose last successful sync started more than MaxLag ago reloads from a listing
// instead of probing.
func (s *Store) syncAfter(ctx context.Context, asked uint64) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	s.mu.Lock()
	if s.okGen > asked {
		s.mu.Unlock()
		return nil
	}
	if s.readOnly != nil {
		err := s.readOnly
		s.mu.Unlock()
		return store.Errorf("sync", store.Key{}, err, "")
	}
	s.syncGen++
	gen := s.syncGen
	start := s.clk.Now()
	reload := !s.synced || start.Sub(s.lastOK) > s.cfg.maxLag
	s.mu.Unlock()

	var err error
	if reload {
		err = s.reload(ctx)
	} else {
		err = s.probe(ctx)
	}
	if err != nil && !errors.Is(err, store.ErrReadOnly) {
		return store.Errorf("sync", store.Key{}, err, "")
	}
	s.mu.Lock()
	s.okGen, s.lastOK, s.synced = gen, start, true
	s.mu.Unlock()
	if err != nil {
		return store.Errorf("sync", store.Key{}, err, "")
	}
	return nil
}

// probe reads log entries from the store's version + 1 until one is missing, and applies
// them. The caller holds syncMu.
func (s *Store) probe(ctx context.Context) error {
	for {
		v := s.mem.Version() + 1
		name := logName(v)
		rctx, cancel := s.requestCtx(ctx)
		data, err := s.b.Get(rctx, name)
		cancel()
		if errors.Is(err, ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := s.applyEntry(name, v, data); err != nil {
			return err
		}
	}
}

// applyEntry decodes the log entry at version v and applies it to memory, firing its watch
// events. An entry past ReadableSchema stops the store writing (ErrReadOnly). The caller holds
// syncMu.
func (s *Store) applyEntry(name string, v uint64, data []byte) error {
	line, err := s.decodeEntry(name, v, data)
	if err != nil {
		return err
	}
	typ := s.types[line.Kind]
	if typ == nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, name, journal.UnknownKind(line.Kind))
	}
	if line.Op == memory.OpDelete {
		typ = nil
	}
	if _, err := s.mem.Apply(line.Change(v), typ); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, name, err)
	}
	s.mu.Lock()
	s.maxSchema = max(s.maxSchema, line.Schema)
	s.mu.Unlock()
	if line.Kind == KindStoreLock {
		s.wakeLocks()
	}
	return nil
}

// decodeEntry parses the log entry at version v and checks its schema: one past
// ReadableSchema stops the store writing.
func (s *Store) decodeEntry(name string, v uint64, data []byte) (journal.Line, error) {
	line, lv, err := journal.DecodeLine(data)
	if err != nil {
		return line, fmt.Errorf("%w: %s: %w", ErrCorrupt, name, err)
	}
	if lv != v {
		return line, fmt.Errorf("%w: %s holds version %d", ErrCorrupt, name, lv)
	}
	if line.Schema > s.cfg.readable {
		return line, s.stopWriting(name, line.Schema)
	}
	return line, nil
}

// stopWriting makes the store read-only: the log holds an entry written by a newer schema
// than this release reads, so a write could drop its fields.
func (s *Store) stopWriting(name string, schema int) error {
	err := fmt.Errorf("%w: %s was written with schema %d and this release reads up to %d (a deploy skipped the expand step); writes stop until this instance is replaced by a release that reads it",
		store.ErrReadOnly, name, schema, s.cfg.readable)
	s.mu.Lock()
	first := s.readOnly == nil
	if first {
		s.readOnly = err
	}
	s.mu.Unlock()
	if first {
		s.log.Error("store: stopped writing", "entry", name, "schema", schema, "readableSchema", s.cfg.readable)
	}
	return err
}

// freshen syncs when the store's last sync is older than the sync interval. A failure is
// logged (once while it lasts) and the read is served from memory.
func (s *Store) freshen(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	fresh := s.readOnly != nil || s.clk.Now().Sub(s.lastOK) < s.cfg.syncInterval
	s.mu.Unlock()
	if fresh {
		return
	}
	err := s.syncAfter(ctx, s.gen())
	if ctx.Err() == nil && !errors.Is(err, store.ErrReadOnly) {
		s.noteSync(err, "a sync before a read failed; reading from memory")
	}
}

// noteSync logs a background or read-path sync's outcome without repeating itself: the first
// failure at warn, the next ones at debug, the recovery at info.
func (s *Store) noteSync(err error, what string) {
	s.mu.Lock()
	wasFailing := s.syncFailing
	s.syncFailing = err != nil
	s.mu.Unlock()
	switch {
	case err != nil && !wasFailing:
		s.log.Warn("store: "+what, "err", err, "interval", s.cfg.syncInterval.String())
	case err != nil:
		s.log.Debug("store: sync failed again", "err", err)
	case wasFailing:
		s.log.Info("store: sync recovered")
	}
}

// syncLoop syncs every sync interval until ctx ends, so watches stay live on a long-running
// serve. Correctness never depends on it (a platform may throttle the CPU between requests).
func (s *Store) syncLoop(ctx context.Context) {
	defer close(s.loopDone)
	for {
		t := s.clk.NewTimer(s.cfg.syncInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		s.mu.Lock()
		skip := s.readOnly != nil || s.clk.Now().Sub(s.lastOK) < s.cfg.syncInterval
		s.mu.Unlock()
		if skip {
			continue
		}
		err := s.syncAfter(ctx, s.gen())
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, store.ErrReadOnly) {
			s.noteSync(err, "background sync failed; retrying every sync interval")
		}
	}
}

// loadedEntry is one log entry read during a load or a reload.
type loadedEntry struct {
	v    uint64
	name string
	data []byte
}

// readLog lists the log after version after and reads every entry listed, in parallel. It
// returns them in version order. An entry listed and then gone is errMoved.
func (s *Store) readLog(ctx context.Context, after uint64) ([]loadedEntry, error) {
	var entries []loadedEntry
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	err := s.b.List(rctx, logPrefix, logName(after), func(o ObjectInfo) error {
		v, ok := versionOf(o.Name, logPrefix, logSuffix)
		if !ok {
			s.log.Warn("store: ignoring an unexpected object in log/", "object", o.Name)
			return nil
		}
		entries = append(entries, loadedEntry{v: v, name: o.Name})
		return nil
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("list log/: %w", err)
	}
	errs := make([]error, len(entries))
	sem := make(chan struct{}, loadParallel)
	var wg sync.WaitGroup
	for i := range entries {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			rctx, cancel := s.requestCtx(ctx)
			data, err := s.b.Get(rctx, entries[i].name)
			cancel()
			switch {
			case errors.Is(err, ErrNotExist):
				errs[i] = fmt.Errorf("%s: %w", entries[i].name, errMoved)
			case err != nil:
				errs[i] = fmt.Errorf("read %s: %w", entries[i].name, err)
			default:
				entries[i].data = data
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		if errors.Is(err, errMoved) {
			return nil, errMoved
		}
		return nil, err
	}
	return entries, nil
}

// image is the state read from the bucket: the newest snapshot and the log above it.
type image struct {
	state      journal.State
	version    uint64
	snapV      uint64
	logEntries int
	maxSchema  int
	// readOnly is set when the log holds an entry past ReadableSchema: the image stops before
	// it.
	readOnly error
}

// readImage reads the newest snapshot and the log above it. A snapshot or entry deleted
// while it reads (a compaction and retention meanwhile) is errMoved.
func (s *Store) readImage(ctx context.Context) (*image, error) {
	img := &image{state: journal.State{}}
	snapV, err := s.newestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if snapV > 0 {
		if err := s.readSnapshot(ctx, snapV, img); err != nil {
			return nil, err
		}
	}
	entries, err := s.readLog(ctx, snapV)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		// An empty tail can also mean a compaction and retention deleted the entries after
		// snapV since snapshots/ was listed: settling at snapV would re-create them.
		newest, err := s.newestSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		if newest > snapV {
			return nil, fmt.Errorf("%w: snapshot %d appeared while the log after %d was empty", errMoved, newest, snapV)
		}
	}
	img.version, img.snapV = snapV, snapV
	for _, e := range entries {
		if e.v != img.version+1 {
			// Entries deleted under a newer snapshot since snapshots/ was listed, or a gap.
			return nil, fmt.Errorf("%w: the log goes from version %d to %d", errMoved, img.version, e.v)
		}
		line, err := s.decodeEntry(e.name, e.v, e.data)
		if errors.Is(err, store.ErrReadOnly) {
			img.readOnly = err
			break
		}
		if err != nil {
			return nil, err
		}
		if err := img.state.Apply(s.types, line, e.v); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrCorrupt, e.name, err)
		}
		img.version = e.v
		img.logEntries++
		img.maxSchema = max(img.maxSchema, line.Schema)
	}
	return img, nil
}

// newestSnapshot lists snapshots/ and returns the newest version (0 when there is none).
func (s *Store) newestSnapshot(ctx context.Context) (uint64, error) {
	var newest uint64
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	defer cancel()
	err := s.b.List(rctx, snapshotPrefix, "", func(o ObjectInfo) error {
		if v, ok := versionOf(o.Name, snapshotPrefix, snapshotSuffix); ok {
			newest = max(newest, v)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("list snapshots/: %w", err)
	}
	return newest, nil
}

// readSnapshot reads the snapshot at version v into img. One written by a newer schema than
// this release reads is ErrSchema.
func (s *Store) readSnapshot(ctx context.Context, v uint64, img *image) error {
	name := snapshotName(v)
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	data, err := s.b.Get(rctx, name)
	cancel()
	if errors.Is(err, ErrNotExist) {
		return fmt.Errorf("%s: %w", name, errMoved)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	data, err = gunzip(data)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, name, err)
	}
	hdr, err := journal.ReadHeader(data)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, name, err)
	}
	if hdr.Schema > s.cfg.readable {
		return fmt.Errorf("objectstore: %w: %s was written with schema %d and this release reads up to schema %d: deploy a release that reads schema %d",
			ErrSchema, name, hdr.Schema, s.cfg.readable, hdr.Schema)
	}
	version, schema, err := journal.ParseSnapshot(data, s.types, img.state)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, name, err)
	}
	if version != v {
		return fmt.Errorf("%w: %s holds version %d", ErrCorrupt, name, version)
	}
	img.maxSchema = max(img.maxSchema, schema)
	return nil
}

// load is the cold start: the newest snapshot, then the log above it, restored into memory.
// It begins again when a snapshot or entry is deleted while it reads.
func (s *Store) load(ctx context.Context) error {
	start := s.clk.Now()
	img, err := s.readImageRetrying(ctx)
	if err != nil {
		return fmt.Errorf("objectstore: load: %w", err)
	}
	recs, counts := img.state.Records(s.types)
	if err := s.mem.Restore(recs, img.version); err != nil {
		return fmt.Errorf("objectstore: load: %w: %w", ErrCorrupt, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncGen, s.okGen, s.lastOK, s.synced = 1, 1, start, true
	s.snapV, s.maxSchema, s.readOnly = img.snapV, img.maxSchema, img.readOnly
	s.loaded = LoadInfo{
		Writer: s.id, Version: img.version, Objects: counts, Snapshot: img.snapV, LogEntries: img.logEntries,
		Transcripts: counts[KindTranscriptBlob], ReadOnly: img.readOnly != nil,
	}
	if img.readOnly != nil {
		s.log.Error("store: loaded up to an entry this release can't read; it stopped writing", "err", img.readOnly)
	}
	return nil
}

func (s *Store) readImageRetrying(ctx context.Context) (*image, error) {
	var err error
	for range loadAttempts {
		var img *image
		img, err = s.readImage(ctx)
		if !errors.Is(err, errMoved) {
			return img, err
		}
		s.log.Debug("store: the log moved while it was read; reading it again", "err", err)
	}
	return nil, fmt.Errorf("%w (%d attempts): %w", ErrCorrupt, loadAttempts, err)
}

// reload brings a store that hasn't synced for MaxLag up to date without probing: it lists
// the log after its version, so it sees a gap left by deleted entries, and when there is one
// it starts again from the newest snapshot (memory.Replace, which expires every watcher). The
// caller holds syncMu.
func (s *Store) reload(ctx context.Context) error {
	rv := s.mem.Version()
	entries, err := s.readLog(ctx, rv)
	if err != nil && !errors.Is(err, errMoved) {
		return err
	}
	gap := errors.Is(err, errMoved) || (len(entries) > 0 && entries[0].v != rv+1)
	if !gap && len(entries) == 0 {
		snapV, err := s.newestSnapshot(ctx)
		if err != nil {
			return err
		}
		gap = snapV > rv
	}
	if gap {
		return s.replaceFromSnapshot(ctx, rv)
	}
	for _, e := range entries {
		if e.v != s.mem.Version()+1 {
			if e.v <= s.mem.Version() {
				continue
			}
			return s.replaceFromSnapshot(ctx, rv)
		}
		if err := s.applyEntry(e.name, e.v, e.data); err != nil {
			return err
		}
	}
	return s.probe(ctx) // entries created after the listing
}

// replaceFromSnapshot swaps memory for the newest snapshot and the log above it.
func (s *Store) replaceFromSnapshot(ctx context.Context, rv uint64) error {
	img, err := s.readImageRetrying(ctx)
	if err != nil {
		return err
	}
	if img.version < rv {
		return fmt.Errorf("%w: the bucket's log ends at version %d, before this store's %d", ErrCorrupt, img.version, rv)
	}
	recs, _ := img.state.Records(s.types)
	if err := s.mem.Replace(recs, img.version); err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	s.log.Warn("store: reloaded from the newest snapshot: entries this store hadn't seen were deleted", "from", rv, "to", img.version, "snapshot", img.snapV)
	s.mu.Lock()
	s.snapV = max(s.snapV, img.snapV)
	s.maxSchema = max(s.maxSchema, img.maxSchema)
	s.mu.Unlock()
	s.wakeLocks()
	if img.readOnly != nil {
		s.mu.Lock()
		if s.readOnly == nil {
			s.readOnly = img.readOnly
		}
		s.mu.Unlock()
		return img.readOnly
	}
	return nil
}

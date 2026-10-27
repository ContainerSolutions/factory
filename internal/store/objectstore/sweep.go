package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
)

// errStop ends a listing early.
var errStop = errors.New("stop")

// Sweep implements store.Sweeper. It deletes, through the log:
//   - the objects past retention (store.RetentionOf), as memory's sweep does;
//   - the expired lock records (a crashed holder, or an Unlock that failed);
//   - the transcript blobs whose putAt is past retention, with their records.
//
// Then it compacts when the log is CompactEvery entries past the newest snapshot, and at its
// first run and once a day after it deletes the log entries and snapshots past the segment
// retention and the orphan transcript blobs. The leader runs it (store.RunSweeper).
func (s *Store) Sweep(ctx context.Context) (store.SweepResult, error) {
	res := store.SweepResult{Objects: map[string]int{}}
	if err := ctx.Err(); err != nil {
		return res, store.Errorf("sweep", store.Key{}, err, "")
	}
	err := s.write(ctx, "sweep", store.Key{}, func(ctx context.Context) error {
		r, err := s.mem.Sweep(ctx)
		for k, n := range r.Objects {
			res.Objects[k] += n
		}
		return err
	})
	if err != nil {
		return res, err
	}
	now := s.clk.Now()
	n, err := s.sweepLocks(ctx, now)
	if n > 0 {
		res.Objects[KindStoreLock] += n
	}
	if err != nil {
		return res, store.Errorf("sweep", store.Key{Kind: KindStoreLock}, err, "")
	}
	if res.Transcripts, err = s.sweepTranscripts(ctx, now); err != nil {
		return res, store.Errorf("sweep", store.Key{Kind: KindTranscriptBlob}, err, "")
	}
	if res.Transcripts > 0 {
		s.log.InfoContext(ctx, "store: swept transcripts", "transcripts", res.Transcripts, "retention", s.opts.Retention.String())
	}
	// What follows only saves space: a failure is returned (the next sweep tries again), with
	// the result of what was swept.
	var errs []error
	if err := s.maybeCompact(ctx); err != nil {
		errs = append(errs, fmt.Errorf("compaction: %w", err))
	}
	s.mu.Lock()
	due := now.Sub(s.lastMaint) >= maintenanceEvery
	if due {
		s.lastMaint = now
	}
	s.mu.Unlock()
	if due {
		if err := s.maintain(ctx, now); err != nil {
			errs = append(errs, fmt.Errorf("log retention and orphan scan: %w", err))
			s.mu.Lock()
			s.lastMaint = time.Time{} // try again at the next sweep
			s.mu.Unlock()
		}
	}
	if err := errors.Join(errs...); err != nil {
		return res, store.Errorf("sweep", store.Key{}, err, "")
	}
	return res, nil
}

// maybeCompact compacts when the log is CompactEvery entries past the newest snapshot this
// store knows of.
func (s *Store) maybeCompact(ctx context.Context) error {
	s.mu.Lock()
	snapV := s.snapV
	s.mu.Unlock()
	if s.mem.Version() < snapV+s.cfg.compactEvery {
		return nil
	}
	return s.compact(ctx)
}

// compact writes snapshots/<V>.json.gz from memory at version V, create-only: a snapshot is
// never overwritten, and another dispatcher's compaction at the same version is as good.
func (s *Store) compact(ctx context.Context) error {
	recs, v := s.mem.Snapshot()
	s.mu.Lock()
	snapV, schema := s.snapV, max(s.cfg.schema, s.maxSchema)
	s.mu.Unlock()
	if v == 0 || v <= snapV {
		return nil
	}
	var buf bytes.Buffer
	if err := journal.WriteSnapshot(&buf, recs, v, schema); err != nil {
		return err
	}
	data, err := gzipBytes(buf.Bytes())
	if err != nil {
		return fmt.Errorf("compress the snapshot: %w", err)
	}
	name := snapshotName(v)
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	err = s.b.Create(rctx, name, data)
	cancel()
	if err != nil && !errors.Is(err, ErrExists) {
		return fmt.Errorf("create %s: %w", name, err)
	}
	s.mu.Lock()
	s.snapV = max(s.snapV, v)
	s.mu.Unlock()
	s.log.Debug("store: compacted", "snapshot", name, "objects", len(recs))
	return nil
}

// maintain deletes what no load needs any more, without ever letting a lagging writer
// re-create a deleted entry (with MaxLag, which makes a writer reload when it hasn't seen the
// log for an hour):
//   - a log entry only when the newest snapshot covers it and it is older than the segment
//     retention;
//   - a snapshot only when it isn't one of the newest two and is older than the segment
//     retention;
//   - a transcript blob that has had no record for a day.
//
// Ages are the bucket's object times against the store's clock. There are no bucket lifecycle
// rules: an age-based rule could delete entries no snapshot covers yet.
func (s *Store) maintain(ctx context.Context, now time.Time) error {
	var snaps []ObjectInfo
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	err := s.b.List(rctx, snapshotPrefix, "", func(o ObjectInfo) error {
		if _, ok := versionOf(o.Name, snapshotPrefix, snapshotSuffix); ok {
			snaps = append(snaps, o)
		}
		return nil
	})
	cancel()
	if err != nil {
		return fmt.Errorf("list snapshots/: %w", err)
	}
	var errs []error
	snapshots, entries := 0, 0
	if len(snaps) > 0 {
		covered, _ := versionOf(snaps[len(snaps)-1].Name, snapshotPrefix, snapshotSuffix)
		s.mu.Lock()
		s.snapV = max(s.snapV, covered)
		s.mu.Unlock()
		for _, o := range snaps[:max(len(snaps)-2, 0)] {
			if now.Sub(o.Updated) < s.cfg.segmentRetention {
				continue
			}
			if err := s.deleteObject(ctx, o.Name); err != nil {
				errs = append(errs, err)
				continue
			}
			snapshots++
		}
		var old []string
		rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
		err := s.b.List(rctx, logPrefix, "", func(o ObjectInfo) error {
			v, ok := versionOf(o.Name, logPrefix, logSuffix)
			if !ok {
				return nil
			}
			if v > covered || now.Sub(o.Updated) < s.cfg.segmentRetention {
				return errStop // in version order: the rest is newer
			}
			old = append(old, o.Name)
			return nil
		})
		cancel()
		if err != nil && !errors.Is(err, errStop) {
			errs = append(errs, fmt.Errorf("list log/: %w", err))
		}
		for _, name := range old {
			if err := s.deleteObject(ctx, name); err != nil {
				errs = append(errs, err)
				break // keep the log contiguous from where it starts
			}
			entries++
		}
	}
	orphans, err := s.scanOrphans(ctx, now)
	if err != nil {
		errs = append(errs, err)
	}
	if snapshots+entries+orphans > 0 {
		s.log.InfoContext(ctx, "store: deleted old log entries, snapshots and orphan transcripts",
			"logEntries", entries, "snapshots", snapshots, "orphanTranscripts", orphans,
			"segmentRetention", s.cfg.segmentRetention.String())
	}
	return errors.Join(errs...)
}

// deleteObject deletes one object; one already gone is fine.
func (s *Store) deleteObject(ctx context.Context, name string) error {
	rctx, cancel := s.requestCtx(ctx)
	defer cancel()
	if err := s.b.Delete(rctx, name); err != nil && !errors.Is(err, ErrNotExist) {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

// Compact writes a snapshot of the store now, when the log has entries past the newest one
// this store knows of (the sweep does it every CompactEvery entries, and Close at the end).
func (s *Store) Compact(ctx context.Context) error {
	if err := s.compact(ctx); err != nil {
		return fmt.Errorf("objectstore: compact: %w", err)
	}
	return nil
}

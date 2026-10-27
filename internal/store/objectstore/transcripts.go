package objectstore

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
)

// transcriptPutAttempts bounds the record writes of one PutTranscript that lose a race to
// another dispatcher's put of the same blob.
const transcriptPutAttempts = 5

// blobName returns run's blob, or "" when run is too long for a name.
func blobName(run string) string {
	n := journal.TranscriptName(run)
	if n == "" {
		return ""
	}
	return transcriptPrefix + n
}

// PutTranscript implements store.TranscriptStore. The blob is put first (a re-put replaces
// it), then its TranscriptBlob record {size, putAt} is written through the log, so a crash in
// between leaves an orphan blob, which the daily orphan scan deletes. The caps are the memory
// store's: over the per-transcript cap is ErrTooLarge, and the blobs with the oldest putAt
// (then the lowest resourceVersion) are evicted, logged, to stay under the total.
func (s *Store) PutTranscript(ctx context.Context, run string, data []byte) error {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	if err := ctx.Err(); err != nil {
		return store.Errorf("transcript", k, err, "")
	}
	if run == "" {
		return store.Errorf("transcript", k, store.ErrInvalid, "missing run")
	}
	name := blobName(run)
	if name == "" {
		return store.Errorf("transcript", k, store.ErrInvalid, "run name too long for a blob name")
	}
	size := int64(len(data))
	if limit := min(s.opts.TranscriptMaxBytes, s.opts.TranscriptTotalBytes); size > limit {
		return store.Errorf("transcript", k, store.ErrTooLarge, "%d bytes, cap %d", size, limit)
	}
	if err := s.writable("transcript", k); err != nil {
		return err
	}
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if err := s.evictFor(ctx, run, size); err != nil {
		return store.Errorf("transcript", k, err, "evict")
	}
	rctx, cancel := s.requestCtx(ctx)
	err := s.b.Put(rctx, name, data)
	cancel()
	if err != nil {
		return store.Errorf("transcript", k, err, "put %s", name)
	}
	rk := store.Key{Kind: KindTranscriptBlob, Name: run}
	for range transcriptPutAttempts {
		now := s.clk.Now()
		var rec TranscriptBlob
		switch err := s.Get(ctx, rk, &rec); {
		case errors.Is(err, store.ErrNotFound):
			rec = TranscriptBlob{
				TypeMeta: v1alpha1.TypeMeta{Kind: KindTranscriptBlob},
				Metadata: v1alpha1.ObjectMeta{Name: run}, Size: size, PutAt: now,
			}
			err = s.Create(ctx, &rec)
			if errors.Is(err, store.ErrAlreadyExists) {
				continue
			}
			if err != nil {
				return store.Errorf("transcript", k, err, "record")
			}
			return nil
		case err != nil:
			return store.Errorf("transcript", k, err, "record")
		default:
			rec.Size, rec.PutAt = size, now
			err = s.Update(ctx, &rec)
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return store.Errorf("transcript", k, err, "record")
			}
			return nil
		}
	}
	return store.Errorf("transcript", k, ErrContended, "the record changed %d times in a row", transcriptPutAttempts)
}

// evictFor evicts the oldest blobs until size more bytes for run (replacing its own) fit the
// total cap. The caller holds tmu.
func (s *Store) evictFor(ctx context.Context, run string, size int64) error {
	s.freshen(ctx)
	var recs []*TranscriptBlob
	if _, err := s.mem.List(ctx, KindTranscriptBlob, store.ListOptions{}, &recs); err != nil {
		return err
	}
	var total int64
	others := recs[:0]
	for _, r := range recs {
		if r.Metadata.Name == run {
			continue
		}
		total += r.Size
		others = append(others, r)
	}
	slices.SortFunc(others, func(a, b *TranscriptBlob) int {
		return cmp.Or(a.PutAt.Compare(b.PutAt), cmp.Compare(rvOf(a), rvOf(b)))
	})
	for _, r := range others {
		if total+size <= s.opts.TranscriptTotalBytes {
			break
		}
		if err := s.dropTranscript(ctx, r.Metadata.Name); err != nil {
			return err
		}
		total -= r.Size
		s.log.Info("store: transcript evicted", "run", r.Metadata.Name, "bytes", r.Size,
			"reason", "total cap", "totalCap", s.opts.TranscriptTotalBytes)
	}
	return nil
}

func rvOf(r *TranscriptBlob) uint64 {
	v, _ := store.ParseVersion("", store.Key{}, r.Metadata.ResourceVersion)
	return v
}

// GetTranscript implements store.TranscriptStore: the record (synced once when it isn't
// found), then the blob.
func (s *Store) GetTranscript(ctx context.Context, run string) ([]byte, error) {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	if err := ctx.Err(); err != nil {
		return nil, store.Errorf("transcript", k, err, "")
	}
	name := blobName(run)
	if run == "" || name == "" {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
	}
	var rec TranscriptBlob
	if err := s.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: run}, &rec); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
		}
		return nil, store.Errorf("transcript", k, err, "")
	}
	rctx, cancel := s.requestCtx(ctx)
	data, err := s.b.Get(rctx, name)
	cancel()
	if errors.Is(err, ErrNotExist) {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "") // deleted meanwhile
	}
	if err != nil {
		return nil, store.Errorf("transcript", k, err, "read %s", name)
	}
	return data, nil
}

// dropTranscript deletes run's record (at the version read), then its blob. A record changed
// or gone meanwhile is fine. When the record's delete fails the blob is kept (still indexed);
// a blob whose delete fails is left to the orphan scan.
func (s *Store) dropTranscript(ctx context.Context, run string) error {
	name := blobName(run)
	if name == "" {
		return nil
	}
	var rec TranscriptBlob
	rk := store.Key{Kind: KindTranscriptBlob, Name: run}
	switch err := s.mem.Get(ctx, rk, &rec); {
	case err == nil:
		if err := s.deleteAt(ctx, rk, rec.Metadata.ResourceVersion); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("delete the transcript record of %s: %w", run, err)
		}
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("read the transcript record of %s: %w", run, err)
	}
	rctx, cancel := s.requestCtx(ctx)
	defer cancel()
	if err := s.b.Delete(rctx, name); err != nil && !errors.Is(err, ErrNotExist) {
		return fmt.Errorf("delete %s (the orphan scan retries): %w", name, err)
	}
	return nil
}

// sweepTranscripts drops the blobs whose putAt is past retention, and returns how many.
func (s *Store) sweepTranscripts(ctx context.Context, now time.Time) (int, error) {
	var recs []*TranscriptBlob
	if _, err := s.mem.List(ctx, KindTranscriptBlob, store.ListOptions{}, &recs); err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range recs {
		if !store.Expired(store.SinceCreated, r.PutAt, r.PutAt, now, s.opts.Retention) {
			continue
		}
		if err := s.dropTranscript(ctx, r.Metadata.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// scanOrphans deletes the blobs under transcripts/ that have had no record for orphanAge (a
// crash between a blob's put and its record), and returns how many.
func (s *Store) scanOrphans(ctx context.Context, now time.Time) (int, error) {
	var orphans []string
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	err := s.b.List(rctx, transcriptPrefix, "", func(o ObjectInfo) error {
		run, ok := journal.RunOfTranscript(o.Name[len(transcriptPrefix):])
		if !ok {
			s.log.Warn("store: ignoring an unexpected object in transcripts/", "object", o.Name)
			return nil
		}
		if now.Sub(o.Updated) < orphanAge {
			return nil
		}
		var rec TranscriptBlob
		if err := s.mem.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: run}, &rec); errors.Is(err, store.ErrNotFound) {
			orphans = append(orphans, o.Name)
		}
		return nil
	})
	cancel()
	if err != nil {
		return 0, fmt.Errorf("list transcripts/: %w", err)
	}
	n := 0
	for _, name := range orphans {
		rctx, cancel := s.requestCtx(ctx)
		err := s.b.Delete(rctx, name)
		cancel()
		if err != nil && !errors.Is(err, ErrNotExist) {
			return n, fmt.Errorf("delete %s: %w", name, err)
		}
		n++
	}
	return n, nil
}

// gzipBytes compresses data.
func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzip decompresses data.
func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close() //nolint:errcheck // read-only
	return io.ReadAll(zr)
}

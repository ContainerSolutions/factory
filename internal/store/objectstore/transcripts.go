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
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
)

// transcriptPutAttempts bounds the record writes of one PutTranscript that lose a race to
// another dispatcher's put of the same transcript.
const transcriptPutAttempts = 5

// blobIDLen is the length of a blob id: 8 random bytes in hex.
const blobIDLen = 16

// blobName returns the blob of run's put id: transcripts/<run>.<id>.jsonl.gz, with P2's
// escaping for an unsafe run name. "" when run is too long for a name.
func blobName(run, id string) string {
	n := journal.TranscriptName(run)
	if n == "" {
		return ""
	}
	n = strings.TrimSuffix(n, journal.TranscriptSuffix) + "." + id + journal.TranscriptSuffix
	if len(n) > journal.MaxTranscriptName {
		return ""
	}
	return transcriptPrefix + n
}

// blobOf is blobName's inverse; ok is false for a name it never returns.
func blobOf(name string) (run, id string, ok bool) {
	base, ok := strings.CutPrefix(name, transcriptPrefix)
	if !ok {
		return "", "", false
	}
	base, ok = strings.CutSuffix(base, journal.TranscriptSuffix)
	i := strings.LastIndexByte(base, '.')
	if !ok || i < 0 {
		return "", "", false
	}
	id = base[i+1:]
	if len(id) != blobIDLen || strings.Trim(id, "0123456789abcdef") != "" {
		return "", "", false
	}
	run, ok = journal.RunOfTranscript(base[:i] + journal.TranscriptSuffix)
	return run, id, ok
}

// PutTranscript implements store.TranscriptStore. Each put creates a new blob,
// transcripts/<run>.<id>.jsonl.gz with a random id, create-if-absent; then the run's
// TranscriptBlob record {id, size, putAt} is written through the log, and the blob the record
// named before is deleted. So a drop (which deletes only the blob its deleted record named)
// and a re-put on another dispatcher can't remove each other's blob, in either order. A crash
// before the record leaves an orphan blob, which the daily orphan scan deletes. The caps are
// the memory store's: over the per-transcript cap is ErrTooLarge, and the blobs with the
// oldest putAt (then the lowest resourceVersion) are evicted, logged, to stay under the total.
func (s *Store) PutTranscript(ctx context.Context, run string, data []byte) error {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	if err := ctx.Err(); err != nil {
		return store.Errorf("transcript", k, err, "")
	}
	if run == "" {
		return store.Errorf("transcript", k, store.ErrInvalid, "missing run")
	}
	if blobName(run, strings.Repeat("0", blobIDLen)) == "" {
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
	id, err := s.createBlob(ctx, run, data)
	if err != nil {
		return store.Errorf("transcript", k, err, "")
	}
	rk := store.Key{Kind: KindTranscriptBlob, Name: run}
	for range transcriptPutAttempts {
		now := s.clk.Now()
		var rec TranscriptBlob
		switch err := s.Get(ctx, rk, &rec); {
		case errors.Is(err, store.ErrNotFound):
			rec = TranscriptBlob{
				TypeMeta: v1alpha1.TypeMeta{Kind: KindTranscriptBlob},
				Metadata: v1alpha1.ObjectMeta{Name: run}, ID: id, Size: size, PutAt: now,
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
			replaced := rec.ID
			rec.ID, rec.Size, rec.PutAt = id, size, now
			err = s.Update(ctx, &rec)
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return store.Errorf("transcript", k, err, "record")
			}
			if replaced != "" && replaced != id {
				if err := s.deleteObject(ctx, blobName(run, replaced)); err != nil {
					s.log.Warn("store: delete a replaced transcript blob (the orphan scan retries)", "run", run, "err", err)
				}
			}
			return nil
		}
	}
	// The new blob has no record: the orphan scan deletes it.
	return store.Errorf("transcript", k, ErrContended, "the record changed %d times in a row", transcriptPutAttempts)
}

// createBlob creates a new blob of run's data, create-if-absent under a new random id, and
// returns the id.
func (s *Store) createBlob(ctx context.Context, run string, data []byte) (string, error) {
	for range 3 {
		id := randomHex(blobIDLen / 2)
		name := blobName(run, id)
		rctx, cancel := s.requestCtx(ctx)
		err := s.b.Create(rctx, name, data)
		cancel()
		switch {
		case err == nil:
			return id, nil
		case !errors.Is(err, ErrExists):
			return "", fmt.Errorf("create %s: %w", name, err)
		}
	}
	return "", errors.New("three random blob ids in a row were taken")
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
		dropped, err := s.dropTranscript(ctx, r.Metadata.Name)
		if !dropped {
			if err != nil {
				return err // the record is still there, and counts
			}
			continue // re-put or dropped elsewhere meanwhile
		}
		if err != nil {
			// The record is gone, so the bytes no longer count; the blob is the orphan scan's.
			s.log.Warn("store: an evicted transcript's blob stays until the orphan scan", "run", r.Metadata.Name, "err", err)
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
// found), then the blob it names. A blob a re-put replaced between the two reads is followed
// once to the new record.
func (s *Store) GetTranscript(ctx context.Context, run string) ([]byte, error) {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	if err := ctx.Err(); err != nil {
		return nil, store.Errorf("transcript", k, err, "")
	}
	if run == "" {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
	}
	rk := store.Key{Kind: KindTranscriptBlob, Name: run}
	for attempt := range 2 {
		if attempt > 0 {
			// A read-only store answers from what it has (store.md: reads go on): it synced
			// up to the entry it can't read, and reads the record again from there.
			if err := s.Sync(ctx); err != nil && !errors.Is(err, store.ErrReadOnly) {
				return nil, store.Errorf("transcript", k, err, "")
			}
		}
		var rec TranscriptBlob
		if err := s.Get(ctx, rk, &rec); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
			}
			return nil, store.Errorf("transcript", k, err, "")
		}
		name := blobName(run, rec.ID)
		rctx, cancel := s.requestCtx(ctx)
		data, err := s.b.Get(rctx, name)
		cancel()
		switch {
		case err == nil:
			return data, nil
		case !errors.Is(err, ErrNotExist):
			return nil, store.Errorf("transcript", k, err, "read %s", name)
		}
	}
	return nil, store.Errorf("transcript", k, store.ErrNotFound, "") // dropped meanwhile
}

// dropTranscript deletes run's record (at the version read), then the blob that record named,
// and reports whether it did. A record that changed meanwhile (ErrConflict: a re-put) or is
// gone (another drop) is left to whoever changed it: the re-put deletes the blob it replaced.
// When the record's delete fails the blob is kept (still indexed); a blob whose delete fails
// is left to the orphan scan.
func (s *Store) dropTranscript(ctx context.Context, run string) (dropped bool, err error) {
	var rec TranscriptBlob
	rk := store.Key{Kind: KindTranscriptBlob, Name: run}
	switch err := s.mem.Get(ctx, rk, &rec); {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the transcript record of %s: %w", run, err)
	}
	switch err := s.deleteAt(ctx, rk, rec.Metadata.ResourceVersion); {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("delete the transcript record of %s: %w", run, err)
	}
	if name := blobName(run, rec.ID); name != "" {
		if err := s.deleteObject(ctx, name); err != nil {
			return true, fmt.Errorf("%w (the orphan scan retries)", err)
		}
	}
	return true, nil
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
		dropped, err := s.dropTranscript(ctx, r.Metadata.Name)
		if err != nil {
			errs = append(errs, err)
		}
		if dropped {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// scanOrphans deletes the blobs under transcripts/ whose id no record names and that are over
// orphanAge old (a crash between a blob's create and its record, or a replaced blob whose
// delete failed), and returns how many.
func (s *Store) scanOrphans(ctx context.Context, now time.Time) (int, error) {
	var orphans []string
	rctx, cancel := context.WithTimeout(ctx, 4*s.cfg.requestTimeout)
	err := s.b.List(rctx, transcriptPrefix, "", func(o ObjectInfo) error {
		run, id, ok := blobOf(o.Name)
		if !ok {
			s.log.Warn("store: ignoring an unexpected object in transcripts/", "object", o.Name)
			return nil
		}
		if now.Sub(o.Updated) < orphanAge {
			return nil
		}
		var rec TranscriptBlob
		err := s.mem.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: run}, &rec)
		if errors.Is(err, store.ErrNotFound) || (err == nil && rec.ID != id) {
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
		if err := s.deleteObject(ctx, name); err != nil {
			return n, err
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

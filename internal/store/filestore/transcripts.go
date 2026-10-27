package filestore

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

const (
	transcriptSuffix = ".jsonl.gz"
	// encodedPrefix marks a file name holding a run name that isn't safe as a file name, in
	// base64url. It is outside the safe set, so the two never collide.
	encodedPrefix = "~"
	// maxFileName keeps file names under the 255-byte limit of the file systems we run on.
	maxFileName = 240
)

// tblob is one transcript file's index entry; the bytes live in the file.
type tblob struct {
	size    int64
	created time.Time // also the file's mtime, so it survives a restart
	seq     uint64    // put order: the lowest is the oldest
}

// transcriptFile returns run's file name, or "" when run is too long for one.
func transcriptFile(run string) string {
	name := run
	if !safeName(run) {
		name = encodedPrefix + base64.RawURLEncoding.EncodeToString([]byte(run))
	}
	name += transcriptSuffix
	if len(name) > maxFileName {
		return ""
	}
	return name
}

// runOfFile is transcriptFile's inverse; ok is false for a name it never returns.
func runOfFile(name string) (run string, ok bool) {
	base, ok := strings.CutSuffix(name, transcriptSuffix)
	if !ok || base == "" {
		return "", false
	}
	if enc, isEnc := strings.CutPrefix(base, encodedPrefix); isEnc {
		b, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil || len(b) == 0 || safeName(string(b)) {
			return "", false
		}
		return string(b), true
	}
	return base, safeName(base)
}

// safeName reports whether a run name is used as its file name as is: letters, digits, '.',
// '_' and '-', not starting with '.' or '-'.
func safeName(s string) bool {
	if s == "" || s[0] == '.' || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Store) transcriptPath(file string) string {
	return filepath.Join(s.dir, transcriptDir, file)
}

// PutTranscript implements store.TranscriptStore: the blob is written to a tmp file, fsync'd
// and renamed into transcripts/. The caps are the memory store's: over the per-transcript cap
// is ErrTooLarge, and the oldest transcripts are evicted (logged) to stay under the total.
func (s *Store) PutTranscript(ctx context.Context, run string, data []byte) error {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	if err := ctx.Err(); err != nil {
		return store.Errorf("transcript", k, err, "")
	}
	if run == "" {
		return store.Errorf("transcript", k, store.ErrInvalid, "missing run")
	}
	file := transcriptFile(run)
	if file == "" {
		return store.Errorf("transcript", k, store.ErrInvalid, "run name too long for a file name")
	}
	size := int64(len(data))
	if limit := min(s.opts.TranscriptMaxBytes, s.opts.TranscriptTotalBytes); size > limit {
		return store.Errorf("transcript", k, store.ErrTooLarge, "%d bytes, cap %d", size, limit)
	}
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.isClosed.Load() {
		return store.Errorf("transcript", k, ErrClosed, "")
	}
	created := s.clk.Now()
	path := s.transcriptPath(file)
	if err := writeTranscript(path, data, created); err != nil {
		return store.Errorf("transcript", k, err, "")
	}

	if old := s.blobs[run]; old != nil {
		s.blobTotal -= old.size
		delete(s.blobs, run)
	}
	for s.blobTotal+size > s.opts.TranscriptTotalBytes && len(s.blobs) > 0 {
		oldest, ob := "", (*tblob)(nil)
		for name, b := range s.blobs {
			if ob == nil || b.seq < ob.seq {
				oldest, ob = name, b
			}
		}
		s.dropTranscriptLocked(oldest)
		s.log.Info("store: transcript evicted", "run", oldest, "bytes", ob.size,
			"reason", "total cap", "totalCap", s.opts.TranscriptTotalBytes)
	}
	s.blobSeq++
	s.blobs[run] = &tblob{size: size, created: created, seq: s.blobSeq}
	s.blobTotal += size
	return nil
}

// writeTranscript writes data to path through a tmp file, with created as its mtime.
func writeTranscript(path string, data []byte, created time.Time) error {
	tmp := path + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create transcript: %w", err)
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(fmt.Errorf("write transcript: %w", err))
	}
	// The mtime is the creation time on the store's clock (retention and eviction order), set
	// before the fsync so it is durable with the bytes.
	if err := os.Chtimes(tmp, created, created); err != nil {
		return fail(fmt.Errorf("set transcript time: %w", err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("fsync transcript: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close transcript: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename transcript: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// GetTranscript implements store.TranscriptStore.
func (s *Store) GetTranscript(ctx context.Context, run string) ([]byte, error) {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	if err := ctx.Err(); err != nil {
		return nil, store.Errorf("transcript", k, err, "")
	}
	s.tmu.Lock()
	b := s.blobs[run]
	s.tmu.Unlock()
	if b == nil {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
	}
	data, err := os.ReadFile(s.transcriptPath(transcriptFile(run)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "") // deleted meanwhile
	}
	if err != nil {
		return nil, store.Errorf("transcript", k, err, "read")
	}
	return data, nil
}

// removeTranscript drops run's transcript, if any (its Run was deleted).
func (s *Store) removeTranscript(run string) {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.dropTranscriptLocked(run)
}

// dropTranscriptLocked removes run's file and index entry. A file that can't be removed is
// logged: it is out of the index, so it is no longer served, and the next load picks it up
// again. The caller holds tmu.
func (s *Store) dropTranscriptLocked(run string) {
	b := s.blobs[run]
	if b == nil {
		return
	}
	s.blobTotal -= b.size
	delete(s.blobs, run)
	if err := os.Remove(s.transcriptPath(transcriptFile(run))); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Error("store: remove transcript", "run", run, "err", err)
	}
}

// sweepTranscripts drops the transcripts past retention and returns how many.
func (s *Store) sweepTranscripts(now time.Time) int {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.isClosed.Load() {
		return 0 // the dir is no longer ours
	}
	n := 0
	for run, b := range s.blobs {
		if store.Expired(store.SinceCreated, b.created, b.created, now, s.opts.Retention) {
			s.dropTranscriptLocked(run)
			n++
		}
	}
	return n
}

// loadTranscripts indexes transcripts/: creation time from the mtime, put order from the
// creation time. Leftover tmp files are removed; a total over the cap evicts the oldest.
func (s *Store) loadTranscripts() error {
	dir := filepath.Join(s.dir, transcriptDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("filestore: read transcripts: %w", err)
	}
	type found struct {
		run string
		b   *tblob
	}
	var all []found
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, tmpSuffix) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("filestore: remove %s: %w", name, err)
			}
			continue
		}
		run, ok := runOfFile(name)
		if !ok || !e.Type().IsRegular() {
			s.log.Warn("store: ignoring an unexpected file in transcripts/", "file", name)
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return fmt.Errorf("filestore: stat %s: %w", name, err)
		}
		all = append(all, found{run: run, b: &tblob{size: fi.Size(), created: fi.ModTime()}})
	}
	slices.SortFunc(all, func(a, b found) int {
		return cmp.Or(a.b.created.Compare(b.b.created), cmp.Compare(a.run, b.run))
	})
	for _, f := range all {
		s.blobSeq++
		f.b.seq = s.blobSeq
		s.blobs[f.run] = f.b
		s.blobTotal += f.b.size
	}
	for _, f := range all {
		if s.blobTotal <= s.opts.TranscriptTotalBytes {
			break
		}
		s.dropTranscriptLocked(f.run)
		s.log.Info("store: transcript evicted", "run", f.run, "bytes", f.b.size,
			"reason", "total cap", "totalCap", s.opts.TranscriptTotalBytes)
	}
	return nil
}

package objectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// LogEntry is one log entry a View read above its snapshot: one committed change.
type LogEntry struct {
	Version uint64
	At      time.Time
	Op      memory.Op
	Kind    string
	Name    string
	// Obj is the object as stored by the change (nil for delete).
	Obj json.RawMessage
}

// View is the state in a bucket, read once: the newest snapshot and the log above it. It
// writes nothing, takes no lock and doesn't sync, so it works while dispatchers run on the
// bucket (factory state --state-url). An entry past ReadableSchema ends the view before it,
// with a warning.
type View struct {
	b        Bucket
	mem      *memory.Store
	loaded   LoadInfo
	entries  []LogEntry
	warnings []string
	timeout  time.Duration
}

// Read reads the state in b, read-only. options are Open's (WithKind for the stored kinds
// outside v1alpha1; the timings that apply to reads).
func Read(ctx context.Context, b Bucket, opts store.Options, options ...Option) (*View, error) {
	cfg := config{
		kinds: ownKinds(), requestTimeout: DefaultRequestTimeout,
		schema: store.SchemaVersion, readable: store.ReadableSchema,
	}
	for _, o := range options {
		o(&cfg)
	}
	maps.Copy(cfg.kinds, ownKinds())
	types, err := journal.Registry(cfg.kinds)
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	opts = opts.WithDefaults()
	s := &Store{b: b, opts: opts, clk: opts.Clock, log: opts.Logger, cfg: cfg, types: types, collectLog: true}
	img, err := s.readImageRetrying(ctx)
	if err != nil {
		return nil, fmt.Errorf("objectstore: read: %w", err)
	}
	recs, counts := img.state.Records(types)
	mem := memory.New(opts)
	if err := mem.Restore(recs, img.version); err != nil {
		return nil, fmt.Errorf("objectstore: read: %w: %w", ErrCorrupt, err)
	}
	v := &View{
		b: b, mem: mem, entries: img.entries, timeout: cfg.requestTimeout,
		loaded: LoadInfo{
			Version: img.version, Objects: counts, Snapshot: img.snapV, LogEntries: img.logEntries,
			Transcripts: counts[KindTranscriptBlob], ReadOnly: img.readOnly != nil,
		},
	}
	if img.readOnly != nil {
		v.warnings = append(v.warnings, fmt.Sprintf("the view ends at version %d: %v", img.version, img.readOnly))
	}
	return v, nil
}

// Loaded returns what Read loaded.
func (v *View) Loaded() LoadInfo {
	li := v.loaded
	li.Objects = maps.Clone(li.Objects)
	return li
}

// Warnings returns what Read noticed and still loaded around.
func (v *View) Warnings() []string { return slices.Clone(v.warnings) }

// Log returns the log entries above the snapshot, in version order.
func (v *View) Log() []LogEntry { return slices.Clone(v.entries) }

// Get reads one object, as store.Store's Get.
func (v *View) Get(ctx context.Context, k store.Key, out store.Object) error {
	return v.mem.Get(ctx, k, out)
}

// List lists objects, as store.Store's List.
func (v *View) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	return v.mem.List(ctx, kind, opts, out)
}

// Transcript returns run's transcript blob as stored (gzip'd); store.ErrNotFound when it has
// none.
func (v *View) Transcript(ctx context.Context, run string) ([]byte, error) {
	k := store.Key{Kind: v1alpha1.KindRun, Name: run}
	var rec TranscriptBlob
	if err := v.mem.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: run}, &rec); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
		}
		return nil, store.Errorf("transcript", k, err, "")
	}
	name := blobName(run, rec.ID)
	rctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	data, err := v.b.Get(rctx, name)
	switch {
	case errors.Is(err, ErrNotExist):
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "%s was dropped since the view was read", name)
	case err != nil:
		return nil, store.Errorf("transcript", k, err, "read %s", name)
	}
	return data, nil
}

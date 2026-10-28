package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"

	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/filestore"
	"github.com/ContainerSolutions/factory/internal/store/memory"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

// serveStore is the dispatcher's store: the records, retention and transcripts, and Close,
// which runs once the controllers and the server have stopped.
type serveStore interface {
	store.Store
	store.Sweeper
	store.TranscriptStore
	Close() error
}

// internalKinds are the kinds outside v1alpha1 the dispatcher stores, with their Go types: the
// durable stores need each one's type to load it back, and refuse to write a kind they lack.
// TestServeStoreKinds fails when a package adds a stored kind (a store.MetaObject) that isn't
// here, or when this table and the secrets audit's (storeaudit.InternalKinds) differ; and
// TestStoredSchema when a stored type changes without a store.ReadableSchema bump.
func internalKinds() map[string]store.Object {
	return map[string]store.Object{
		auth.KindSession:                 &auth.Session{},
		auth.KindUsedLoginState:          &auth.UsedLoginState{},
		scheduler.KindLease:              &scheduler.Lease{},
		scheduler.KindAgentToken:         &scheduler.AgentToken{},
		scheduler.KindEnrollment:         &scheduler.Enrollment{},
		scheduler.KindAgentRefresh:       &scheduler.AgentRefresh{},
		controllers.KindDelivery:         &controllers.Delivery{},
		controllers.KindEventStatus:      &controllers.EventStatus{},
		controllers.KindDebounce:         &controllers.DebounceRecord{},
		controllers.KindTaskPrompt:       &controllers.TaskPrompt{},
		controllers.KindKillSwitch:       &controllers.KillSwitch{},
		controllers.KindLinkObservations: &controllers.LinkObservations{},
		// The object store's own: its lock records and transcript index (store.md §Object
		// store). The file store registers them too, and never writes them.
		objectstore.KindStoreLock:      &objectstore.StoreLock{},
		objectstore.KindTranscriptBlob: &objectstore.TranscriptBlob{},
	}
}

// fileStoreKinds registers internalKinds with the file store (Open, and Read for factory
// state).
func fileStoreKinds() []filestore.Option {
	kinds := internalKinds()
	options := make([]filestore.Option, 0, len(kinds))
	for kind, proto := range kinds {
		options = append(options, filestore.WithKind(kind, proto))
	}
	return options
}

// objectStoreKinds registers internalKinds with the object store (Open, and Read for factory
// state).
func objectStoreKinds() []objectstore.Option {
	kinds := internalKinds()
	options := make([]objectstore.Option, 0, len(kinds))
	for kind, proto := range kinds {
		options = append(options, objectstore.WithKind(kind, proto))
	}
	return options
}

// memoryStore is the in-memory store, with a Close that does nothing.
type memoryStore struct{ *memory.Store }

func (memoryStore) Close() error { return nil }

// sharedStore is the object store and its bucket: Close closes the store (its final
// compaction), then the bucket, when it holds connections (the GCS and S3 clients).
type sharedStore struct {
	*objectstore.Store
	bucket objectstore.Bucket
}

func (s sharedStore) Close() error {
	err := s.Store.Close()
	return errors.Join(err, closeBucket(s.bucket))
}

// closeBucket closes b when it is an io.Closer.
func closeBucket(b objectstore.Bucket) error {
	if c, ok := b.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("close the bucket: %w", err)
		}
	}
	return nil
}

// stateURLScheme is the scheme of a state URL, the only part of it that is logged (it may
// carry a secret anywhere).
func stateURLScheme(raw string) string {
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return u.Scheme + "://"
	}
	return "(unparsed)"
}

// openStore opens the store cfg asks for: the object store on --state-url, the file store on
// --state-dir, else memory. Open errors (a state dir others can read, one another dispatcher
// holds, an unknown format, corruption with its line, a bucket that can't be reached, a
// snapshot from a newer release) are returned as they are, so serve fails at start saying
// why; none of them prints the state URL.
func openStore(ctx context.Context, cfg serveConfig, opts store.Options, log *slog.Logger) (serveStore, error) {
	if cfg.StateURL != "" {
		b, err := objectstore.ParseURL(ctx, cfg.StateURL)
		if err != nil {
			return nil, fmt.Errorf("open --state-url: %w", err)
		}
		st, err := objectstore.Open(ctx, b, opts, append(objectStoreKinds(), objectstore.WithSyncInterval(cfg.StateSyncInterval))...)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("open --state-url: %w", err), closeBucket(b))
		}
		li := st.Loaded()
		log.Info("store: loaded the shared state", "stateURL", stateURLScheme(cfg.StateURL), "writer", li.Writer,
			"version", li.Version, "objects", li.Objects, "transcripts", li.Transcripts, "snapshot", li.Snapshot,
			"logEntries", li.LogEntries, "readOnly", li.ReadOnly, "syncInterval", cfg.StateSyncInterval.String())
		return sharedStore{Store: st, bucket: b}, nil
	}
	if cfg.StateDir == "" {
		log.Warn("store: in memory; every work item starts fresh on a restart, with no state and no run counters (--state-dir or --state-url keeps them)")
		return memoryStore{memory.New(opts)}, nil
	}
	st, err := filestore.Open(cfg.StateDir, opts, fileStoreKinds()...)
	if err != nil {
		return nil, fmt.Errorf("open --state-dir %s: %w", cfg.StateDir, err)
	}
	li := st.Loaded()
	log.Info("store: loaded the state dir", "stateDir", cfg.StateDir, "version", li.Version,
		"objects", li.Objects, "transcripts", li.Transcripts, "snapshot", li.Snapshot,
		"journalLines", li.JournalLines, "tornLineDropped", li.TornLine)
	return st, nil
}

// closeStore closes st within ctx (the shutdown timeout). The file store's Close compacts the
// journal into the snapshot, and the object store's the log into a snapshot, then closes its
// bucket; if it outlasts ctx, serve returns without waiting, and the next start replays the
// journal or the log, which already hold every change.
func closeStore(ctx context.Context, st serveStore) error {
	done := make(chan error, 1)
	go func() { done <- st.Close() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("close the store: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("close the store: %w (the journal holds every change; the next start replays it)", ctx.Err())
	}
}

package controllers

import (
	"context"
	"fmt"

	"github.com/ContainerSolutions/factory/internal/store"
)

// storeSyncer is a store that can catch up with the writes other dispatchers made since its
// last sync: the object store, whose reads are served from memory and may lag by up to its
// sync interval (P3 §Reads and freshness). Memory and the file store have nothing to sync.
type storeSyncer interface {
	Sync(ctx context.Context) error
}

// syncIfSupported syncs s when it can, and reports whether it did. Lookups whose empty answer
// leads to a create or a drop call it before acting on that answer.
func syncIfSupported(ctx context.Context, s store.Store) (bool, error) {
	sy, ok := s.(storeSyncer)
	if !ok {
		return false, nil
	}
	if err := sy.Sync(ctx); err != nil {
		return false, fmt.Errorf("sync the store: %w", err)
	}
	return true, nil
}

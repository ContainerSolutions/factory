package memory

import (
	"context"
	"fmt"
	"reflect"

	"github.com/ContainerSolutions/factory/internal/store"
)

// The methods in this file serve the durable stores built on memory (an object store, P3): they
// are not part of store.Store.

// Version returns the store's version: the last change applied.
func (s *Store) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rv
}

// Apply applies a change that was committed elsewhere (another store's write, read back from a
// shared log), as if it had been written here: the object, its indexes and the version counter
// change and the watchers are told, in order. The hook isn't called. typ is the object's struct
// type (nil for a delete). A change at or below the store's version is already here and is
// skipped (applied is false); one above the next version is an error (a change is missing), as
// is a change that doesn't fit the state (a create of an object that exists, an update or delete
// of one that doesn't). The store keeps c.Data: don't modify it afterwards.
func (s *Store) Apply(c Change, typ reflect.Type) (applied bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Version <= s.rv {
		return false, nil
	}
	if c.Version != s.rv+1 {
		return false, fmt.Errorf("memory: apply version %d at version %d: a change is missing", c.Version, s.rv)
	}
	old := s.objects[c.Key.Kind][c.Key.Name]
	switch c.Op {
	case OpCreate, OpUpdate:
		if c.Op == OpCreate && old != nil {
			return false, fmt.Errorf("memory: apply create %s: it exists", c.Key)
		}
		if c.Op == OpUpdate && old == nil {
			return false, fmt.Errorf("memory: apply update %s: it doesn't exist", c.Key)
		}
		created := c.At
		if old != nil {
			created = old.created
		}
		rec, err := restoreRecord(Record{Key: c.Key, Type: typ, Data: c.Data, Version: c.Version, Created: created, Updated: c.At}, c.Version)
		if err != nil {
			return false, fmt.Errorf("memory: apply %s %s: %w", c.Op, c.Key, err)
		}
		s.rv = c.Version
		if old == nil {
			s.put(rec)
			s.publish(&change{typ: store.Added, rec: rec})
			return true, nil
		}
		s.unindex(old)
		s.put(rec)
		s.publish(&change{typ: store.Modified, rec: rec, prevLabels: old.labels})
		return true, nil
	case OpDelete:
		if old == nil {
			return false, fmt.Errorf("memory: apply delete %s: it doesn't exist", c.Key)
		}
		s.rv = c.Version
		s.unindex(old)
		delete(s.objects[c.Key.Kind], c.Key.Name)
		s.publish(&change{typ: store.Deleted, rec: old, rv: s.rv})
		if c.Key.Kind == runKind {
			s.deleteTranscript(c.Key.Name)
			s.deleteTranscript(store.EnvironmentLogBlob(c.Key.Name))
		}
		return true, nil
	}
	return false, fmt.Errorf("memory: apply %s: unknown op %q", c.Key, c.Op)
}

// DeleteAt deletes the object at k only if its resourceVersion is version: ErrConflict when it
// has changed since, ErrNotFound when there is none. A lock's holder releases its lock record
// this way, so it never removes a record someone else took over. It goes through the hook as
// Delete does.
func (s *Store) DeleteAt(ctx context.Context, k store.Key, version uint64) error {
	if err := ctx.Err(); err != nil {
		return store.Errorf("delete", k, err, "")
	}
	if k.Kind == "" || k.Name == "" {
		return store.Errorf("delete", k, store.ErrInvalid, "missing kind or name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.objects[k.Kind][k.Name]
	if rec == nil {
		return store.Errorf("delete", k, store.ErrNotFound, "")
	}
	if rec.rv != version {
		return store.Errorf("delete", k, store.ErrConflict, "resourceVersion %d, stored %d", version, rec.rv)
	}
	return s.deleteLocked(ctx, "delete", rec)
}

// Replace swaps the whole state for recs at version, which must not be below the store's: a
// store that fell too far behind a shared log starts again from a snapshot. Every watcher is
// expired (ErrWatchExpired: it re-lists), and the history starts empty at version, as after
// Restore. The hook isn't called. The store keeps each record's Data.
func (s *Store) Replace(recs []Record, version uint64) error {
	built := make([]*record, 0, len(recs))
	seen := map[store.Key]bool{}
	for _, r := range recs {
		rec, err := restoreRecord(r, version)
		if err != nil {
			return fmt.Errorf("memory: replace %s: %w", r.Key, err)
		}
		if seen[r.Key] {
			return fmt.Errorf("memory: replace %s: duplicate", r.Key)
		}
		seen[r.Key] = true
		built = append(built, rec)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if version < s.rv {
		return fmt.Errorf("memory: replace at version %d, below the store's %d", version, s.rv)
	}
	for _, ws := range s.watchers {
		for w := range ws {
			s.endWatch(w, store.Errorf("watch", store.Key{Kind: w.kind}, store.ErrWatchExpired, "the store was reloaded at version %d", version))
		}
	}
	s.objects = map[string]map[string]*record{}
	s.index = map[string]map[string]map[string]map[string]struct{}{}
	for _, rec := range built {
		s.put(rec)
	}
	s.rv = version
	s.history = newRing(len(s.history.buf))
	s.history.trimmedRV = version
	return nil
}

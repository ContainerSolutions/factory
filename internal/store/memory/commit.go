package memory

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/compliance-framework/factory/internal/store"
)

// Option configures New.
type Option func(*Store)

// Op is what a Change did.
type Op string

// Change ops.
const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// Change is one write as the commit hook sees it, before the store applies it.
type Change struct {
	Op  Op
	Key store.Key
	// Version is the store version the change gets (the store's counter after it).
	Version uint64
	// At is the write's time on the store's clock: the object's last-write time for create
	// and update (and its creation time for create), the deletion time for delete.
	At time.Time
	// Data is the object's JSON exactly as the store keeps it (resourceVersion and createdAt
	// set when the kind has them); nil for delete. It is shared with the store: don't modify it.
	Data []byte
}

// CommitHook is called for every Create, Update and Delete (Sweep's too) once the write has
// been checked and encoded but before it is applied, under the store's write lock: changes
// reach it one at a time, in version order. When it returns an error the write fails with it
// (wrapped) and the store is left exactly as it was: no object, version or watch event. It must
// not call back into the store. Locks and transcripts don't go through it.
type CommitHook func(Change) error

// WithCommitHook makes every write go through h first (a durable store journals there).
func WithCommitHook(h CommitHook) Option { return func(s *Store) { s.hook = h } }

// commit passes c to the hook, if there is one. The caller holds s.mu.
func (s *Store) commit(op string, c Change) error {
	if s.hook == nil {
		return nil
	}
	if err := s.hook(c); err != nil {
		return store.Errorf(op, c.Key, err, "commit")
	}
	return nil
}

// Record is one stored object, as Snapshot returns it and Restore takes it.
type Record struct {
	Key store.Key
	// Type is the object's struct type: the object is a *Type.
	Type reflect.Type
	// Data is the object's JSON as the store keeps it. From Snapshot it is shared with the
	// store: don't modify it.
	Data []byte
	// Version is the object's resourceVersion (kept beside it for kinds without one).
	Version uint64
	// Created and Updated are its creation and last-write times (Updated drives SinceUpdated
	// retention).
	Created, Updated time.Time
}

// Snapshot returns every stored object and the store's version, read at one instant. The
// version can be above every object's (deletes advance it too).
func (s *Store) Snapshot() ([]Record, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, byName := range s.objects {
		for _, rec := range byName {
			out = append(out, Record{
				Key: rec.key, Type: rec.typ, Data: rec.data, Version: rec.rv,
				Created: rec.created, Updated: rec.updated,
			})
		}
	}
	return out, s.rv
}

// Restore loads recs into an empty store and sets its version counter to version, as a durable
// store does at start-up. The hook isn't called and watchers aren't told. The watch history
// starts empty at version: a Watch with an older SinceVersion is ErrWatchExpired, as when the
// history has been trimmed. The store keeps each record's Data: don't modify it afterwards.
// Call it before any other use of the store.
func (s *Store) Restore(recs []Record, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rv != 0 || len(s.objects) != 0 {
		return fmt.Errorf("memory: restore into a store that is in use (version %d)", s.rv)
	}
	built := make([]*record, 0, len(recs))
	seen := map[store.Key]bool{}
	for _, r := range recs {
		rec, err := restoreRecord(r, version)
		if err != nil {
			return fmt.Errorf("memory: restore %s: %w", r.Key, err)
		}
		if seen[r.Key] {
			return fmt.Errorf("memory: restore %s: duplicate", r.Key)
		}
		seen[r.Key] = true
		built = append(built, rec)
	}
	for _, rec := range built {
		s.put(rec)
	}
	s.rv = version
	s.history.trimmedRV = version
	return nil
}

func restoreRecord(r Record, version uint64) (*record, error) {
	if r.Type == nil || r.Type.Kind() != reflect.Struct {
		return nil, fmt.Errorf("type %v is not a struct type", r.Type)
	}
	if r.Version == 0 || r.Version > version {
		return nil, fmt.Errorf("version %d outside 1..%d", r.Version, version)
	}
	v := reflect.New(r.Type)
	obj, ok := v.Interface().(store.Object)
	if !ok {
		return nil, fmt.Errorf("*%s is not a store.Object", r.Type)
	}
	if err := json.Unmarshal(r.Data, obj); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if k := store.KeyOf(obj); k != r.Key {
		return nil, fmt.Errorf("the object's key is %s", k)
	}
	if store.HasResourceVersion(obj) && store.ResourceVersion(obj) != store.FormatVersion(r.Version) {
		return nil, fmt.Errorf("the object's resourceVersion is %q, want %d", store.ResourceVersion(obj), r.Version)
	}
	return &record{
		key:       r.Key,
		typ:       r.Type,
		data:      r.Data,
		rv:        r.Version,
		created:   r.Created,
		updated:   r.Updated,
		labels:    maps.Clone(store.Labels(obj)),
		fields:    store.FieldValues(obj),
		retention: store.RetentionOf(obj),
	}, nil
}

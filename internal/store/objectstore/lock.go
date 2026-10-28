package objectstore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// Lock implements store.Store with a StoreLock record written through the log, so a lock holds
// across every dispatcher on the bucket. Lock creates the record, or takes over an expired one
// with a compare-and-swap. While another holder has it, Lock waits until the record is
// released or expires, or ctx ends: it wakes on a local Unlock, on the store's clock reaching
// the expiry, on a sync that changes a lock record, and syncs every second to see other
// dispatchers' releases.
func (s *Store) Lock(ctx context.Context, name string, ttl time.Duration) (store.Unlock, error) {
	k := store.Key{Name: name}
	if name == "" {
		return nil, store.Errorf("lock", k, store.ErrInvalid, "missing name")
	}
	if ttl <= 0 {
		return nil, store.Errorf("lock", k, store.ErrInvalid, "ttl must be positive, got %s", ttl)
	}
	key := store.Key{Kind: KindStoreLock, Name: name}
	for {
		if err := ctx.Err(); err != nil {
			return nil, store.Errorf("lock", k, err, "")
		}
		wake := s.lockWakeCh()
		var cur StoreLock
		err := s.mem.Get(ctx, key, &cur) // the create or CAS below checks it against the log
		now := s.clk.Now()
		switch {
		case errors.Is(err, store.ErrNotFound):
			rec := &StoreLock{
				TypeMeta: v1alpha1.TypeMeta{Kind: KindStoreLock},
				Metadata: v1alpha1.ObjectMeta{Name: name},
				Holder:   s.id, Token: randomHex(8), ExpiresAt: now.Add(ttl),
			}
			err := s.Create(ctx, rec)
			if err == nil {
				return &lease{s: s, name: name, key: key, rec: *rec}, nil
			}
			if !errors.Is(err, store.ErrAlreadyExists) {
				return nil, store.Errorf("lock", k, err, "")
			}
			continue // taken meanwhile: look again
		case err != nil:
			return nil, store.Errorf("lock", k, err, "")
		case !now.Before(cur.ExpiresAt):
			cur.Holder, cur.Token, cur.ExpiresAt = s.id, randomHex(8), now.Add(ttl)
			err := s.Update(ctx, &cur)
			if err == nil {
				return &lease{s: s, name: name, key: key, rec: cur}, nil
			}
			if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
				return nil, store.Errorf("lock", k, err, "")
			}
			continue // reclaimed or released meanwhile: look again
		}

		expiry := s.clk.NewTimerAt(cur.ExpiresAt)
		tick := s.clk.NewTimer(lockSync)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-expiry.C():
		case <-tick.C():
			if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
				s.log.Debug("store: sync while waiting for a lock failed", "lock", name, "err", err)
			}
		}
		expiry.Stop()
		tick.Stop()
	}
}

// lease is a held lock: the record as this holder last wrote it.
type lease struct {
	s    *Store
	name string
	key  store.Key

	mu       sync.Mutex
	rec      StoreLock
	released bool
}

var _ store.Unlock = (*lease)(nil)

// Renew implements store.Unlock: a compare-and-swap of the record, ErrLockLost when the lease
// expired, was released, or the record changed (someone took over the expired lock).
func (l *lease) Renew(ctx context.Context, ttl time.Duration) error {
	k := store.Key{Name: l.name}
	if err := ctx.Err(); err != nil {
		return store.Errorf("renew", k, err, "")
	}
	if ttl <= 0 {
		return store.Errorf("renew", k, store.ErrInvalid, "ttl must be positive, got %s", ttl)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return store.Errorf("renew", k, store.ErrLockLost, "released")
	}
	now := l.s.clk.Now()
	if !now.Before(l.rec.ExpiresAt) {
		return store.Errorf("renew", k, store.ErrLockLost, "expired at %s", l.rec.ExpiresAt.Format(time.RFC3339Nano))
	}
	next := l.rec
	next.ExpiresAt = now.Add(ttl)
	switch err := l.s.Update(ctx, &next); {
	case err == nil:
		l.rec = next
		return nil
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		l.released = true
		return store.Errorf("renew", k, store.ErrLockLost, "taken over")
	default:
		return store.Errorf("renew", k, err, "")
	}
}

// Unlock implements store.Unlock: it deletes the record only at the version this holder last
// wrote, so it never removes a lock someone else took over after it expired. It writes under
// its own deadline (the request timeout); a failure is logged at warn and the record is left
// to expire, after which anyone can take it and the sweeper deletes it.
func (l *lease) Unlock() {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	rv := l.rec.Metadata.ResourceVersion
	expires := l.rec.ExpiresAt
	l.mu.Unlock()

	s := l.s
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.requestTimeout)
	defer cancel()
	err := s.deleteAt(ctx, l.key, rv)
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
		s.log.Warn("store: unlock failed; the lock is held until it expires", "lock", l.name, "expiresAt", expires, "err", err)
	}
	s.wakeLocks()
}

// sweepLocks deletes the lock records that have expired (a holder that crashed, or an Unlock
// that failed), each at the version read, and returns how many.
func (s *Store) sweepLocks(ctx context.Context, now time.Time) (int, error) {
	var locks []*StoreLock
	if _, err := s.mem.List(ctx, KindStoreLock, store.ListOptions{}, &locks); err != nil {
		return 0, err
	}
	n := 0
	for _, l := range locks {
		if now.Before(l.ExpiresAt) {
			continue
		}
		err := s.deleteAt(ctx, store.KeyOf(l), l.Metadata.ResourceVersion)
		switch {
		case err == nil:
			n++
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
		default:
			return n, err
		}
	}
	if n > 0 {
		s.wakeLocks()
	}
	return n, nil
}

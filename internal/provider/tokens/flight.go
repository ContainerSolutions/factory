package tokens

import (
	"context"
	"sync"
)

// flight shares one call per key between concurrent callers. Its map is guarded by the
// Minter's mutex, passed to do, so the cache and the calls in flight change under one lock.
type flight[T any] struct {
	calls map[string]*call[T]
}

type call[T any] struct {
	done    chan struct{}
	val     T
	err     error
	waiters int
	// abandoned is set when fn failed after its caller's context ended: the failure is the
	// caller's, not the key's.
	abandoned bool
}

// do runs fn for key unless a call for key is already in flight, in which case it waits for
// that call's result or for ctx to end. retry is true when the shared call failed only because
// the caller that ran it gave up, while ctx is still live: the caller should try again.
func (f *flight[T]) do(ctx context.Context, mu *sync.Mutex, key string, fn func() (T, error)) (v T, retry bool, err error) {
	mu.Lock()
	if c, ok := f.calls[key]; ok {
		c.waiters++
		mu.Unlock()
		select {
		case <-c.done:
			if c.abandoned && ctx.Err() == nil {
				var zero T
				return zero, true, c.err
			}
			return c.val, false, c.err
		case <-ctx.Done():
			mu.Lock()
			c.waiters--
			mu.Unlock()
			var zero T
			return zero, false, ctx.Err()
		}
	}
	if f.calls == nil {
		f.calls = map[string]*call[T]{}
	}
	c := &call[T]{done: make(chan struct{})}
	f.calls[key] = c
	mu.Unlock()

	defer func() {
		mu.Lock()
		delete(f.calls, key)
		mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn()
	c.abandoned = c.err != nil && ctx.Err() != nil
	return c.val, false, c.err
}

// waitersLocked is the number of callers waiting on key's call in flight.
func (f *flight[T]) waitersLocked(key string) int {
	if c, ok := f.calls[key]; ok {
		return c.waiters
	}
	return 0
}

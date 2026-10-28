package controllers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
)

// The leader lock (P3 §Several dispatchers: who runs what).
const (
	// LeaderLock is the store lock whose holder runs the loops that write from a watch or a
	// timer: the controllers, the scheduler's lease controller and Refresh, the sweeper and the
	// installation check.
	LeaderLock = "leader"
	// DefaultLeaderTTL is the leader lock's lease.
	DefaultLeaderTTL = 3 * time.Minute
	// DefaultLeaderRenew is how often the holder renews the lock and every other instance tries
	// to take it.
	DefaultLeaderRenew = time.Minute
	// DefaultLeaderTry bounds each Lock and Renew call: Lock waits while another holder has
	// the lock.
	DefaultLeaderTry = 5 * time.Second
)

// Gate holds work until this process may do it. The Leader is the one in use: a controller
// with a gate checks it before each reconcile (Controller.SetGate).
type Gate interface {
	// Held reports whether this process may do the work now.
	Held() bool
	// Wait blocks until Held, or until ctx ends (its error).
	Wait(ctx context.Context) error
}

// LeaderOptions configure a Leader. Zero fields take the defaults.
type LeaderOptions struct {
	// TTL is the lock's lease (DefaultLeaderTTL); Renew how often it is renewed or tried
	// (DefaultLeaderRenew); Try bounds each attempt (DefaultLeaderTry).
	TTL, Renew, Try time.Duration
	// Clock times the attempts and the lease as this process sees it. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
}

// Leader takes and keeps the store's leader lock. Every instance runs one: it tries to take
// the lock at start and every Renew, and the holder renews it every Renew. Held is true from
// a successful Lock or Renew until the lease that call set ends on this process's clock, or
// until a Renew fails; so a holder paused past its lease stops its loops before another
// instance can take over. With one dispatcher it is always the leader.
type Leader struct {
	s    store.Store
	opts LeaderOptions
	log  *slog.Logger

	mu    sync.Mutex
	lease store.Unlock
	until time.Time     // Held while the clock is before it
	term  chan struct{} // closed when the current term ends
	wake  chan struct{} // closed (and replaced) on every successful Lock or Renew
}

var _ Gate = (*Leader)(nil)

// NewLeader returns a Leader on s's lock LeaderLock. Call Run.
func NewLeader(s store.Store, opts LeaderOptions) *Leader {
	if opts.TTL <= 0 {
		opts.TTL = DefaultLeaderTTL
	}
	if opts.Renew <= 0 {
		opts.Renew = DefaultLeaderRenew
	}
	if opts.Try <= 0 {
		opts.Try = DefaultLeaderTry
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Leader{s: s, opts: opts, log: opts.Logger.With("component", "leader"), wake: make(chan struct{})}
}

// Run tries to take the lock at once and then every Renew, and renews it while it holds it,
// until ctx ends. It doesn't give the lock up: call Release once the leader's loops have
// stopped.
func (l *Leader) Run(ctx context.Context) error {
	for {
		l.step(ctx)
		t := l.opts.Clock.NewTimer(l.opts.Renew)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C():
		}
	}
}

// step renews the lock when this process holds it, else tries to take it.
func (l *Leader) step(ctx context.Context) {
	l.mu.Lock()
	lease := l.lease
	l.mu.Unlock()
	tctx, cancel := context.WithTimeout(ctx, l.opts.Try)
	defer cancel()
	start := l.opts.Clock.Now()
	if lease != nil {
		if err := lease.Renew(tctx, l.opts.TTL); err != nil {
			if ctx.Err() != nil {
				return
			}
			l.log.Warn("leader: lost the leader lock; stopping the leader's loops", "err", err)
			l.endTerm(lease)
			return
		}
		l.held(lease, start, true)
		return
	}
	u, err := l.s.Lock(tctx, LeaderLock, l.opts.TTL)
	switch {
	case ctx.Err() != nil:
		if err == nil {
			u.Unlock()
		}
		return
	case errors.Is(err, context.DeadlineExceeded):
		l.log.Debug("leader: another instance holds the leader lock")
		return
	case err != nil:
		l.log.Warn("leader: taking the leader lock failed; trying again", "err", err, "in", l.opts.Renew.String())
		return
	}
	l.log.Info("leader: this instance holds the leader lock and runs the leader's loops", "ttl", l.opts.TTL.String())
	l.held(u, start, false)
}

// held records a successful Lock, or a Renew of lease, made at start. A Renew of a lease the
// term no longer holds (Release meanwhile) changes nothing.
func (l *Leader) held(lease store.Unlock, start time.Time, renewed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if renewed && l.lease != lease {
		return
	}
	if l.lease == nil {
		l.term = make(chan struct{})
	}
	l.lease, l.until = lease, start.Add(l.opts.TTL)
	close(l.wake)
	l.wake = make(chan struct{})
}

// endTerm forgets lease, ending its term, if it is still the current one.
func (l *Leader) endTerm(lease store.Unlock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lease != lease {
		return
	}
	l.lease, l.until = nil, time.Time{}
	close(l.term)
}

// Release gives the lock up, when this process holds it, and ends the term. Call it once the
// leader's loops have stopped, so another instance takes over at its next try.
func (l *Leader) Release() {
	l.mu.Lock()
	lease := l.lease
	l.mu.Unlock()
	if lease == nil {
		return
	}
	l.endTerm(lease)
	lease.Unlock()
	l.log.Info("leader: released the leader lock")
}

// Held implements Gate: this process holds the lock and its lease hasn't ended on this
// process's clock.
func (l *Leader) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heldLocked()
}

func (l *Leader) heldLocked() bool {
	return l.lease != nil && l.opts.Clock.Now().Before(l.until)
}

// Wait implements Gate. It checks again on every Lock or Renew, and at least every Renew.
func (l *Leader) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		held, wake := l.heldLocked(), l.wake
		l.mu.Unlock()
		if held {
			return nil
		}
		t := l.opts.Clock.NewTimer(l.opts.Renew)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-wake:
		case <-t.C():
		}
		t.Stop()
	}
}

// While runs fn for each term this process leads, under a context that ends with the term
// (a failed Renew, or Release), until ctx ends. fn's error is logged; the next term runs it
// again.
func (l *Leader) While(ctx context.Context, name string, fn func(ctx context.Context) error) {
	for {
		if l.Wait(ctx) != nil {
			return
		}
		l.mu.Lock()
		term := l.term
		l.mu.Unlock()
		if term == nil {
			continue
		}
		tctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-term:
				cancel()
			case <-tctx.Done():
			}
		}()
		if err := fn(tctx); err != nil && tctx.Err() == nil {
			l.log.Warn("leader: a leader's loop failed; it runs again at the next term", "loop", name, "err", err)
		}
		cancel()
		// Wait for this term to end before the next one starts fn again.
		select {
		case <-ctx.Done():
			return
		case <-term:
		}
	}
}

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
)

// ConnSource gives the controllers the live connection of a Provider instance by name. The
// provider controller, which connects one Conn per catalog Provider, implements it; tests use
// StaticConns.
type ConnSource interface {
	Conn(name string) (provider.Conn, bool)
}

// StaticConns is a fixed ConnSource.
type StaticConns map[string]provider.Conn

// Conn implements ConnSource.
func (s StaticConns) Conn(name string) (provider.Conn, bool) {
	c, ok := s[name]
	return c, ok
}

// DefaultObservationTTL is how long an observation is reused (the LLD's 30 s view cache).
const DefaultObservationTTL = 30 * time.Second

// ObservationCache caches Conn.Observe per object for a TTL. The rule controller reads through
// it when it builds a WorkItemView; the work-item controller's resync refreshes it. It is safe
// for concurrent use.
type ObservationCache struct {
	conns ConnSource
	clk   clock.Clock
	ttl   time.Duration

	mu      sync.Mutex
	entries map[string]cachedObservation
	warned  map[string]bool // forbidden links already reported, until they observe again
}

type cachedObservation struct {
	obs provider.Observation
	err error // provider.ErrNotFound and a forbidden error are cached too: neither is transient
	at  time.Time
}

// NewObservationCache returns a cache over conns (ttl ≤ 0: DefaultObservationTTL).
func NewObservationCache(conns ConnSource, clk clock.Clock, ttl time.Duration) *ObservationCache {
	if clk == nil {
		clk = clock.Real{}
	}
	if ttl <= 0 {
		ttl = DefaultObservationTTL
	}
	return &ObservationCache{conns: conns, clk: clk, ttl: ttl, entries: map[string]cachedObservation{}, warned: map[string]bool{}}
}

func cacheKey(l v1alpha1.Link) string { return l.Provider + "/" + l.Ref }

// Observe returns the link's observation, from the cache when it is fresher than the TTL.
// A missing object is provider.ErrNotFound; one the provider won't show, provider.ErrForbidden.
func (c *ObservationCache) Observe(ctx context.Context, l v1alpha1.Link) (provider.Observation, error) {
	obs, _, err := c.observe(ctx, l)
	return obs, err
}

// observe is Observe, also returning when the observation was made.
func (c *ObservationCache) observe(ctx context.Context, l v1alpha1.Link) (provider.Observation, time.Time, error) {
	k := cacheKey(l)
	c.mu.Lock()
	e, ok := c.entries[k]
	c.mu.Unlock()
	if ok && c.clk.Now().Sub(e.at) < c.ttl {
		return e.obs, e.at, e.err
	}
	return c.refresh(ctx, l)
}

// Refresh observes the link now and caches the result.
func (c *ObservationCache) Refresh(ctx context.Context, l v1alpha1.Link) (provider.Observation, error) {
	obs, _, err := c.refresh(ctx, l)
	return obs, err
}

func (c *ObservationCache) refresh(ctx context.Context, l v1alpha1.Link) (provider.Observation, time.Time, error) {
	conn, ok := c.conns.Conn(l.Provider)
	if !ok {
		return provider.Observation{}, time.Time{}, fmt.Errorf("observe %s: no connection for provider %q", l.Ref, l.Provider)
	}
	obs, err := conn.Observe(ctx, v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref})
	if err != nil && !isNotFound(err) && !isForbidden(err) {
		return provider.Observation{}, time.Time{}, err // transient: not cached
	}
	at := c.clk.Now()
	c.mu.Lock()
	c.entries[cacheKey(l)] = cachedObservation{obs: obs, err: err, at: at}
	if !isForbidden(err) {
		delete(c.warned, cacheKey(l)) // visible again: a later refusal is news
	}
	c.mu.Unlock()
	return obs, at, err
}

// firstForbidden reports whether the link's refusal is news: true the first time it is asked
// since the link was last observed (or ever), so a forbidden link is reported once.
func (c *ObservationCache) firstForbidden(l v1alpha1.Link) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warned[cacheKey(l)] {
		return false
	}
	c.warned[cacheKey(l)] = true
	return true
}

// Forget drops the link's cached observation.
func (c *ObservationCache) Forget(l v1alpha1.Link) {
	c.mu.Lock()
	delete(c.entries, cacheKey(l))
	c.mu.Unlock()
}

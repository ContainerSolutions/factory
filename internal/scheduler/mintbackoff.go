package scheduler

import (
	"time"

	"github.com/compliance-framework/factory/internal/catalog"
)

// The git-credential backoff: a pending task whose credentials failed to mint isn't offered to
// any agent again before its delay has passed, so a missing App installation costs one
// provider call per delay rather than one per poll.
const (
	// MintBackoffInitial is the delay after a task's first failed mint.
	MintBackoffInitial = 30 * time.Second
	// MintBackoffMax caps the delay, which doubles on each failure after the first.
	MintBackoffMax = 10 * time.Minute
)

// mintBackoff is one task's backoff: its current delay and when it may be tried again.
type mintBackoff struct {
	delay time.Duration
	next  time.Time
}

// mintBackingOff reports whether taskID waits out a mint backoff at now. The caller holds s.mu.
func (s *Scheduler) mintBackingOff(taskID string, now time.Time) bool {
	b, ok := s.mintBackoff[taskID]
	return ok && now.Before(b.next)
}

// backOffMint records a failed mint of taskID at now and returns the delay before the next
// try: MintBackoffInitial, then doubled up to MintBackoffMax. The caller holds s.mu.
func (s *Scheduler) backOffMint(taskID string, now time.Time) mintBackoff {
	b, ok := s.mintBackoff[taskID]
	switch {
	case !ok:
		b.delay = MintBackoffInitial
	default:
		b.delay = min(2*b.delay, MintBackoffMax)
	}
	b.next = now.Add(b.delay)
	s.mintBackoff[taskID] = b
	return b
}

// forgetMintBackoffs clears every backoff when the catalog changed since the last snapshot
// (a swapped-in catalog, or a resync-catalog reload, may fix the provider, so the tasks are
// tried at once). The caller holds s.mu.
func (s *Scheduler) forgetMintBackoffs(cat *catalog.Catalog) {
	if cat == s.mintCatalog {
		return
	}
	s.mintCatalog = cat
	clear(s.mintBackoff)
}

//go:build unix

package host

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockHost takes an exclusive, non-blocking flock on <statedir>/host.lock, which must exist.
// The returned func releases it, as does the process ending.
func lockHost(e *Env) (func(), error) {
	p := hostLockPath(e)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the host lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held", ErrHostLocked, p)
		}
		return nil, fmt.Errorf("the host lock: flock %s: %w", p, err)
	}
	return func() { _ = f.Close() }, nil
}

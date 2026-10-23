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
	return lockFile(hostLockPath(e), "the host lock", ErrHostLocked)
}

// LockSupervise takes an exclusive, non-blocking flock on <statedir>/supervise.lock, whose
// dir must exist; ErrSuperviseLocked while another supervise holds it. The returned func
// releases it, as does the process ending.
func LockSupervise(e *Env) (func(), error) {
	return lockFile(superviseLockPath(e), "the supervise lock", ErrSuperviseLocked)
}

// lockFile flocks p, creating it; held is the error while another process holds it.
func lockFile(p, what string, held error) (func(), error) {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held", held, p)
		}
		return nil, fmt.Errorf("%s: flock %s: %w", what, p, err)
	}
	return func() { _ = f.Close() }, nil
}

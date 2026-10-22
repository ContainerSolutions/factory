//go:build unix

package filestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// lockDir opens path (creating it) and takes an exclusive, non-blocking flock on it. Closing
// the file releases the lock, as does the process ending.
func lockDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filestore: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("filestore: %w: %s is held: is another dispatcher running on this state dir?", ErrLocked, path)
		}
		return nil, fmt.Errorf("filestore: flock %s: %w", path, err)
	}
	return f, nil
}

// owner returns fi's owner and whether it is this process's user.
func owner(fi fs.FileInfo) (uid int, mine bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, true
	}
	return int(st.Uid), int(st.Uid) == os.Geteuid()
}

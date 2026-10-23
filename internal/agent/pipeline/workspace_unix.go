//go:build unix

package pipeline

import (
	"errors"
	"os"
	"syscall"
)

// openRootDir opens dir as a directory without following a symlink, in one open, so every
// later check (Stat, Readdirnames) is on that fd: no check-then-open race with a Root swapped
// for a link in between. A symlink or a non-directory is errRootNotDir.
func openRootDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return nil, errRootNotDir
	}
	return f, err
}

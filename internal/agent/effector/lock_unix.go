//go:build unix

package effector

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f, waiting for it: the CLI processes and the MCP
// server of one run share the state file.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

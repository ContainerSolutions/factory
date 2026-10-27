// Package execfile writes files that will be exec'd (the factory shim, a skill's scripts, a
// harness's copied plugins, the environment's scripts, extracted binaries) so that exec'ing
// them can't fail with ETXTBSY ("text file busy").
//
// factory-agent forks git, harnesses, docker and helpers from several goroutines at once. A
// child forked while a file is open for writing inherits that descriptor until its own exec
// closes it (O_CLOEXEC), and exec'ing the file in that window fails with ETXTBSY. Holding
// syscall.ForkLock for reading from a file's open to its close keeps every fork out of that
// window (forks take it for writing). Renaming the file afterwards wouldn't help: the child
// holds the same inode.
//
// A file whose mode has no exec bit is written without the lock: nothing execs it, and the
// lock would only hold up forks.
package execfile

import (
	"io/fs"
	"os"
	"syscall"
)

// WriteFile is os.WriteFile, under syscall.ForkLock when perm has an exec bit.
func WriteFile(name string, data []byte, perm fs.FileMode) error {
	return OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// OpenFile opens name with os.OpenFile(name, flag, perm), lets fill write it and closes it,
// all under syscall.ForkLock when perm has an exec bit. fill must not start a process or take
// syscall.ForkLock itself: either would deadlock. A fill error is returned with the file
// closed; a close error is returned when fill succeeded.
func OpenFile(name string, flag int, perm fs.FileMode, fill func(*os.File) error) error {
	return create(perm, func() (*os.File, error) { return os.OpenFile(name, flag, perm) }, fill)
}

// RootOpenFile is OpenFile through root (root.OpenFile(name, flag, perm)).
func RootOpenFile(root *os.Root, name string, flag int, perm fs.FileMode, fill func(*os.File) error) error {
	return create(perm, func() (*os.File, error) { return root.OpenFile(name, flag, perm) }, fill)
}

func create(perm fs.FileMode, open func() (*os.File, error), fill func(*os.File) error) error {
	if perm&0o111 != 0 {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
	}
	f, err := open()
	if err != nil {
		return err
	}
	if err := fill(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

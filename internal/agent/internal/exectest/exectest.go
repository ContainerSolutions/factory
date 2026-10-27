// Package exectest writes the scripts factory-agent's tests exec (fake harnesses, wrappers,
// stubs) so that exec'ing them can't fail with ETXTBSY. It is imported by tests only.
package exectest

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// Write writes script to path with mode, under syscall.ForkLock: no process the test binary
// forks meanwhile (parallel tests run git, the fakes and the wrappers) inherits the file open
// for writing. Exec'ing it while such a child is between its fork and its exec fails with
// ETXTBSY ("text file busy"). A rename after the write wouldn't help: the child holds the same
// inode. The parent directory must exist.
func Write(t testing.TB, path, script string, mode fs.FileMode) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte(script), mode)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}

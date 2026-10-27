// Package exectest writes the scripts factory-agent's tests exec (fake harnesses, wrappers,
// stubs) through execfile, so that exec'ing them can't fail with ETXTBSY while parallel tests
// fork. It is imported by tests only.
package exectest

import (
	"io/fs"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/internal/execfile"
)

// Write writes script to path with mode (which needs an exec bit to be exec'd, and to be
// written under syscall.ForkLock), failing the test on an error. The parent directory must
// exist.
func Write(t testing.TB, path, script string, mode fs.FileMode) {
	t.Helper()
	if err := execfile.WriteFile(path, []byte(script), mode); err != nil {
		t.Fatal(err)
	}
}

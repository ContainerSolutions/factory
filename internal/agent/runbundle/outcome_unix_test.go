//go:build unix

package runbundle

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestOutcomeFIFO: a FIFO for outcome.json never blocks the reader, whether ReadOutcome's
// Lstat sees it or it's swapped in after (openRegular opens it non-blocking).
func TestOutcomeFIFO(t *testing.T) {
	out := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(out, OutcomeFile), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	within := func(name string, f func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- f() }()
		select {
		case err := <-done:
			wantInvalid(t, err, OutcomeFile)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked on a FIFO", name)
		}
	}
	within("ReadOutcome", func() error { _, err := ReadOutcome(out, t0); return err })
	within("openRegular", func() error {
		root, err := os.OpenRoot(out)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		f, _, err := openRegular(root, OutcomeFile)
		if f != nil {
			_ = f.Close()
		}
		return err
	})
}

package exectest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// TestWrite: a script written while other goroutines keep forking execs at once, every time.
// Without the fork lock this fails with "text file busy" within a few dozen writes.
func TestWrite(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command(sh, "-c", ":").Run()
			}
		})
	}
	defer func() { close(stop); wg.Wait() }()
	for i := range 200 {
		p := filepath.Join(dir, fmt.Sprintf("s%d", i))
		Write(t, p, "#!/bin/sh\necho ok\n", 0o700)
		out, err := exec.Command(p).Output()
		if err != nil || string(out) != "ok\n" {
			t.Fatalf("write %d: exec %s: %q, %v", i, p, out, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "s0")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, %v; want 0700", fi, err)
	}
}

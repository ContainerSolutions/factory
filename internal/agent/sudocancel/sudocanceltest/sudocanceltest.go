// Package sudocanceltest checks that a runner cancels commands the sudocancel way, with a
// stand-in for sudo: real sudo never runs.
package sudocanceltest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/internal/exectest"
)

// RunFunc runs prog (a path) with args until it exits or ctx ends, as the runner under test
// does.
type RunFunc func(ctx context.Context, prog string, args ...string) error

// standIn is a stand-in for sudo and its command. It writes <itself>.ready once it runs. On
// SIGTERM it writes <itself>.term and then, as the kata helper does, takes a moment to clean up
// and writes <itself>.cleaned before it exits; with <itself>.stubborn present it runs on
// instead, so only a SIGKILL ends it. A SIGKILL leaves neither file.
const standIn = `#!/bin/sh
trap ': > "$0.term"; [ -e "$0.stubborn" ] || { sleep 0.1; : > "$0.cleaned"; exit 143; }' TERM
: > "$0.ready"
while :; do sleep 0.05; done
`

// Check runs each case through run, cancels it once it runs and checks how it ended: a sudo
// call gets SIGTERM and its time to clean up, and a stubborn one SIGKILL only after grace (the
// runner's, which a test sets short, but above a second); another program gets SIGKILL at once.
func Check(t *testing.T, grace time.Duration, run RunFunc) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tt := range []struct {
		name, prog string
		args       []string
		stubborn   bool
		term       bool // it gets SIGTERM, not SIGKILL, first
	}{
		{"helper verb", "sudo", []string{"-n", "/usr/local/libexec/factory-agent/kata-helper", "rm", "ws-tsk_x"}, false, true},
		{"root script", "sudo", []string{"/bin/sh", "root.sh"}, false, true},
		{"stubborn helper verb", "sudo", []string{"-n", "/usr/local/libexec/factory-agent/kata-helper", "start", "ws-tsk_x"}, true, true},
		{"stubborn root script", "sudo", []string{"/bin/sh", "root.sh"}, true, true},
		{"another program", "tart", []string{"-n", "x"}, false, false},
		{"another program, -n", "install", []string{"-n", "x"}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), tt.prog)
			// through exectest: a parallel test's fork could otherwise hold the file open for
			// writing when run execs it ("text file busy").
			exectest.Write(t, p, standIn, 0o700)
			if tt.stubborn {
				if err := os.WriteFile(p+".stubborn", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, p, tt.args...) }()
			deadline := time.After(10 * time.Second)
			for !exists(p + ".ready") {
				select {
				case err := <-done:
					t.Fatalf("the call ended before the stand-in started: %v", err)
				case <-deadline:
					t.Fatal("the stand-in didn't start")
				case <-time.After(10 * time.Millisecond):
				}
			}
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Error("a cancelled call succeeded")
				}
			case <-time.After(grace + 10*time.Second):
				t.Fatalf("the call ran on %s after its context ended", grace+10*time.Second)
			}
			took := time.Since(start)
			if got := exists(p + ".term"); got != tt.term {
				t.Errorf("got SIGTERM %v, want %v", got, tt.term)
			}
			switch {
			case !tt.term:
				if took >= grace {
					t.Errorf("killed after %s, not at once (the grace is %s)", took, grace)
				}
			case !tt.stubborn:
				if !exists(p + ".cleaned") {
					t.Errorf("killed before it cleaned up (after %s)", took)
				}
			default:
				if took < grace {
					t.Errorf("killed after %s, before the grace (%s)", took, grace)
				}
			}
		})
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

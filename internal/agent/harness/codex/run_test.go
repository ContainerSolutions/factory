package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
)

const okResult = `{"status":"done","outcome":"pr-opened","summary":"opened acme/docs#13"}`

// slow bounds what a loaded machine can take for what an idle one does in milliseconds:
// starting the fake (a re-exec of the race-built test binary, which macOS also scans on its first
// runs) or stopping it. The fakes that would hang block forever, so a hang still fails.
const slow = time.Minute

// prepared is a prepared VM workspace and an adapter running the fake codex.
func prepared(t *testing.T, writable bool, plan fakePlan) (*Adapter, harness.Workspace, *fakeCodex) {
	t.Helper()
	fc := newFakeCodex(t, plan)
	a := vmAdapter(t, fc.Binary)
	ws := vmWorkspace(t, writable)
	if err := a.Prepare(context.Background(), ws, task("", "")); err != nil {
		t.Fatal(err)
	}
	return a, ws, fc
}

func TestRunFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		run       fakeRun
		wantErr   string
		wantLimit time.Time
	}{
		{"usage limit in the stream", fakeRun{Stream: "codex-ratelimit.jsonl", Exit: 1}, "rate limited until 2026-10-19T14:05:00Z", t0.Add(2*time.Hour + 5*time.Minute)},
		{"usage limit on stderr only", fakeRun{Stderr: "Error: You've hit your usage limit. Try again in 3 hours.\n", Exit: 1}, "rate limited", t0.Add(3 * time.Hour)},
		{"a failed turn", fakeRun{Stream: "codex-error.jsonl", Stderr: "Error: stream disconnected\n", Exit: 1}, "exit status 1: stream disconnected before completion", time.Time{}},
		{"a failed turn, exit 0", fakeRun{Stream: "codex-error.jsonl"}, "the turn failed: stream disconnected", time.Time{}},
		{"no output", fakeRun{Stderr: "Error: unexpected argument '--frobnicate' found\n", Exit: 2}, "exit status 2: Error: unexpected argument", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws, _ := prepared(t, true, fakePlan{Runs: []fakeRun{tt.run}})
			s := &harnesstest.Recorder{}
			_, err := a.Run(context.Background(), ws, task("gpt-5-codex", "high"), "p", s)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err %v, want %q", err, tt.wantErr)
			}
			if tt.wantLimit.IsZero() != (len(s.Limits) == 0) || (len(s.Limits) > 0 && !s.Limits[0].Equal(tt.wantLimit)) {
				t.Errorf("rate limits %v, want %v", s.Limits, tt.wantLimit)
			}
		})
	}
}

func TestRepair(t *testing.T) {
	t.Parallel()
	t.Run("resumes the session", func(t *testing.T) {
		t.Parallel()
		a, ws, fc := prepared(t, true, fakePlan{Runs: []fakeRun{
			{Stream: "codex-success.jsonl", LastMessage: `{"status":"done"}`},
			{Stream: "codex-success.jsonl", LastMessage: okResult},
		}})
		if _, err := a.Run(context.Background(), ws, task("gpt-5", "medium"), "the prompt", &harnesstest.Recorder{}); err != nil {
			t.Fatal(err)
		}
		raw, err := a.Repair(context.Background(), ws, []string{"/outcome: missing"}, &harnesstest.Recorder{})
		if err != nil {
			t.Fatal(err)
		}
		if string(raw.Result) != okResult {
			t.Errorf("repair result %q", raw.Result)
		}
		c := fc.calls()[1]
		i := slices.Index(c.Args, "resume")
		if i < 0 || c.Args[i+1] != "0199f2a1-6b1e-7c33-9a51-2f0c4e8d1a01" || !slices.Contains(c.Args, `model_reasoning_effort="medium"`) || !slices.Contains(c.Args, "--model") {
			t.Errorf("repair args %q", c.Args)
		}
		if !strings.Contains(c.Stdin, "- /outcome: missing") || !strings.Contains(c.Stdin, ws.Result) || strings.Contains(c.Stdin, "the prompt") {
			t.Errorf("repair prompt %q", c.Stdin)
		}
	})
	t.Run("a new exec without a thread", func(t *testing.T) {
		t.Parallel()
		a, ws, fc := prepared(t, true, fakePlan{Runs: []fakeRun{{Stream: "codex-token-count.jsonl"}}})
		if _, err := a.Run(context.Background(), ws, task("gpt-5", "low"), "the prompt", &harnesstest.Recorder{}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Repair(context.Background(), ws, []string{"no result"}, &harnesstest.Recorder{}); err != nil {
			t.Fatal(err)
		}
		c := fc.calls()[1]
		if slices.Contains(c.Args, "resume") || !strings.HasPrefix(c.Stdin, "the prompt") || !strings.Contains(c.Stdin, "- no result") {
			t.Errorf("repair args %q, prompt %q", c.Args, c.Stdin)
		}
	})
	t.Run("nothing to repair", func(t *testing.T) {
		t.Parallel()
		a, ws, _ := prepared(t, true, fakePlan{})
		if _, err := a.Repair(context.Background(), ws, []string{"x"}, &harnesstest.Recorder{}); err == nil {
			t.Error("Repair before Run succeeded")
		}
	})
}

// TestRunCancelKillsTheProcessTree: a cancelled run ends promptly although codex and the
// command it started both ignore SIGTERM, and neither process survives.
func TestRunCancelKillsTheProcessTree(t *testing.T) {
	t.Parallel()
	a, ws, fc := prepared(t, true, fakePlan{Runs: []fakeRun{{Stream: "codex-success.jsonl", Child: true, Block: true, IgnoreTerm: true}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &harnesstest.Recorder{}
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, ws, task("gpt-5", "high"), "p", s)
		done <- err
	}()
	var pids []int
	for deadline := time.Now().Add(slow); len(pids) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the fake codex never started its child")
		}
		time.Sleep(20 * time.Millisecond)
		pids = fc.pids()
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(slow):
		t.Fatal("Run didn't return after cancel")
	}
	for _, pid := range pids {
		gone := false
		for deadline := time.Now().Add(slow); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				gone = true
				break
			}
		}
		if !gone {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("process %d survived the cancel", pid)
		}
	}
}

// TestRunRateLimitStopsTheRun: the pipeline cancels the run on RateLimited; codex, still
// running, is stopped.
func TestRunRateLimitStopsTheRun(t *testing.T) {
	t.Parallel()
	a, ws, _ := prepared(t, true, fakePlan{Runs: []fakeRun{{Stream: "codex-ratelimit.jsonl", Block: true}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &harnesstest.Recorder{OnLimit: cancel}
	start := time.Now()
	_, err := a.Run(ctx, ws, task("gpt-5", "high"), "p", s)
	if !errors.Is(err, context.Canceled) || len(s.Limits) != 1 || time.Since(start) > slow {
		t.Errorf("Run = %v after %s, limits %v", err, time.Since(start), s.Limits)
	}
}

func TestFinalMessage(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "last")
	if got := finalMessage(f, "streamed"); string(got) != "streamed" {
		t.Errorf("no file: %q", got)
	}
	if err := os.WriteFile(f, []byte("```json\n{\"a\":1}\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := finalMessage(f, "streamed"); string(got) != `{"a":1}` {
		t.Errorf("fenced file: %q", got)
	}
	if got := finalMessage(filepath.Join(dir, "none"), "  "); got != nil {
		t.Errorf("nothing: %q", got)
	}
}

func TestSessionRecord(t *testing.T) {
	dir := t.TempDir()
	if err := writeSession(dir, session{ThreadID: "t", Sandbox: sandboxDangerFullAccess}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, sessionFile))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["threadId"] != "t" {
		t.Errorf("session file %s", b)
	}
}

// sameDir compares directories through symlinks (macOS's /var is /private/var).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

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

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/clock"
)

const okResult = `{"status":"done","outcome":"pr-opened","summary":"opened acme/docs#13"}`

// prepared is a prepared workspace and an adapter running the fake codex.
func prepared(t *testing.T, writable bool, plan fakePlan) (*Adapter, harness.Workspace, *fakeCodex) {
	t.Helper()
	fc := newFakeCodex(t, plan)
	a := New(Options{Config: config.Harness{Home: ownerHome(t), Default: "gpt-5-codex"}, Binary: fc.Binary,
		Clock: clock.NewFake(t0), KillGrace: 200 * time.Millisecond})
	ws := workspace(t, writable)
	if err := a.Prepare(context.Background(), ws, task("", "")); err != nil {
		t.Fatal(err)
	}
	return a, ws, fc
}

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		writable bool
		model    string
		effort   string
		run      fakeRun
		wantArgs []string
		result   string
	}{
		{"read-write, final message from the file", true, "gpt-5.1-codex-max", "max",
			fakeRun{Stream: "codex-success.jsonl", LastMessage: okResult},
			[]string{"--model", "gpt-5.1-codex-max", `model_reasoning_effort="xhigh"`, "--sandbox", "workspace-write"}, okResult},
		{"read-only, final message from the stream, unfenced", false, "", "minimal",
			fakeRun{Stream: "codex-multiturn.jsonl"},
			[]string{"--model", "gpt-5-codex", `model_reasoning_effort="low"`, "--sandbox", "read-only"},
			`{"status":"done","outcome":"decomposed","summary":"five sub-issues"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws, fc := prepared(t, tt.writable, fakePlan{Runs: []fakeRun{tt.run}})
			s := &harnesstest.Recorder{}
			raw, err := a.Run(context.Background(), ws, task(tt.model, tt.effort), "the prompt\n", s)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw.Result) != tt.result {
				t.Errorf("result %q, want %q", raw.Result, tt.result)
			}
			calls := fc.calls()
			if len(calls) != 1 {
				t.Fatalf("%d execs", len(calls))
			}
			c := calls[0]
			for _, w := range append(tt.wantArgs, "exec", "--json", "--cd", ws.Repo, "--output-last-message", `approval_policy="never"`) {
				if !slices.Contains(c.Args, w) {
					t.Errorf("args %q lack %q", c.Args, w)
				}
			}
			checkSchemaArgs(t, c.Args, ws.Schema)
			if c.Args[len(c.Args)-1] != "-" || c.Stdin != "the prompt\n" {
				t.Errorf("the prompt isn't on stdin: args %q, stdin %q", c.Args, c.Stdin)
			}
			home := filepath.Join(ws.Home, ".codex")
			if c.Env["CODEX_HOME"] != home || c.Env["FACTORY_TOKEN"] != "agent-token-never-on-disk" || c.Env["SECRET"] != "" || !sameDir(c.Dir, ws.Repo) {
				t.Errorf("env %v, dir %s", c.Env, c.Dir)
			}
			if !slices.Contains(c.Home, "auth.json -> "+filepath.Join(a.o.Config.Home, "auth.json")) || !slices.Contains(c.Home, "skills/docs-style/SKILL.md") {
				t.Errorf("CODEX_HOME during the run: %q", c.Home)
			}
			lines := strings.Split(strings.TrimSpace(string(raw.Transcript)), "\n")
			if !strings.Contains(lines[0], `"factory.codex-exec"`) || !strings.Contains(string(raw.Transcript), `"thread.started"`) {
				t.Errorf("transcript:\n%s", raw.Transcript)
			}
			if len(s.Events) == 0 || s.Events[0].Kind != harness.KindTurn || len(s.Usages) == 0 || len(s.Limits) != 0 {
				t.Errorf("sink: %+v", s)
			}
			sess, err := readSession(home)
			if err != nil || sess.ThreadID == "" || sess.Sandbox != sandboxFor(ws) {
				t.Errorf("session %+v, %v", sess, err)
			}
		})
	}
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
	for deadline := time.Now().Add(10 * time.Second); len(pids) < 2; {
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
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	for _, pid := range pids {
		gone := false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
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
	if !errors.Is(err, context.Canceled) || len(s.Limits) != 1 || time.Since(start) > 10*time.Second {
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
	if err := writeSession(dir, session{ThreadID: "t", Sandbox: "read-only"}); err != nil {
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

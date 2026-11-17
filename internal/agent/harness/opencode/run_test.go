package opencode

import (
	"context"
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
	"github.com/ContainerSolutions/factory/internal/clock"
)

// okResult is the final message of opencode-success.jsonl, unfenced.
const okResult = `{"status":"done","outcome":"pr-opened","summary":"opened acme/docs#13"}`

// slow bounds what a loaded machine can take for what an idle one does in milliseconds:
// starting the fake (a re-exec of the race-built test binary) or stopping it. The fakes that
// would hang block forever, so a hang still fails.
const slow = time.Minute

// prepared is a prepared workspace (with its checkout) and an adapter on the fake clock
// running a fake opencode with runs.
func prepared(t *testing.T, runs ...fakeRun) (*Adapter, harness.Workspace, *fakeOpencode) {
	t.Helper()
	fo := newFakeOpencode(t, runs...)
	a := New(Options{Binary: fo.Binary, Clock: clock.NewFake(t0), KillGrace: 200 * time.Millisecond, ContextWindow: 131072, MaxOutput: 16384})
	ws := workspace(t, true)
	if err := os.MkdirAll(ws.Repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Prepare(context.Background(), ws, task("qwen3-coder", "high", AuthSelfHosted)); err != nil {
		t.Fatal(err)
	}
	return a, ws, fo
}

// TestRun: run's command line, directory, prompt on stdin and environment; the final text
// unfenced as the Result; the transcript's lines; and the session recorded for Repair.
func TestRun(t *testing.T) {
	t.Parallel()
	a, ws, fo := prepared(t, fakeRun{Stream: "opencode-success.jsonl", Stderr: "INFO done\n"})
	s := &harnesstest.Recorder{}
	raw, err := a.Run(context.Background(), ws, task("qwen3-coder", "high", AuthSelfHosted), "the prompt", s)
	if err != nil || string(raw.Result) != okResult || len(s.Events) != 6 || len(s.Usages) != 3 {
		t.Fatalf("Run = %v, result %q, %d events, %d usages", err, raw.Result, len(s.Events), len(s.Usages))
	}
	c := fo.calls()[0]
	if want := []string{"run", "--format", "json", "--model", "factory/qwen3-coder"}; !slices.Equal(c.Args, want) || c.Stdin != "the prompt" || !sameDir(c.Dir, ws.Repo) {
		t.Errorf("args %q, stdin %q, dir %s", c.Args, c.Stdin, c.Dir)
	}
	cfg := filepath.Join(ws.Home, ".config", "opencode")
	if c.Env["OPENCODE_CONFIG"] != filepath.Join(cfg, "opencode.json") || c.Env["OPENCODE_DISABLE_MODELS_FETCH"] != "1" ||
		!slices.Contains(c.ConfigDir, "skills/lisa-review/SKILL.md") {
		t.Errorf("env %v, config dir %q", c.Env, c.ConfigDir)
	}
	tr := string(raw.Transcript)
	for _, w := range []string{`{"args":["run","--format","json","--model","factory/qwen3-coder"],"type":"factory.opencode-exec"}`, `"type":"step_finish"`,
		`{"text":"INFO done\n","type":"factory.opencode-stderr"}`} {
		if !strings.Contains(tr, w) {
			t.Errorf("the transcript lacks %s:\n%s", w, tr)
		}
	}
	if s, err := readSession(filepath.Join(ws.Home, ownDir)); err != nil || s != (session{SessionID: "ses_1a2b3c4d5effOpenCodeRun01", Model: "qwen3-coder", WorkDir: ws.Repo}) {
		t.Errorf("session %+v, %v", s, err)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		run       fakeRun
		wantErr   string // none: the Raw goes to the pipeline
		wantLimit time.Time
	}{
		{"an error opencode carried on from", fakeRun{Stream: "opencode-overflow-recovered.jsonl", Exit: 1}, "", time.Time{}},
		{"429 in the stream", fakeRun{Stream: "opencode-ratelimit.jsonl", Exit: 1}, "rate limited until 2026-11-17T12:02:00Z: The engine is currently overloaded", t0.Add(DefaultThrottle)},
		{"the gateway's upstream refusal", fakeRun{Stream: "opencode-gateway-upstream.jsonl", Exit: 1}, "rate limited until 2026-11-17T12:05:00Z: Bad Gateway", t0.Add(OutageThrottle)},
		{"the gateway's no-login", fakeRun{Stream: "opencode-failed.jsonl", Exit: 1}, "exit status 1: Service Unavailable: factory gateway: no model login", time.Time{}},
		{"a 503 on stderr only", fakeRun{Stderr: "Error: 503 Service Unavailable\n", Exit: 1}, "rate limited until 2026-11-17T12:02:00Z: Error: 503", t0.Add(DefaultThrottle)},
		{"a failed exit", fakeRun{Stderr: "Error: Session not found\n", Exit: 1}, "exit status 1: Error: Session not found", time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws, _ := prepared(t, tt.run)
			s := &harnesstest.Recorder{}
			raw, err := a.Run(context.Background(), ws, task("qwen3-coder", "", ""), "p", s)
			if tt.wantErr == "" && (err != nil || string(raw.Result) != okResult) || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err %v, result %q, want %q", err, raw.Result, tt.wantErr)
			}
			if tt.wantLimit.IsZero() != (len(s.Limits) == 0) || (len(s.Limits) > 0 && (len(s.Limits) != 1 || !s.Limits[0].Equal(tt.wantLimit))) {
				t.Errorf("rate limits %v, want %v", s.Limits, tt.wantLimit)
			}
		})
	}
}

// TestRepair: the repair resumes the run's session; without one, a new run gets the prompt
// first. Either asks for the Result only. Without a run there is nothing to repair.
func TestRepair(t *testing.T) {
	t.Parallel()
	t.Run("nothing to repair", func(t *testing.T) {
		t.Parallel()
		a, ws, fo := prepared(t)
		if _, err := a.Repair(context.Background(), ws, nil, &harnesstest.Recorder{}); err == nil || len(fo.calls()) != 0 {
			t.Errorf("Repair before Run = %v", err)
		}
	})
	for _, tt := range []struct {
		name, session string
		run           fakeRun
	}{
		{"resumes the session", "ses_2b3c4d5e6fffOpenCodeRun02", fakeRun{Stream: "opencode-multistep.jsonl"}},
		{"a new run without a session", "", fakeRun{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws, fo := prepared(t, tt.run, fakeRun{Stream: "opencode-success.jsonl"})
			if _, err := a.Run(context.Background(), ws, task("qwen3-coder", "", ""), "the prompt", &harnesstest.Recorder{}); err != nil {
				t.Fatal(err)
			}
			raw, err := a.Repair(context.Background(), ws, []string{"/outcome: missing"}, &harnesstest.Recorder{})
			if err != nil || string(raw.Result) != okResult {
				t.Fatalf("Repair = %v, result %q", err, raw.Result)
			}
			c, want := fo.calls()[1], []string{"run", "--format", "json", "--model", "factory/qwen3-coder"}
			if tt.session != "" {
				want = append(want, "--session", tt.session)
			}
			if !slices.Equal(c.Args, want) || !strings.Contains(c.Stdin, "- /outcome: missing") || !strings.Contains(c.Stdin, ws.Result) ||
				strings.HasPrefix(c.Stdin, "the prompt") != (tt.session == "") {
				t.Errorf("repair args %q, prompt %q", c.Args, c.Stdin)
			}
		})
	}
}

// TestRunCancelKillsTheProcessTree: a cancelled run ends promptly although opencode and the
// command it started both ignore SIGTERM, and neither process survives.
func TestRunCancelKillsTheProcessTree(t *testing.T) {
	t.Parallel()
	a, ws, fo := prepared(t, fakeRun{Stream: "opencode-success.jsonl", Stubborn: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, ws, task("qwen3-coder", "", ""), "p", &harnesstest.Recorder{})
		done <- err
	}()
	deadline := time.Now().Add(slow)
	for len(fo.pids()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	pids := fo.pids()
	cancel()
	select {
	case err := <-done:
		if len(pids) != 2 || !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled, after starting %v", err, pids)
		}
	case <-time.After(slow):
		t.Fatal("Run didn't return after cancel")
	}
	for _, pid := range pids {
		for deadline := time.Now().Add(slow); syscall.Kill(pid, 0) == nil && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("process %d survived the cancel", pid)
		}
	}
}

// TestRunRateLimitStopsTheRun: the pipeline cancels the run on RateLimited; opencode, still
// running, is stopped.
func TestRunRateLimitStopsTheRun(t *testing.T) {
	t.Parallel()
	a, ws, _ := prepared(t, fakeRun{Stream: "opencode-ratelimit.jsonl", Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &harnesstest.Recorder{OnLimit: cancel}
	start := time.Now()
	if _, err := a.Run(ctx, ws, task("qwen3-coder", "", ""), "p", s); !errors.Is(err, context.Canceled) || len(s.Limits) != 1 || time.Since(start) > slow {
		t.Errorf("Run = %v after %s, limits %v", err, time.Since(start), s.Limits)
	}
}

// sameDir compares directories through symlinks (macOS's /var is /private/var).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

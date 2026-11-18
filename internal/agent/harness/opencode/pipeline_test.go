package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// TestPipelineEndToEnd runs the pipeline with the opencode adapter on the fake opencode, as a
// VM workspace runs it (gateway mode, from the run bundle): the run writes an invalid Result,
// the repair turn resumes the session and answers a valid one as its final text.
func TestPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	const taskID, ownerKey = "tsk_01JOPENCODE", "sk-spark-owner-never-passed" // the model server's key, in the owner's environment
	e := harnesstest.NewE2E(t, taskID, "Open a PR on acme/docs.\n")
	fo := newFakeOpencode(t,
		fakeRun{Stream: "opencode-multistep.jsonl", Result: `{"status":"done","outcome":"shipped","summary":"x"}`},
		fakeRun{Stream: "opencode-success.jsonl"})
	sink := &harnesstest.Sink{}
	a := New(Options{Binary: fo.Binary, ContextWindow: harnesstest.ModelLimits.ContextWindow, MaxOutput: harnesstest.ModelLimits.MaxOutput}) // the bundle's Model, as vmrun builds it
	h := v1alpha1.HarnessResolved{Name: Name, Version: "1.19.6", Auth: AuthSelfHosted, Model: "qwen3-coder", ReasoningEffort: "high"}
	p, job := e.Pipeline(a, e.Job(h, []string{"pr-opened"}), "FACTORY_OPENCODE_KEY="+ownerKey)
	out, err := p.Run(context.Background(), job, sink)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "pr-opened" {
		t.Fatalf("outcome %+v, result %+v", out, out.Result)
	}
	if h := out.Result.Harness; h == nil || h.Usage.InputTokens != 31500+37840 || h.Usage.OutputTokens != 720+660 || h.Usage.Turns != 6 || h.Usage.CostUSD != nil || h.Auth != AuthSelfHosted {
		t.Errorf("result harness %+v", h)
	}

	calls := fo.calls()
	if len(calls) != 2 {
		t.Fatalf("%d opencode runs, want the run and the repair", len(calls))
	}
	run, repair := calls[0], calls[1]
	root := filepath.Join(e.WorkRoot, taskID)
	if !sameDir(run.Dir, filepath.Join(root, "repo")) || !strings.Contains(run.Stdin, "Open a PR on acme/docs.") || !strings.Contains(run.Stdin, "unattended inside CCF Factory") {
		t.Errorf("run in %s, prompt %q", run.Dir, run.Stdin)
	}
	if !slices.Contains(repair.Args, "ses_2b3c4d5e6fffOpenCodeRun02") || !strings.Contains(repair.Stdin, "/outcome") {
		t.Errorf("repair args %q, prompt %q", repair.Args, repair.Stdin)
	}
	config, err := os.ReadFile(run.Env["OPENCODE_CONFIG"])
	if err != nil || run.Env["GH_TOKEN"] != harness.Placeholder || !slices.Contains(run.ConfigDir, "skills/docs-style/scripts/check.sh") ||
		!strings.Contains(string(config), `"reasoningEffort": "high"`) {
		t.Errorf("run env %v, config dir %q, opencode.json %s (%v)", run.Env, run.ConfigDir, config, err)
	}
	for _, s := range []string{ownerKey, harnesstest.GitToken, harnesstest.AgentToken} {
		for k, v := range run.Env {
			if strings.Contains(v, s) {
				t.Errorf("opencode's $%s holds %q", k, s)
			}
		}
		if strings.Contains(string(config), s) || slices.ContainsFunc(append(run.Args, repair.Args...), func(a string) bool { return strings.Contains(a, s) }) {
			t.Errorf("a key or token is in opencode.json or the command line: %q", s)
		}
	}

	types := e.TranscriptTypes()
	for _, w := range []string{"factory.opencode-exec", "step_start", "tool_use", "step_finish", "text", "factory.repair"} {
		if !slices.Contains(types, w) {
			t.Errorf("the transcript lacks a %s line: %v", w, types)
		}
	}
	if len(sink.Events()) == 0 {
		t.Error("no progress reached the worker")
	}
}

// TestPipelineWallClockStopsDetachedCommands (tsk_aa0555463f611aac56ec: a yarn build ran on in
// the VM past the run's wall clock): at the wall clock the run ends failed{limit} with its
// transcript, within the runner's margin, although opencode ignores SIGTERM and a command it
// started in a session of its own ignores it too and holds opencode's output open. On Linux, as
// in the VM, the command is killed with opencode.
func TestPipelineWallClockStopsDetachedCommands(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, "tsk_01JOPENCODEWALL", "Build the site.\n")
	clk := clock.NewFake(time.Now())
	e.Clock = clk
	fo := newFakeOpencode(t, fakeRun{Stream: "opencode-multistep.jsonl", Detached: true})
	t.Cleanup(func() {
		if pids := fo.pids(); len(pids) == 2 {
			_ = syscall.Kill(pids[1], syscall.SIGKILL) // the command; clirun reaped opencode
		}
	})
	a := New(Options{Binary: fo.Binary, Clock: clk, KillGrace: 200 * time.Millisecond,
		ContextWindow: harnesstest.ModelLimits.ContextWindow, MaxOutput: harnesstest.ModelLimits.MaxOutput})
	job := e.Job(v1alpha1.HarnessResolved{Name: Name, Version: "1.19.6", Auth: AuthSelfHosted, Model: "qwen3-coder"}, []string{"pr-opened"})
	job.StartedAt = clk.Now()
	p, job := e.Pipeline(a, job)
	type ended struct {
		out runner.Outcome
		err error
	}
	done := make(chan ended, 1)
	go func() {
		out, err := p.Run(context.Background(), job, &harnesstest.Sink{})
		done <- ended{out, err}
	}()
	for deadline := time.Now().Add(slow); len(fo.pids()) < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fake opencode never started its command")
		}
	}
	pids := fo.pids()
	clk.Advance(10 * time.Minute) // the task's maxWallClock
	var r ended
	select {
	case r = <-done:
	case <-time.After(slow): // a minute, the runner's DefaultMargin; an idle machine takes well under a second
		t.Fatal("the run didn't end at its wall clock")
	}
	if r.err != nil || r.out.Status != runner.StatusFailed || r.out.Reason != runner.ReasonLimit || r.out.TranscriptRef == "" {
		t.Fatalf("outcome %+v, err %v: want failed{limit} with a transcript", r.out, r.err)
	}
	if types := e.TranscriptTypes(); !slices.Contains(types, "factory.opencode-exec") || !slices.Contains(types, "tool_use") {
		t.Errorf("transcript types %v", types)
	}
	if runtime.GOOS != "linux" {
		return // no /proc: only opencode's own group is signalled
	}
	for _, pid := range pids {
		for deadline := time.Now().Add(slow); syscall.Kill(pid, 0) == nil && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("process %d survived the wall clock", pid)
		}
	}
}

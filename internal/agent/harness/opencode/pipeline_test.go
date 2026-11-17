package opencode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
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

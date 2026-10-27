package claudecode

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// e2eTaskID is the end-to-end runs' task.
const e2eTaskID = "tsk_01JCLAUDE"

// claudeHarness is the resolved harness of an end-to-end run.
func claudeHarness(model, effort, auth string) v1alpha1.HarnessResolved {
	return v1alpha1.HarnessResolved{Name: Name, Version: "2.1.3", Auth: auth, Model: model, ReasoningEffort: effort}
}

// e2eAdapter is the claude-code adapter as vmrun builds it, running binary.
func e2eAdapter(binary string) *Adapter {
	return New(Options{Binary: binary, Getuid: func() int { return 1000 },
		Clock: clock.NewFake(t0), KillGrace: 200 * time.Millisecond})
}

// TestPipelineEndToEnd runs the pipeline with the claude-code adapter on the fake claude, as
// a VM workspace runs it (gateway mode, from the run bundle): the run's hook allows an edit
// in the checkout and refuses a push, the run writes an invalid Result, and the repair turn
// resumes the session and answers a valid one as its structured output.
func TestPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR on acme/docs.\n")
	ws := filepath.Join(e.WorkRoot, e2eTaskID)
	fc := newFakeClaude(t, fakePlan{Runs: []fakeRun{
		{Stream: "claude-success.jsonl", Result: `{"status":"done","outcome":"shipped","summary":"x"}`, ToolCalls: []json.RawMessage{
			toolCall("Edit", map[string]any{"file_path": "README.md"}),
			toolCall("Bash", map[string]any{"command": "git push origin HEAD:main"}),
		}},
		{Stream: "claude-success.jsonl"},
	}})
	sink := &harnesstest.Sink{}
	p, job := e.Pipeline(e2eAdapter(fc.Binary), e.Job(claudeHarness("claude-sonnet-5-5", "max", AuthSubscription), []string{"pr-opened"}),
		"ANTHROPIC_API_KEY=sk-ant-api03-worker-key")
	out, err := p.Run(context.Background(), job, sink)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "pr-opened" {
		t.Fatalf("outcome %+v, result %+v", out, out.Result)
	}
	h := out.Result.Harness
	if h == nil || h.Usage.InputTokens != 2*62215 || h.Usage.OutputTokens != 2*510 || h.Usage.Turns != 10 || h.Usage.CostUSD != nil {
		t.Errorf("result harness %+v", h)
	}

	calls := fc.calls()
	if len(calls) != 2 {
		t.Fatalf("%d claude runs, want the run and the repair", len(calls))
	}
	run, repair := calls[0], calls[1]
	for _, w := range []string{"--model", "claude-sonnet-5-5", "--max-turns", "20", filepath.Join(ws, "out")} {
		if !slices.Contains(run.Args, w) {
			t.Errorf("run args %q lack %q", run.Args, w)
		}
	}
	for _, plugin := range []string{"ccf-docs", "ccf-review", "lisa"} {
		if !slices.ContainsFunc(run.Args, func(a string) bool { return filepath.Base(a) == plugin && strings.Contains(a, "skills") }) {
			t.Errorf("run args %q lack the %s plugin dir", run.Args, plugin)
		}
	}
	if slices.Contains(run.Args, "--disallowedTools") || !sameDir(run.Dir, filepath.Join(ws, "repo")) {
		t.Errorf("a writable checkout: args %q, dir %s", run.Args, run.Dir)
	}
	if !strings.Contains(run.Stdin, "Open a PR on acme/docs.") || !strings.Contains(run.flag("--append-system-prompt"), "unattended inside CCF Factory (task "+e2eTaskID) {
		t.Errorf("run prompt %q", run.Stdin)
	}
	if js := run.flag("--json-schema"); !strings.Contains(js, `"title":"Result"`) || !strings.Contains(js, `"#/definitions/Artifact"`) ||
		strings.Contains(js, `"$schema"`) || strings.Contains(js, `"$id"`) || strings.Contains(js, `$defs`) {
		t.Errorf("the Result schema isn't passed as draft-07: %q", js)
	}
	var codes []int
	for _, h := range run.Hooks {
		codes = append(codes, h.Exit)
	}
	if !slices.Equal(codes, []int{0, hookExitBlock}) || !strings.Contains(run.Hooks[1].Stderr, "factory push") {
		t.Errorf("hook answers %+v: the edit is allowed, the push refused", run.Hooks)
	}
	if repair.flag("--resume") != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" || !strings.Contains(repair.Stdin, "/outcome") {
		t.Errorf("repair args %q, prompt %q", repair.Args, repair.Stdin)
	}
	if run.Env["CLAUDE_CONFIG_DIR"] != filepath.Join(ws, "home", ".claude") || run.Env["ANTHROPIC_API_KEY"] != "" ||
		run.Env["CLAUDE_CODE_OAUTH_TOKEN"] != harness.Placeholder || run.Env["FACTORY_RESULT"] == "" ||
		run.Env["GH_TOKEN"] != harness.Placeholder || run.Env["HOME"] != filepath.Join(ws, "home") {
		t.Errorf("run env %v", run.Env)
	}
	if strings.Contains(run.Settings, `"sandbox"`) || strings.Contains(run.Settings, `"permissions"`) {
		t.Errorf("the VM run's settings have a sandbox:\n%s", run.Settings)
	}
	if !strings.Contains(run.Settings, `"effortLevel": "high"`) || !strings.Contains(run.MCP, `"FACTORY_RESULT": "${FACTORY_RESULT}"`) {
		t.Errorf("settings %s\nmcp %s", run.Settings, run.MCP)
	}
	for _, s := range append(testSecrets, harnesstest.GitToken, harnesstest.AgentToken) {
		if strings.Contains(run.Settings, s) || strings.Contains(run.MCP, s) || slices.ContainsFunc(run.Args, func(a string) bool { return strings.Contains(a, s) }) {
			t.Errorf("a token is in the settings, the MCP config or the command line: %q", s)
		}
	}

	types := e.TranscriptTypes()
	for _, w := range []string{"factory.claude-exec", "system", "assistant", "result", "factory.repair"} {
		if !slices.Contains(types, w) {
			t.Errorf("the transcript lacks a %s line: %v", w, types)
		}
	}
	if len(sink.Events()) == 0 {
		t.Error("no progress reached the worker")
	}
}

// TestPipelineStructuredOutputRefused: claude ran out of StructuredOutput retries and exited
// 1 (a docs-review run's model sent `"data": "{}"`, a string, five times). The run isn't a
// harness error: the last attempt, its stringified fields decoded, is the Result candidate, and
// the pipeline validates it and repairs it as any other.
func TestPipelineStructuredOutputRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		runs     []fakeRun
		outcome  string
		claudes  int // the run, and the repair when there was one
		repaired bool
	}{
		{"the reported run: data decoded, valid", []fakeRun{{Stream: "claude-structured-retries.jsonl", Exit: 1}},
			"changes-requested", 1, false},
		{"data that isn't JSON: the repair turn", []fakeRun{{Stream: "claude-structured-notjson.jsonl", Exit: 1}, {Stream: "claude-success.jsonl"}},
			"pr-opened", 2, true},
		{"the repair refused too, its data decoded", []fakeRun{{Stream: "claude-structured-notjson.jsonl", Exit: 1}, {Stream: "claude-structured-retries.jsonl", Exit: 1}},
			"changes-requested", 2, true},
		{"a structured output claude accepted, unchanged", []fakeRun{{Stream: "claude-success.jsonl"}},
			"pr-opened", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := harnesstest.NewE2E(t, e2eTaskID, "Review acme/docs#21.\n")
			fc := newFakeClaude(t, fakePlan{Runs: tt.runs})
			p, job := e.Pipeline(e2eAdapter(fc.Binary), e.Job(claudeHarness("claude-sonnet-5-5", "low", AuthSubscription), []string{"pr-opened", "changes-requested"}))
			out, err := p.Run(context.Background(), job, &harnesstest.Sink{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != tt.outcome {
				t.Fatalf("outcome %+v, result %+v", out, out.Result)
			}
			if n := len(fc.calls()); n != tt.claudes {
				t.Errorf("%d claude runs, want %d", n, tt.claudes)
			}
			if got := slices.Contains(e.TranscriptTypes(), "factory.repair"); got != tt.repaired {
				t.Errorf("repaired %v, want %v", got, tt.repaired)
			}
		})
	}
}

// TestPipelineRateLimited: a rejected rate limit in claude's stream ends the run throttled
// until the limit's reset.
func TestPipelineRateLimited(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR.\n")
	fc := newFakeClaude(t, fakePlan{Runs: []fakeRun{{Stream: "claude-ratelimit-event.jsonl", Block: true}}})
	p, job := e.Pipeline(e2eAdapter(fc.Binary), e.Job(claudeHarness("claude-sonnet-5-5", "low", AuthSubscription), []string{"pr-opened"}))
	out, err := p.Run(context.Background(), job, &harnesstest.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusThrottled || out.ThrottledUntil == nil || !out.ThrottledUntil.Equal(t0.Add(90*time.Minute)) || out.Result != nil {
		t.Errorf("outcome %+v", out)
	}
}

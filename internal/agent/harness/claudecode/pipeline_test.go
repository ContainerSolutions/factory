package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// e2eTaskID is the end-to-end runs' task.
const e2eTaskID = "tsk_01JCLAUDE"

// claudeHarness is the resolved harness of an end-to-end run.
func claudeHarness(model, effort, auth string) v1alpha1.HarnessResolved {
	return v1alpha1.HarnessResolved{Name: Name, Version: "2.1.3", Auth: auth, Model: model, ReasoningEffort: effort}
}

// TestPipelineEndToEnd runs the pipeline with the claude-code adapter on the fake claude:
// the run's hook allows an edit in the checkout and refuses a push, the run writes an invalid
// Result, and the repair turn resumes the session and answers a valid one as its structured
// output.
func TestPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR on acme/docs.\n")
	ws := filepath.Join(e.WorkRoot, e2eTaskID)
	owner := ownerConfigDir(t)
	before := listDir(t, owner)
	fc := newFakeClaude(t, fakePlan{Runs: []fakeRun{
		{Stream: "claude-success.jsonl", Result: `{"status":"done","outcome":"shipped","summary":"x"}`, ToolCalls: []json.RawMessage{
			toolCall("Edit", map[string]any{"file_path": "README.md"}),
			toolCall("Bash", map[string]any{"command": "git push origin HEAD:main"}),
			toolCall("Write", map[string]any{"file_path": filepath.Join(ws, "home", ".factory-claude", "settings.json")}),
		}},
		{Stream: "claude-success.jsonl"},
	}})
	a := New(Options{LookPath: sandboxed, Config: config.Harness{ConfigDir: owner}, Binary: fc.Binary, GOOS: "linux",
		Environ: workerEnv("ANTHROPIC_API_KEY=sk-ant-api03-worker-key")})
	sink := &harnesstest.Sink{}
	out, err := e.Pipeline(a, "ANTHROPIC_API_KEY=sk-ant-api03-worker-key").Run(context.Background(),
		e.Job(claudeHarness("claude-sonnet-5-5", "max", AuthSubscription), []string{"pr-opened"}), sink)
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
	for _, w := range []string{"--model", "claude-sonnet-5-5", "--max-turns", "20", filepath.Join(ws, "skills", "ccf-docs"),
		filepath.Join(ws, "skills", "ccf-review"), filepath.Join(ws, "skills", "lisa"), filepath.Join(ws, "out")} {
		if !slices.Contains(run.Args, w) {
			t.Errorf("run args %q lack %q", run.Args, w)
		}
	}
	// The workspace is gone: compare the run's directory by its tail (macOS's /var is /private/var).
	if slices.Contains(run.Args, "--disallowedTools") || !strings.HasSuffix(run.Dir, filepath.Join(e2eTaskID, "repo")) {
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
	if !slices.Equal(codes, []int{0, hookExitBlock, hookExitBlock}) || !strings.Contains(run.Hooks[1].Stderr, "factory push") {
		t.Errorf("hook answers %+v: the edit is allowed, the push and the settings write refused", run.Hooks)
	}
	if repair.flag("--resume") != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" || !strings.Contains(repair.Stdin, "/outcome") {
		t.Errorf("repair args %q, prompt %q", repair.Args, repair.Stdin)
	}
	if run.Env["CLAUDE_CONFIG_DIR"] != owner || run.Env["ANTHROPIC_API_KEY"] != "" || run.Env["FACTORY_RESULT"] == "" ||
		run.Env["GH_TOKEN"] != harnesstest.GitToken || run.Env["HOME"] != filepath.Join(ws, "home") {
		t.Errorf("run env %v", run.Env)
	}
	var sd settingsDoc
	if err := json.Unmarshal([]byte(run.Settings), &sd); err != nil || sd.Sandbox.Enabled == nil || !*sd.Sandbox.Enabled ||
		!slices.Contains(sd.Permissions.Allow, "Edit(/"+filepath.Join(ws, "repo")+"/**)") || !slices.Contains(sd.Permissions.Allow, "WebFetch(domain:127.0.0.1)") ||
		len(sd.Sandbox.Network.AllowUnixSockets) != 1 || run.Env["FACTORY_GIT_CREDENTIAL_SOCKET"] != sd.Sandbox.Network.AllowUnixSockets[0] {
		t.Errorf("the run isn't sandboxed for its checkout, hosts and credential helper (%v):\n%s", err, run.Settings)
	}
	if !strings.Contains(run.Settings, `"effortLevel": "high"`) || !strings.Contains(run.MCP, `"FACTORY_TOKEN": "${FACTORY_TOKEN}"`) {
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
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("the workspace is still there: %v", err)
	}
	if got := listDir(t, owner); !slices.Equal(got, before) {
		t.Errorf("the config dir changed: %q", got)
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
			a := New(Options{LookPath: sandboxed, Config: config.Harness{ConfigDir: ownerConfigDir(t)}, Binary: fc.Binary, GOOS: "linux", Environ: workerEnv()})
			out, err := e.Pipeline(a).Run(context.Background(),
				e.Job(claudeHarness("claude-sonnet-5-5", "low", AuthSubscription), []string{"pr-opened", "changes-requested"}), &harnesstest.Sink{})
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
	a := New(Options{LookPath: sandboxed, Config: config.Harness{ConfigDir: ownerConfigDir(t)}, Binary: fc.Binary, GOOS: "linux", Environ: workerEnv(),
		Clock: clock.NewFake(t0), KillGrace: 200 * time.Millisecond})
	out, err := e.Pipeline(a).Run(context.Background(), e.Job(claudeHarness("claude-sonnet-5-5", "low", AuthSubscription), []string{"pr-opened"}), &harnesstest.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusThrottled || out.ThrottledUntil == nil || !out.ThrottledUntil.Equal(t0.Add(90*time.Minute)) || out.Result != nil {
		t.Errorf("outcome %+v", out)
	}
}

// TestSmokeClaude is `make smoke-claude`: a trivial task end to end through the real claude,
// on a maintainer machine signed in to a dedicated CLAUDE_CONFIG_DIR (or with
// ANTHROPIC_API_KEY set). It never runs in CI or in `make test`: it needs
// FACTORY_SMOKE_CLAUDE=1.
//
//	FACTORY_SMOKE_CLAUDE_CONFIG_DIR the dedicated CLAUDE_CONFIG_DIR (default ~/.factory-agent/claude)
//	FACTORY_SMOKE_CLAUDE_MODEL      the model (default claude-sonnet-5-5)
//	FACTORY_SMOKE_CLAUDE_EFFORT     the normalised effort (default low)
//	FACTORY_SMOKE_CLAUDE_BIN        the claude binary (default claude on PATH)
func TestSmokeClaude(t *testing.T) {
	if os.Getenv("FACTORY_SMOKE_CLAUDE") != "1" || os.Getenv("CI") != "" {
		t.Skip("the smoke run needs a signed-in claude on a maintainer machine: make smoke-claude")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dir, err := config.ExpandPath(env("FACTORY_SMOKE_CLAUDE_CONFIG_DIR", "~/.factory-agent/claude"), os.UserHomeDir)
	if err != nil {
		t.Fatal(err)
	}
	model := env("FACTORY_SMOKE_CLAUDE_MODEL", "claude-sonnet-5-5")
	effort := env("FACTORY_SMOKE_CLAUDE_EFFORT", "low")
	cfg := config.Harness{ConfigDir: dir, Models: []string{model}, Default: model}
	a := New(Options{Config: cfg, Binary: env("FACTORY_SMOKE_CLAUDE_BIN", "claude")})
	off, err := a.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("probe: %+v", off)

	const want = `{"status": "done", "outcome": "smoke", "summary": "claude smoke test"}`
	e := harnesstest.NewE2E(t, e2eTaskID, "This is a smoke test of CCF Factory's Claude Code adapter. Don't change any file. "+
		"Run `ls` in the repository. Then run `git push --dry-run origin HEAD` once: a hook is expected to refuse it, so don't retry it or work around it. "+
		"Then write exactly this JSON to the file named by $FACTORY_RESULT: "+want+" and give the same JSON object as your final output.\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sink := &harnesstest.Sink{}
	out, err := e.Pipeline(a).Run(ctx, e.Job(claudeHarness(model, effort, off.Auth), []string{"smoke"}), sink)
	lines := e.Transcript()
	pushes, refused := 0, 0
	for _, l := range lines {
		switch l["type"] {
		case "factory.claude-exec", "factory.claude-stderr", "system", "result", "rate_limit_event":
			t.Logf("%v", l)
		}
		b, _ := json.Marshal(l)
		if strings.Contains(string(b), "git push --dry-run") && l["type"] == "assistant" {
			pushes++
		}
		if strings.Contains(string(b), "factory push") && l["type"] == "user" {
			refused++
		}
	}
	for _, p := range sink.Events() {
		t.Logf("progress %s: %s", p.Kind, p.Message)
	}
	switch {
	case pushes == 0:
		t.Log("WARNING: the model never tried the push, so the PreToolUse hook is unverified")
	case refused == 0:
		t.Errorf("the push was tried %d times and the hook never refused it", pushes)
	default:
		t.Logf("the PreToolUse hook refused the push (%d tries)", pushes)
	}
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "smoke" {
		for _, l := range lines {
			t.Logf("transcript: %v", l)
		}
		t.Fatalf("outcome %+v, result %+v", out, out.Result)
	}
	if h := out.Result.Harness; h == nil || h.Usage.Turns < 1 || h.Usage.InputTokens == 0 {
		t.Errorf("usage %+v", h)
	}
	t.Logf("result: %+v, usage %+v", out.Result, out.Result.Harness.Usage)
}

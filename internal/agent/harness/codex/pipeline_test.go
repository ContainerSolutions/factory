package codex

import (
	"context"
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
)

// e2eTaskID is the end-to-end runs' task.
const e2eTaskID = "tsk_01JCODEX"

// codexHarness is the resolved harness of an end-to-end run.
func codexHarness(model, effort string) v1alpha1.HarnessResolved {
	return v1alpha1.HarnessResolved{Name: Name, Version: "0.80.0", Auth: AuthSubscription, Model: model, ReasoningEffort: effort}
}

// TestPipelineEndToEnd runs the pipeline with the codex adapter on the fake codex: the run
// writes an invalid Result, the repair turn resumes the session and answers a valid one as
// its final message.
func TestPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR on acme/docs.\n")
	owner := ownerHome(t)
	fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{
		{Stream: "codex-success.jsonl", Result: `{"status":"done","outcome":"shipped","summary":"x"}`},
		{Stream: "codex-success.jsonl", LastMessage: okResult},
	}})
	a := New(Options{Config: config.Harness{Home: owner}, Binary: fc.Binary})
	sink := &harnesstest.Sink{}
	out, err := e.Pipeline(a, "OPENAI_API_KEY=sk-owner-never-passed").Run(context.Background(), e.Job(codexHarness("gpt-5-codex", "high"), []string{"pr-opened"}), sink)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "pr-opened" {
		t.Fatalf("outcome %+v, result %+v", out, out.Result)
	}
	h := out.Result.Harness
	if h == nil || h.Usage.InputTokens != 2*24763 || h.Usage.OutputTokens != 2*1220 || h.Usage.Turns != 2 || h.Usage.CostUSD != nil {
		t.Errorf("result harness %+v", h)
	}

	calls := fc.calls()
	if len(calls) != 2 {
		t.Fatalf("%d codex execs, want the run and the repair", len(calls))
	}
	run, repair := calls[0], calls[1]
	repo := filepath.Join(e.WorkRoot, e2eTaskID, "repo")
	for _, w := range []string{"--sandbox", "workspace-write", "--cd", repo, "--model", "gpt-5-codex", `model_reasoning_effort="high"`,
		"--output-schema", filepath.Join(e.WorkRoot, e2eTaskID, "result.schema.json")} {
		if !slices.Contains(run.Args, w) {
			t.Errorf("run args %q lack %q", run.Args, w)
		}
	}
	if !strings.Contains(run.Stdin, "Open a PR on acme/docs.") || !strings.Contains(run.Stdin, "unattended inside CCF Factory") {
		t.Errorf("run prompt %q", run.Stdin)
	}
	if i := slices.Index(repair.Args, "resume"); i < 0 || repair.Args[i+1] != "0199f2a1-6b1e-7c33-9a51-2f0c4e8d1a01" ||
		!strings.Contains(repair.Stdin, "/outcome") {
		t.Errorf("repair args %q, prompt %q", repair.Args, repair.Stdin)
	}
	home := filepath.Join(e.WorkRoot, e2eTaskID, "home", ".codex")
	if run.Env["CODEX_HOME"] != home || run.Env["OPENAI_API_KEY"] != "" || run.Env["FACTORY_RESULT"] == "" || run.Env["GH_TOKEN"] != harnesstest.GitToken {
		t.Errorf("run env %v", run.Env)
	}
	for _, w := range []string{"auth.json -> " + filepath.Join(owner, "auth.json"), "skills/docs-style/SKILL.md",
		"skills/docs-style/scripts/check.sh", "skills/ccf-review-review/SKILL.md", "skills/lisa-review/SKILL.md", "config.toml", "AGENTS.md"} {
		if !slices.Contains(run.Home, w) {
			t.Errorf("CODEX_HOME during the run lacks %q: %q", w, run.Home)
		}
	}
	if !strings.Contains(run.Agents, "unattended inside CCF Factory (task "+e2eTaskID) || !strings.Contains(run.Config, `"FACTORY_TOKEN"`) {
		t.Errorf("AGENTS.md %q\nconfig.toml %q", run.Agents, run.Config)
	}
	for _, s := range append(testSecrets, harnesstest.GitToken, harnesstest.AgentToken) {
		if strings.Contains(run.Config, s) || strings.Contains(run.Agents, s) || slices.ContainsFunc(run.Args, func(a string) bool { return strings.Contains(a, s) }) {
			t.Errorf("a token is in the config, AGENTS.md or the command line: %q", s)
		}
	}

	types := e.TranscriptTypes()
	for _, w := range []string{"factory.codex-exec", "thread.started", "turn.completed", "factory.repair"} {
		if !slices.Contains(types, w) {
			t.Errorf("the transcript lacks a %s line: %v", w, types)
		}
	}
	if len(sink.Events()) == 0 {
		t.Error("no progress reached the worker")
	}
	if _, err := os.Stat(filepath.Join(e.WorkRoot, e2eTaskID)); !os.IsNotExist(err) {
		t.Errorf("the workspace is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(owner, "auth.json")); string(b) != ownerLogin {
		t.Errorf("the owner's login changed or went with the workspace: %q", b)
	}
}

// TestPipelineRateLimited: a usage limit in codex's stream ends the run throttled until the
// limit's reset.
func TestPipelineRateLimited(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR.\n")
	fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{{Stream: "codex-ratelimit.jsonl", Block: true}}})
	before := time.Now()
	a := New(Options{Config: config.Harness{Home: ownerHome(t)}, Binary: fc.Binary, KillGrace: 200 * time.Millisecond})
	out, err := e.Pipeline(a).Run(context.Background(), e.Job(codexHarness("gpt-5-codex", "low"), []string{"pr-opened"}), &harnesstest.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusThrottled || out.ThrottledUntil == nil || out.ThrottledUntil.Before(before.Add(2*time.Hour)) || out.Result != nil {
		t.Errorf("outcome %+v", out)
	}
}

// TestSmokeCodex is `make smoke-codex`: a trivial task end to end through the real codex,
// on a maintainer machine signed in to a dedicated CODEX_HOME. It never runs in CI or in
// `make test`: it needs FACTORY_SMOKE_CODEX=1.
//
//	FACTORY_SMOKE_CODEX_HOME   the dedicated CODEX_HOME (default ~/.factory-agent/codex-home)
//	FACTORY_SMOKE_CODEX_MODEL  the model (default gpt-5-codex)
//	FACTORY_SMOKE_CODEX_EFFORT the normalised effort (default low)
//	FACTORY_SMOKE_CODEX_BIN    the codex binary (default codex on PATH)
func TestSmokeCodex(t *testing.T) {
	if os.Getenv("FACTORY_SMOKE_CODEX") != "1" || os.Getenv("CI") != "" {
		t.Skip("the smoke run needs a signed-in codex on a maintainer machine: make smoke-codex")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	home, err := config.ExpandPath(env("FACTORY_SMOKE_CODEX_HOME", "~/.factory-agent/codex-home"), os.UserHomeDir)
	if err != nil {
		t.Fatal(err)
	}
	model := env("FACTORY_SMOKE_CODEX_MODEL", "gpt-5-codex")
	effort := env("FACTORY_SMOKE_CODEX_EFFORT", "low")
	a := New(Options{Config: config.Harness{Home: home, Default: model}, Binary: env("FACTORY_SMOKE_CODEX_BIN", "codex")})
	off, err := a.Probe(context.Background(), config.Harness{Home: home, Models: []string{model}, Default: model})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("probe: %+v", off)

	const want = `{"status": "done", "outcome": "smoke", "summary": "codex smoke test"}`
	e := harnesstest.NewE2E(t, e2eTaskID, "This is a smoke test of CCF Factory's Codex adapter. Don't change any file. "+
		"Run `ls` in the repository, then write exactly this JSON to the file named by $FACTORY_RESULT: "+want+
		" and reply with the same JSON object, and nothing else, as your final message.\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sink := &harnesstest.Sink{}
	out, err := e.Pipeline(a).Run(ctx, e.Job(codexHarness(model, effort), []string{"smoke"}), sink)
	lines := e.Transcript()
	for _, l := range lines {
		if l["type"] == "factory.codex-exec" || l["type"] == "factory.codex-stderr" {
			t.Logf("%v", l)
		}
	}
	for _, p := range sink.Events() {
		t.Logf("progress %s: %s", p.Kind, p.Message)
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

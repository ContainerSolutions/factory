package codex

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
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

// TestPipelineEndToEnd runs the pipeline with the codex adapter on the fake codex, as a VM
// workspace runs it (gateway mode, from the run bundle): the run writes an invalid Result,
// the repair turn resumes the session and answers a valid one as its final message.
func TestPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR on acme/docs.\n")
	fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{
		{Stream: "codex-success.jsonl", Result: `{"status":"done","outcome":"shipped","summary":"x"}`},
		{Stream: "codex-success.jsonl", LastMessage: okResult},
	}})
	acct := harnesstest.CodexAccount
	a := New(Options{Account: &acct, Binary: fc.Binary})
	sink := &harnesstest.Sink{}
	p, job := e.Pipeline(a, e.Job(codexHarness("gpt-5-codex", "high"), []string{"pr-opened"}), "OPENAI_API_KEY=sk-owner-never-passed")
	out, err := p.Run(context.Background(), job, sink)
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
	root := filepath.Join(e.WorkRoot, e2eTaskID)
	repo := filepath.Join(root, "repo")
	checkSchemaArgs(t, run.Args, filepath.Join(root, "result.schema.json"))
	for _, w := range []string{"--sandbox", "danger-full-access", "--cd", repo, "--model", "gpt-5-codex", `model_reasoning_effort="high"`} {
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
	home := filepath.Join(root, "home", ".codex")
	if run.Env["CODEX_HOME"] != home || run.Env["OPENAI_API_KEY"] != "" || run.Env["FACTORY_RESULT"] == "" || run.Env["GH_TOKEN"] != harness.Placeholder {
		t.Errorf("run env %v", run.Env)
	}
	for _, w := range []string{"auth.json", "skills/docs-style/SKILL.md",
		"skills/docs-style/scripts/check.sh", "skills/ccf-review-review/SKILL.md", "skills/lisa-review/SKILL.md", "config.toml", "AGENTS.md"} {
		if !slices.Contains(run.Home, w) {
			t.Errorf("CODEX_HOME during the run lacks %q: %q", w, run.Home)
		}
	}
	if !strings.Contains(run.Auth, harness.Placeholder) || !strings.Contains(run.Auth, harnesstest.CodexAccount.AccountID) {
		t.Errorf("auth.json during the run %q: want the placeholder login", run.Auth)
	}
	if !strings.Contains(run.Agents, "unattended inside CCF Factory (task "+e2eTaskID) || !strings.Contains(run.Config, `"FACTORY_RESULT"`) {
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
}

// TestPipelineRateLimited: a usage limit in codex's stream ends the run throttled until the
// limit's reset.
func TestPipelineRateLimited(t *testing.T) {
	t.Parallel()
	e := harnesstest.NewE2E(t, e2eTaskID, "Open a PR.\n")
	fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{{Stream: "codex-ratelimit.jsonl", Block: true}}})
	before := time.Now()
	acct := harnesstest.CodexAccount
	a := New(Options{Account: &acct, Binary: fc.Binary, KillGrace: 200 * time.Millisecond})
	p, job := e.Pipeline(a, e.Job(codexHarness("gpt-5-codex", "low"), []string{"pr-opened"}))
	out, err := p.Run(context.Background(), job, &harnesstest.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != runner.StatusThrottled || out.ThrottledUntil == nil || out.ThrottledUntil.Before(before.Add(2*time.Hour)) || out.Result != nil {
		t.Errorf("outcome %+v", out)
	}
}

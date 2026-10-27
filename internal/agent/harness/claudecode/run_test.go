package claudecode

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

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
)

const okResult = `{"status":"done","outcome":"pr-opened","summary":"opened acme/docs#13","data":{"prs":["acme/docs#13"]}}`

// slow bounds what a loaded machine can take for what an idle one does in milliseconds:
// starting the fake (a re-exec of the race-built test binary, which macOS also scans on its first
// runs) or stopping it. The fakes that would hang block forever, so a hang still fails.
const slow = time.Minute

// prepared is a prepared VM workspace and an adapter running the fake claude.
func prepared(t *testing.T, writable bool, plan fakePlan) (*Adapter, harness.Workspace, *fakeClaude) {
	t.Helper()
	fc := newFakeClaude(t, plan)
	a := vmAdapter(t, 1000, fc.Binary)
	ws := vmWorkspace(t, writable)
	if err := a.Prepare(context.Background(), ws, task("", "high", "")); err != nil {
		t.Fatal(err)
	}
	return a, ws, fc
}

func toolCall(tool string, input map[string]any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": input})
	return b
}

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		writable  bool
		model     string
		auth      string
		wantArgs  []string
		lackArgs  []string
		wantEnv   map[string]string
		wantHooks []int // exit codes for: write out/, write repo/, git push
		wantCost  bool
	}{
		{"read-write on a subscription", true, "claude-opus-5-5", AuthSubscription,
			[]string{"--model", "claude-opus-5-5", "--max-turns", "30"},
			[]string{"--disallowedTools", "--max-budget-usd"},
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": harness.Placeholder},
			[]int{0, 0, hookExitBlock}, false},
		{"read-only on an API key", false, "", AuthAPIKey,
			[]string{"--model", "claude-sonnet-5-5", "--disallowedTools", "Write,Edit,MultiEdit,NotebookEdit", "--max-budget-usd", "2.5"},
			nil,
			map[string]string{"ANTHROPIC_API_KEY": harness.Placeholder},
			[]int{0, hookExitBlock, hookExitBlock}, true},
		{"no resolved auth: the subscription", true, "", "",
			nil, []string{"--max-budget-usd"},
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": harness.Placeholder},
			[]int{0, 0, hookExitBlock}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws, _ := prepared(t, tt.writable, fakePlan{})
			// The tool calls put to the hook: the Result in out/, an edit in the checkout, a push.
			fc := newFakeClaude(t, fakePlan{Runs: []fakeRun{{Stream: "claude-success.jsonl", ToolCalls: []json.RawMessage{
				toolCall("Write", map[string]any{"file_path": ws.Result, "content": okResult}),
				toolCall("Edit", map[string]any{"file_path": "docs/a.md"}),
				toolCall("Bash", map[string]any{"command": "git push origin HEAD"}),
			}}}})
			a.o.Binary = fc.Binary
			s := &harnesstest.Recorder{}
			raw, err := a.Run(context.Background(), ws, task(tt.model, "high", tt.auth), "the prompt\n", s)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw.Result) != okResult {
				t.Errorf("result %q, want the structured output", raw.Result)
			}
			calls := fc.calls()
			if len(calls) != 1 {
				t.Fatalf("%d runs", len(calls))
			}
			c := calls[0]
			dir := filepath.Join(ws.Home, ".factory-claude")
			for _, w := range append(tt.wantArgs, "--print", "--output-format", "stream-json", "--verbose", "--append-system-prompt", ws.Preamble,
				"--settings", filepath.Join(dir, "settings.json"), "--setting-sources", "user", "--mcp-config", filepath.Join(dir, "mcp.json"),
				"--strict-mcp-config", "--json-schema", `{"required":["status"],"type":"object"}`, "--permission-mode", "bypassPermissions",
				filepath.Join(ws.Skills, "ccf-docs"), filepath.Join(ws.Skills, "ccf-review"), filepath.Join(ws.Skills, "lisa"), ws.Out, ws.Mounts[1].Dir) {
				if !slices.Contains(c.Args, w) {
					t.Errorf("args %q lack %q", c.Args, w)
				}
			}
			for _, w := range tt.lackArgs {
				if slices.Contains(c.Args, w) {
					t.Errorf("args %q have %q", c.Args, w)
				}
			}
			if slices.Contains(c.Args, "--resume") || c.Stdin != "the prompt\n" || !sameDir(c.Dir, ws.Repo) {
				t.Errorf("args %q, stdin %q, dir %s", c.Args, c.Stdin, c.Dir)
			}
			env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(ws.Home, ".claude"), "DISABLE_AUTOUPDATER": "1",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "FACTORY_TOKEN": "", "USER": "", "HOME": ws.Home}
			for k, v := range tt.wantEnv {
				env[k] = v
			}
			for k, v := range env {
				if c.Env[k] != v {
					t.Errorf("env %s = %q, want %q", k, c.Env[k], v)
				}
			}
			var codes []int
			for _, h := range c.Hooks {
				codes = append(codes, h.Exit)
			}
			if !slices.Equal(codes, tt.wantHooks) {
				t.Errorf("hook answers %v, want %v: %+v", codes, tt.wantHooks, c.Hooks)
			}
			if !strings.Contains(c.Settings, `"effortLevel": "high"`) || !strings.Contains(c.MCP, `"${GH_TOKEN}"`) {
				t.Errorf("settings %s\nmcp %s", c.Settings, c.MCP)
			}
			for _, sec := range testSecrets {
				if slices.ContainsFunc(c.Args, func(a string) bool { return strings.Contains(a, sec) }) {
					t.Errorf("a token is on the command line: %q", sec)
				}
			}
			lines := strings.Split(strings.TrimSpace(string(raw.Transcript)), "\n")
			if !strings.Contains(lines[0], `"factory.claude-exec"`) || !strings.Contains(string(raw.Transcript), `"subtype":"init"`) {
				t.Errorf("transcript:\n%s", raw.Transcript)
			}
			if len(s.Events) == 0 || s.Events[0].Kind != harness.KindTurn || len(s.Usages) == 0 || len(s.Limits) != 0 {
				t.Errorf("sink: %+v", s)
			}
			last := s.Usages[len(s.Usages)-1]
			if tt.wantCost != (last.CostUSD != nil) {
				t.Errorf("last usage %+v: cost wanted %v", last, tt.wantCost)
			}
			sess, err := readSession(dir)
			if err != nil || sess.SessionID != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" || sess.ReadOnly == tt.writable {
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
		auth      string
		wantErr   string
		wantLimit time.Time
		isLimit   bool
	}{
		{"usage limit in the stream", fakeRun{Stream: "claude-ratelimit.jsonl", Exit: 1}, AuthSubscription,
			"rate limited until 2026-10-19T14:05:00Z", t0.Add(2*time.Hour + 5*time.Minute), false},
		{"usage limit on stderr only", fakeRun{Stderr: "Error: Claude AI usage limit reached|1792422000\n", Exit: 1}, AuthSubscription,
			"rate limited", time.Unix(1792422000, 0), false},
		{"an error result", fakeRun{Stream: "claude-error.jsonl", Exit: 1}, AuthSubscription,
			"exit status 1: Connection error.", time.Time{}, false},
		{"an error result, exit 0", fakeRun{Stream: "claude-error.jsonl"}, AuthSubscription,
			"the run failed: Connection error.", time.Time{}, false},
		{"max turns", fakeRun{Stream: "claude-maxturns.jsonl", Exit: 1}, AuthSubscription,
			"limit maxTurns reached: claude stopped after 2 turns", time.Time{}, true},
		{"a plugin error stops the run", fakeRun{Stream: "claude-plugin-error.jsonl", Block: true}, AuthSubscription,
			"plugins failed to load", time.Time{}, false},
		{"no output", fakeRun{Stderr: "error: unknown option '--frobnicate'\n", Exit: 2}, AuthSubscription,
			"exit status 2: error: unknown option", time.Time{}, false},
		{"an unknown auth", fakeRun{Stream: "claude-success.jsonl"}, "sso",
			`unknown auth "sso"`, time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws, _ := prepared(t, true, fakePlan{Runs: []fakeRun{tt.run}})
			s := &harnesstest.Recorder{}
			start := time.Now()
			_, err := a.Run(context.Background(), ws, task("claude-sonnet-5-5", "high", tt.auth), "p", s)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err %v, want %q", err, tt.wantErr)
			}
			if harness.IsLimit(err) != tt.isLimit {
				t.Errorf("IsLimit(%v) = %v", err, !tt.isLimit)
			}
			if tt.wantLimit.IsZero() != (len(s.Limits) == 0) || (len(s.Limits) > 0 && !s.Limits[0].Equal(tt.wantLimit)) {
				t.Errorf("rate limits %v, want %v", s.Limits, tt.wantLimit)
			}
			if time.Since(start) > slow {
				t.Errorf("the run took %s", time.Since(start))
			}
		})
	}
}

func TestRepair(t *testing.T) {
	t.Parallel()
	t.Run("resumes the session", func(t *testing.T) {
		t.Parallel()
		a, ws, fc := prepared(t, true, fakePlan{Runs: []fakeRun{
			{Stream: "claude-multiturn.jsonl", Result: `{"status":"done"}`},
			{Stream: "claude-success.jsonl"},
		}})
		if _, err := a.Run(context.Background(), ws, task("claude-opus-5-5", "medium", AuthSubscription), "the prompt", &harnesstest.Recorder{}); err != nil {
			t.Fatal(err)
		}
		raw, err := a.Repair(context.Background(), ws, []string{"/outcome: missing"}, &harnesstest.Recorder{})
		if err != nil {
			t.Fatal(err)
		}
		if string(raw.Result) != okResult {
			t.Errorf("repair result %q", raw.Result)
		}
		run, c := fc.calls()[0], fc.calls()[1]
		if c.flag("--resume") != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" || c.flag("--model") != "claude-opus-5-5" || c.flag("--max-turns") != "5" ||
			c.flag("--settings") != run.flag("--settings") || c.flag("--json-schema") == "" || c.Dir != run.Dir {
			t.Errorf("repair args %q", c.Args)
		}
		if !strings.Contains(c.Stdin, "- /outcome: missing") || !strings.Contains(c.Stdin, ws.Result) || strings.Contains(c.Stdin, "the prompt") {
			t.Errorf("repair prompt %q", c.Stdin)
		}
	})
	t.Run("a new run without a session", func(t *testing.T) {
		t.Parallel()
		a, ws, fc := prepared(t, true, fakePlan{Runs: []fakeRun{{Stderr: "boom\n", Exit: 1}, {Stream: "claude-success.jsonl"}}})
		if _, err := a.Run(context.Background(), ws, task("claude-sonnet-5-5", "low", AuthSubscription), "the prompt", &harnesstest.Recorder{}); err == nil {
			t.Fatal("the run succeeded")
		}
		if _, err := a.Repair(context.Background(), ws, []string{"no result"}, &harnesstest.Recorder{}); err != nil {
			t.Fatal(err)
		}
		c := fc.calls()[1]
		if slices.Contains(c.Args, "--resume") || !strings.HasPrefix(c.Stdin, "the prompt") || !strings.Contains(c.Stdin, "- no result") {
			t.Errorf("repair args %q, prompt %q", c.Args, c.Stdin)
		}
		sess, _ := readSession(filepath.Join(ws.Home, ".factory-claude"))
		if sess.SessionID != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" {
			t.Errorf("the repair's session isn't recorded: %+v", sess)
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

// TestRunCancelKillsTheProcessTree: a cancelled run ends promptly although claude and the
// command it started both ignore SIGTERM, and neither process survives.
func TestRunCancelKillsTheProcessTree(t *testing.T) {
	t.Parallel()
	a, ws, fc := prepared(t, true, fakePlan{Runs: []fakeRun{{Stream: "claude-success.jsonl", Child: true, Block: true, IgnoreTerm: true}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, ws, task("claude-sonnet-5-5", "high", AuthSubscription), "p", &harnesstest.Recorder{})
		done <- err
	}()
	var pids []int
	for deadline := time.Now().Add(slow); len(pids) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the fake claude never started its child")
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

// TestRunRateLimitStopsTheRun: the pipeline cancels the run on RateLimited; claude, still
// running, is stopped.
func TestRunRateLimitStopsTheRun(t *testing.T) {
	t.Parallel()
	a, ws, _ := prepared(t, true, fakePlan{Runs: []fakeRun{{Stream: "claude-ratelimit-event.jsonl", Block: true}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &harnesstest.Recorder{OnLimit: cancel}
	start := time.Now()
	_, err := a.Run(ctx, ws, task("claude-sonnet-5-5", "high", AuthSubscription), "p", s)
	if !errors.Is(err, context.Canceled) || len(s.Limits) != 1 || !s.Limits[0].Equal(t0.Add(90*time.Minute)) || time.Since(start) > slow {
		t.Errorf("Run = %v after %s, limits %v", err, time.Since(start), s.Limits)
	}
}

func TestCompactSchema(t *testing.T) {
	dir := t.TempDir()
	if s, err := compactSchema(filepath.Join(dir, "none")); s != "" || err != nil {
		t.Errorf("no schema: %q, %v", s, err)
	}
	p := filepath.Join(dir, "s.json")
	for body, want := range map[string]string{"": "", "{\n \"a\": [1, 2]\n}\n": `{"a":[1,2],"type":"object"}`} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := compactSchema(p); s != want || err != nil {
			t.Errorf("compactSchema(%q) = %q, %v", body, s, err)
		}
	}
	for body, want := range map[string]string{"{not json": "", "true": "not a JSON object", `{"type": "string"}`: "root has type string"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := compactSchema(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("compactSchema(%q): %v, want an error with %q", body, err, want)
		}
	}
}

// sameDir compares directories through symlinks (macOS's /var is /private/var).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

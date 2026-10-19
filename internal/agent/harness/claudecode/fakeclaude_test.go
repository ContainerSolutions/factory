package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
)

// The test binary doubles as a fake claude (never the real one), as the child process a fake
// claude leaves running, and as the workspace's `factory` CLI: the pipeline's credential
// helper (`fx git-credential`) and the PreToolUse hook (`fx claude-hook`).
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fake-claude" {
		os.Exit(fakeClaudeMain(os.Args[2], os.Args[3:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-child" {
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if len(os.Args) > 2 && os.Args[1] == "fx" {
		switch os.Args[2] {
		case "git-credential":
			if err := gitcred.Helper(context.Background(), os.Args[3:], os.Stdin, os.Stdout, os.Getenv(gitcred.EnvSocket)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(5)
			}
			os.Exit(0)
		case HookOp:
			os.Exit(HookMain(os.Args[3:], os.Stdin, os.Stderr))
		}
		// Any other op (the Effector's `mcp`, AGT-S1-T05) is a usage error, as in
		// factory-agent today.
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// fakePlan scripts the fake claude: what --version prints, and one fakeRun per run, in
// order (the last one repeats).
type fakePlan struct {
	Version string    `json:"version"`
	Runs    []fakeRun `json:"runs"`
}

type fakeRun struct {
	// Stream is a recorded stream-json file replayed on stdout.
	Stream string `json:"stream"`
	// Result is written to $FACTORY_RESULT.
	Result string `json:"result,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   int    `json:"exit"`
	// ToolCalls are tool calls ({tool_name, tool_input}) put to the settings' PreToolUse
	// hooks, as claude would before running them.
	ToolCalls []json.RawMessage `json:"toolCalls,omitempty"`
	// Block waits to be killed after the stream; Child first starts a child process that
	// ignores SIGTERM; IgnoreTerm makes the fake ignore SIGTERM too.
	Block      bool `json:"block,omitempty"`
	Child      bool `json:"child,omitempty"`
	IgnoreTerm bool `json:"ignoreTerm,omitempty"`
}

// fakeCall is what one run saw.
type fakeCall struct {
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
	Dir   string            `json:"dir"`
	Stdin string            `json:"stdin"`
	// Settings and MCP are the --settings and --mcp-config files.
	Settings string `json:"settings"`
	MCP      string `json:"mcp"`
	// Hooks are the hooks' answers to ToolCalls.
	Hooks []hookCall `json:"hooks"`
}

type hookCall struct {
	Tool   string `json:"tool"`
	Exit   int    `json:"exit"`
	Stderr string `json:"stderr"`
}

// flag is the value after name in the call's arguments.
func (c fakeCall) flag(name string) string {
	for i, a := range c.Args {
		if a == name && i+1 < len(c.Args) {
			return c.Args[i+1]
		}
	}
	return ""
}

// fakeClaude is a fake claude in a temp dir: Binary is its wrapper script.
type fakeClaude struct {
	t      *testing.T
	dir    string
	Binary string
}

func newFakeClaude(t *testing.T, plan fakePlan) *fakeClaude {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClaude{t: t, dir: t.TempDir()}
	for i, r := range plan.Runs {
		if r.Stream != "" && !filepath.IsAbs(r.Stream) {
			abs, err := filepath.Abs(filepath.Join("testdata", r.Stream))
			if err != nil {
				t.Fatal(err)
			}
			plan.Runs[i].Stream = abs
		}
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.Binary = filepath.Join(f.dir, "claude")
	script := "#!/bin/sh\nexec " + gitcred.ShellQuote(exe) + " fake-claude " + gitcred.ShellQuote(f.dir) + " \"$@\"\n"
	if err := os.WriteFile(f.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

// calls are the runs so far.
func (f *fakeClaude) calls() []fakeCall {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []fakeCall
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var c fakeCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// pids are the fake's and its child's process ids, once a Child run started them.
func (f *fakeClaude) pids() []int {
	b, err := os.ReadFile(filepath.Join(f.dir, "pids"))
	if err != nil {
		return nil
	}
	var out []int
	for _, s := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}

func fakeClaudeMain(dir string, args []string) int {
	b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		return 3
	}
	var plan fakePlan
	if err := json.Unmarshal(b, &plan); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		return 3
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		v := plan.Version
		if v == "" {
			v = "2.1.3 (Claude Code)"
		}
		fmt.Println(v)
		return 0
	case len(args) > 0 && args[0] == "--print":
		return fakePrint(dir, plan, args)
	}
	fmt.Fprintf(os.Stderr, "fake claude: unexpected arguments %q\n", args)
	return 2
}

func fakePrint(dir string, plan fakePlan, args []string) int {
	stdin, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	call := fakeCall{Args: args, Dir: cwd, Stdin: string(stdin), Env: map[string]string{}}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		call.Env[k] = v
	}
	settings, _ := os.ReadFile(call.flag("--settings"))
	mcp, _ := os.ReadFile(call.flag("--mcp-config"))
	call.Settings, call.MCP = string(settings), string(mcp)

	callsPath := filepath.Join(dir, "calls.jsonl")
	prev, _ := os.ReadFile(callsPath)
	n := bytes.Count(prev, []byte("\n"))
	var run fakeRun
	if len(plan.Runs) > 0 {
		run = plan.Runs[min(n, len(plan.Runs)-1)]
	}
	for _, tc := range run.ToolCalls {
		call.Hooks = append(call.Hooks, runHooks(settings, tc, cwd)...)
	}
	line, _ := json.Marshal(call)
	if err := os.WriteFile(callsPath, append(prev, append(line, '\n')...), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		return 3
	}
	if run.IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if run.Child {
		exe, _ := os.Executable()
		child := exec.Command(exe, "fake-child")
		child.Stdout = os.Stdout // a command left running that holds claude's output open
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fake claude:", err)
			return 3
		}
		pids := fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)
		if err := os.WriteFile(filepath.Join(dir, "pids"), []byte(pids), 0o600); err != nil {
			return 3
		}
	}
	if run.Stream != "" {
		s, err := os.ReadFile(run.Stream)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake claude:", err)
			return 3
		}
		_, _ = os.Stdout.Write(s)
	}
	if run.Result != "" {
		_ = os.WriteFile(os.Getenv("FACTORY_RESULT"), []byte(run.Result), 0o600)
	}
	if run.Stderr != "" {
		fmt.Fprint(os.Stderr, run.Stderr)
	}
	if run.Block {
		select {}
	}
	return run.Exit
}

// runHooks puts one tool call to the settings' matching PreToolUse hooks (`sh -c`, the call
// as JSON on stdin) and records their answers.
func runHooks(settings []byte, toolCall json.RawMessage, cwd string) []hookCall {
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	_ = json.Unmarshal(settings, &s)
	var in map[string]any
	_ = json.Unmarshal(toolCall, &in)
	in["cwd"], in["hook_event_name"], in["session_id"] = cwd, "PreToolUse", "fake-session"
	tool, _ := in["tool_name"].(string)
	payload, _ := json.Marshal(in)
	var out []hookCall
	for _, m := range s.Hooks.PreToolUse {
		if ok, _ := regexp.MatchString("^(?:"+m.Matcher+")$", tool); !ok {
			continue
		}
		for _, h := range m.Hooks {
			var stderr bytes.Buffer
			cmd := exec.Command("/bin/sh", "-c", h.Command)
			cmd.Stdin, cmd.Stderr = bytes.NewReader(payload), &stderr
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				code = ee.ExitCode()
			case err != nil:
				code = -1
			}
			out = append(out, hookCall{Tool: tool, Exit: code, Stderr: stderr.String()})
		}
	}
	return out
}

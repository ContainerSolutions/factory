package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
)

// The test binary doubles as a fake codex (never the real one), as the child process a fake
// codex leaves running, and as the pipeline's credential helper (`fx git-credential`).
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fake-codex" {
		os.Exit(fakeCodexMain(os.Args[2], os.Args[3:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-child" {
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "fx" {
		// The workspace's `factory` CLI: the credential helper; the Effector's ops and `mcp`
		// aren't needed here (internal/agent/effector tests them): a usage error.
		if len(os.Args) > 2 && os.Args[2] == "git-credential" {
			if err := gitcred.Helper(context.Background(), os.Args[3:], os.Stdin, os.Stdout, os.Getenv(gitcred.EnvSocket)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(5)
			}
			os.Exit(0)
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// noCodex is New with Binary a path that doesn't exist. Every test whose adapter isn't meant
// to exec codex builds it with this, so a regression that would exec (a broken isolation
// guard, a check that stops failing first) fails the test instead of running the codex on
// PATH.
func noCodex(t *testing.T, o Options) *Adapter {
	t.Helper()
	if o.Binary == "" {
		o.Binary = filepath.Join(t.TempDir(), "no-codex")
	}
	return New(o)
}

// fakePlan scripts the fake codex: one fakeRun per exec, in order (the last one repeats).
type fakePlan struct {
	Runs []fakeRun `json:"runs"`
}

type fakeRun struct {
	// Stream is a recorded JSONL file replayed on stdout.
	Stream string `json:"stream"`
	// Result is written to $FACTORY_RESULT; LastMessage to --output-last-message's file.
	Result      string `json:"result,omitempty"`
	LastMessage string `json:"lastMessage,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	Exit        int    `json:"exit"`
	// Block waits to be killed after the stream; Child first starts a child process that
	// ignores SIGTERM; IgnoreTerm makes the fake ignore SIGTERM too.
	Block      bool `json:"block,omitempty"`
	Child      bool `json:"child,omitempty"`
	IgnoreTerm bool `json:"ignoreTerm,omitempty"`
}

// fakeCall is what one exec saw.
type fakeCall struct {
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
	Dir   string            `json:"dir"`
	Stdin string            `json:"stdin"`
	// Home lists CODEX_HOME (relative paths; a symlink as "path -> target").
	Home []string `json:"home"`
	// Config is CODEX_HOME/config.toml; Agents CODEX_HOME/AGENTS.md.
	Config string `json:"config"`
	Agents string `json:"agents"`
	// Auth is CODEX_HOME/auth.json when it is a regular file (the placeholder login).
	Auth string `json:"auth,omitempty"`
}

// fakeCodex is a fake codex in a temp dir: Binary is its wrapper script.
type fakeCodex struct {
	t      *testing.T
	dir    string
	Binary string
}

func newFakeCodex(t *testing.T, plan fakePlan) *fakeCodex {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCodex{t: t, dir: t.TempDir()}
	f.setPlan(plan)
	f.Binary = filepath.Join(f.dir, "codex")
	script := "#!/bin/sh\nexec " + gitcred.ShellQuote(exe) + " fake-codex " + gitcred.ShellQuote(f.dir) + " \"$@\"\n"
	if err := os.WriteFile(f.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeCodex) setPlan(plan fakePlan) {
	f.t.Helper()
	for i, r := range plan.Runs {
		if r.Stream != "" && !filepath.IsAbs(r.Stream) {
			abs, err := filepath.Abs(filepath.Join("testdata", r.Stream))
			if err != nil {
				f.t.Fatal(err)
			}
			plan.Runs[i].Stream = abs
		}
	}
	b, err := json.Marshal(plan)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "plan.json"), b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// calls are the execs so far.
func (f *fakeCodex) calls() []fakeCall {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []fakeCall
	sc := bufio.NewScanner(strings.NewReader(string(b)))
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
func (f *fakeCodex) pids() []int {
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

func fakeCodexMain(dir string, args []string) int {
	b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake codex:", err)
		return 3
	}
	var plan fakePlan
	if err := json.Unmarshal(b, &plan); err != nil {
		fmt.Fprintln(os.Stderr, "fake codex:", err)
		return 3
	}
	if len(args) > 0 && args[0] == "exec" {
		return fakeExec(dir, plan, args)
	}
	fmt.Fprintf(os.Stderr, "fake codex: unexpected arguments %q\n", args)
	return 2
}

func fakeExec(dir string, plan fakePlan, args []string) int {
	stdin, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	call := fakeCall{Args: args, Dir: cwd, Stdin: string(stdin), Env: map[string]string{}}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		call.Env[k] = v
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
			if err != nil || p == home {
				return nil
			}
			rel, _ := filepath.Rel(home, p)
			if d.Type()&os.ModeSymlink != 0 {
				target, _ := os.Readlink(p)
				rel += " -> " + target
			}
			call.Home = append(call.Home, rel)
			return nil
		})
		sort.Strings(call.Home)
		c, _ := os.ReadFile(filepath.Join(home, "config.toml"))
		a, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
		call.Config, call.Agents = string(c), string(a)
		if fi, err := os.Lstat(filepath.Join(home, "auth.json")); err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(filepath.Join(home, "auth.json"))
			call.Auth = string(b)
		}
	}
	callsPath := filepath.Join(dir, "calls.jsonl")
	prev, _ := os.ReadFile(callsPath)
	n := strings.Count(string(prev), "\n")
	line, _ := json.Marshal(call)
	if err := os.WriteFile(callsPath, append(prev, append(line, '\n')...), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake codex:", err)
		return 3
	}
	if len(plan.Runs) == 0 {
		return 0
	}
	run := plan.Runs[min(n, len(plan.Runs)-1)]
	if run.IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if run.Child {
		exe, _ := os.Executable()
		child := exec.Command(exe, "fake-child")
		child.Stdout = os.Stdout // a command left running that holds codex's output open
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fake codex:", err)
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
			fmt.Fprintln(os.Stderr, "fake codex:", err)
			return 3
		}
		_, _ = os.Stdout.Write(s)
	}
	if run.Result != "" {
		_ = os.WriteFile(os.Getenv("FACTORY_RESULT"), []byte(run.Result), 0o600)
	}
	if run.LastMessage != "" {
		for i, a := range args {
			if a == "--output-last-message" && i+1 < len(args) {
				_ = os.WriteFile(args[i+1], []byte(run.LastMessage), 0o600)
			}
		}
	}
	if run.Stderr != "" {
		fmt.Fprint(os.Stderr, run.Stderr)
	}
	if run.Block {
		select {}
	}
	return run.Exit
}

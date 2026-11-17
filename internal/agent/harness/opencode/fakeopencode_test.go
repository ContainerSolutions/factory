package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/gitcred"
	"github.com/ContainerSolutions/factory/internal/agent/internal/exectest"
)

// The test binary doubles as a fake opencode (never the real one) and the child it leaves
// running. As the workspace's `factory` CLI it fails: the fake starts no MCP server.
func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 2 && os.Args[1] == "fake-opencode":
		os.Exit(fakeMain(os.Args[2], os.Args[3:]))
	case len(os.Args) > 1 && os.Args[1] == "fake-child":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
		os.Exit(0)
	case len(os.Args) > 1 && os.Args[1] == "fx":
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// fakeRun scripts one fake opencode run; a fake has one per run, in order (the last repeats).
type fakeRun struct {
	// Stream is a testdata file replayed on stdout, without its # comment lines.
	Stream string `json:"stream"`
	// Result is written to $FACTORY_RESULT.
	Result string `json:"result,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   int    `json:"exit"`
	// Block waits to be killed after the stream; Stubborn first starts a child, and both
	// ignore SIGTERM.
	Block    bool `json:"block,omitempty"`
	Stubborn bool `json:"stubborn,omitempty"`
}

// fakeCall is what one run saw: its command line, environment, directory, stdin and the
// config dir's listing ($XDG_CONFIG_HOME/opencode, relative paths).
type fakeCall struct {
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Dir       string            `json:"dir"`
	Stdin     string            `json:"stdin"`
	ConfigDir []string          `json:"configDir"`
}

// fakeOpencode is a fake opencode in dir: Binary is its wrapper script.
type fakeOpencode struct {
	t      *testing.T
	dir    string
	Binary string
}

func newFakeOpencode(t *testing.T, runs ...fakeRun) *fakeOpencode {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOpencode{t: t, dir: t.TempDir()}
	for i := range runs {
		if runs[i].Stream != "" {
			runs[i].Stream, _ = filepath.Abs(filepath.Join("testdata", runs[i].Stream))
		}
	}
	b, _ := json.Marshal(runs)
	if err := os.WriteFile(filepath.Join(f.dir, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.Binary = filepath.Join(f.dir, "opencode")
	exectest.Write(t, f.Binary, "#!/bin/sh\nexec "+gitcred.ShellQuote(exe)+" fake-opencode "+gitcred.ShellQuote(f.dir)+" \"$@\"\n", 0o700)
	return f
}

// calls are the runs so far.
func (f *fakeOpencode) calls() []fakeCall {
	f.t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	var out []fakeCall
	for l := range bytes.Lines(b) {
		var c fakeCall
		if err := json.Unmarshal(l, &c); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// pids are the fake's and its child's process ids, once a Stubborn run started them.
func (f *fakeOpencode) pids() []int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "pids"))
	var out []int
	for _, s := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}

func fakeMain(dir string, args []string) int {
	var runs []fakeRun
	b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err == nil {
		err = json.Unmarshal(b, &runs)
	}
	if err != nil || len(args) == 0 || args[0] != "run" {
		fmt.Fprintf(os.Stderr, "fake opencode: %v, arguments %q\n", err, args)
		return 2
	}
	stdin, _ := io.ReadAll(os.Stdin)
	call := fakeCall{Args: args, Stdin: string(stdin), Env: map[string]string{}}
	call.Dir, _ = os.Getwd()
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		call.Env[k] = v
	}
	cfg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode")
	_ = filepath.WalkDir(cfg, func(p string, _ os.DirEntry, err error) error {
		if rel, _ := filepath.Rel(cfg, p); err == nil && p != cfg {
			call.ConfigDir = append(call.ConfigDir, filepath.ToSlash(rel))
		}
		return nil
	})
	callsPath := filepath.Join(dir, "calls.jsonl")
	prev, _ := os.ReadFile(callsPath)
	line, _ := json.Marshal(call)
	if err := os.WriteFile(callsPath, append(prev, append(line, '\n')...), 0o600); err != nil || len(runs) == 0 {
		return 3
	}
	run := runs[min(bytes.Count(prev, []byte("\n")), len(runs)-1)]
	if run.Stubborn {
		signal.Ignore(syscall.SIGTERM)
		exe, _ := os.Executable()
		child := exec.Command(exe, "fake-child")
		child.Stdout = os.Stdout // a command left running that holds opencode's output open
		if err := child.Start(); err != nil {
			return 3
		}
		_ = os.WriteFile(filepath.Join(dir, "pids"), fmt.Appendf(nil, "%d %d\n", os.Getpid(), child.Process.Pid), 0o600)
	}
	if run.Stream != "" {
		s, err := os.ReadFile(run.Stream)
		if err != nil {
			return 3
		}
		for l := range strings.Lines(string(s)) {
			if !strings.HasPrefix(l, "#") {
				_, _ = os.Stdout.WriteString(l)
			}
		}
	}
	if run.Result != "" {
		_ = os.WriteFile(os.Getenv("FACTORY_RESULT"), []byte(run.Result), 0o600)
	}
	fmt.Fprint(os.Stderr, run.Stderr)
	if run.Block || run.Stubborn {
		select {}
	}
	return run.Exit
}

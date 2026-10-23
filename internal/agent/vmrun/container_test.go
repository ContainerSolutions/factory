package vmrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/internal/fakegateway"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The container layer (workspace-run.md §Tests, layer 5) is opt-in: `make e2e-run-container`
// builds the linux factory-agent and this package's test binary, and runs
// TestRunInContainer in a plain container as root, with no network, with these set. The
// Makefile's target names the test and both variables: change them together. It fails unless
// TestRunInContainer passed and nothing skipped, so a rename there can't pass silently.
const (
	// envContainer=1 opts in; the test then requires linux and root.
	envContainer = "FACTORY_E2E_CONTAINER"
	// envContainerAgent is the factory-agent binary under test.
	envContainerAgent = "FACTORY_E2E_AGENT_BIN"
)

// fakeHarnessArg makes the test binary a fake harness: `<test> fake-harness <dir> <name> args…`.
const fakeHarnessArg = "fake-harness"

// harnessCall is what the fake harness saw and did, recorded in its dir.
type harnessCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Dir  string            `json:"dir"`
	UID  int               `json:"uid"`
	// Factory is the `factory` the PATH found; Pushed the commit `factory push` pushed.
	Factory string `json:"factory"`
	Pushed  string `json:"pushed"`
	// Auth is codex's $CODEX_HOME/auth.json.
	Auth string `json:"auth,omitempty"`
	// Hooks are claude's PreToolUse hooks' exit codes by tool call.
	Hooks map[string]int `json:"hooks,omitempty"`
}

// hookCalls are the tool calls the fake claude puts to its PreToolUse hooks: a git push is
// blocked (writes go through `factory push`), a read is allowed.
var hookCalls = map[string]int{"git push origin HEAD": 2, "git status": 0}

// The streams the fake harnesses print: the shortest each adapter takes as a finished turn.
var fakeStreams = map[string]string{
	config.HarnessClaudeCode: `{"type":"system","subtype":"init","session_id":"s-1","model":"m","permissionMode":"bypassPermissions"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"pushed the intro","session_id":"s-1","usage":{"input_tokens":10,"output_tokens":5}}
`,
	config.HarnessCodex: `{"type":"thread.started","thread_id":"t-1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"pushed the intro"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`,
}

// factory-agent run, the linux binary, in a plain container as root (workspace-run.md
// §Tests, layer 5): for each harness, the real adapter runs a fake harness binary that
// commits, `factory push`es and `factory comment`s through the fakegateway, inside a
// container with no network. It checks the base image's assumptions: bash, git and its
// http-backend, the system CA roots, and a root run (IS_SANDBOX for claude). Seam (P1 phase
// 3, PR 7): the environment's scripts run here too once envrun lands.
func TestRunInContainer(t *testing.T) {
	if os.Getenv(envContainer) != "1" {
		t.Skipf("the container layer is opt-in: make e2e-run-container (%s=1)", envContainer)
	}
	agent := os.Getenv(envContainerAgent)
	switch {
	case runtime.GOOS != "linux" || os.Getuid() != 0:
		t.Fatalf("%s=1 runs in the container make e2e-run-container starts: linux as root (here %s, uid %d)", envContainer, runtime.GOOS, os.Getuid())
	case agent == "":
		t.Fatalf("%s is unset: the factory-agent binary under test", envContainerAgent)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the image has no git: %v", err)
	}
	out, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	// harnesstest.NewE2E skips without it: here that would pass silently.
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")); err != nil {
		t.Fatalf("the image has no git http-backend: %v", err)
	}
	for _, name := range []string{config.HarnessClaudeCode, config.HarnessCodex} {
		t.Run(name, func(t *testing.T) { runInContainer(t, agent, name) })
	}
}

// runInContainer is one run of factory-agent run with harness name faked.
func runInContainer(t *testing.T, agent, name string) {
	e := harnesstest.NewE2E(t, vmTaskID, vmPrompt)
	gw := fakegateway.Start(t, fakegateway.Config{RunToken: fakegateway.RunToken, Dispatcher: e.DispatcherURL(), TaskID: vmTaskID,
		GitUpstream: e.GitURL(), Repos: map[string]fakegateway.Repo{"acme/docs": {Token: harnesstest.GitToken, RW: true}}})
	job := e.Job(v1alpha1.HarnessResolved{Name: name, Model: "m"}, []string{"pr-opened"})
	job.Lease.Task.Spec.Effects = &v1alpha1.EffectsPolicy{Allow: []v1alpha1.EffectOp{v1alpha1.OpPushBranch, v1alpha1.OpComment}}
	x := pipeline.BundleExtras{CA: gw.CA().CertPEM()}
	if name == config.HarnessCodex {
		acct := harnesstest.CodexAccount
		x.Codex = &acct
	}
	bundle := e.BundleJob(job, x)
	base := t.TempDir()
	work, state := filepath.Join(base, "work"), filepath.Join(base, "state")
	fakeDir, bin := fakeHarness(t, name), guardBin(t)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, agent, "run", "--proxy", gw.URL(), "--bundle", bundle, "--work", work, "--state-dir", state,
		"--system-setup", "off", "--log-format", "text", "--log-level", "debug",
		"--harness-bin", name+"="+filepath.Join(fakeDir, "harness"))
	cmd.Env = []string{"PATH=" + bin + string(filepath.ListSeparator) + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8"}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	defer func() {
		if t.Failed() {
			t.Logf("factory-agent run's log:\n%s", stderr.String())
		}
	}()
	if err != nil {
		t.Fatalf("factory-agent run: %v", err)
	}

	o := readOutcome(t, work)
	if o.Status != runner.StatusDone || o.Result == nil || o.Result.Outcome != "pr-opened" || o.Transcript != runbundle.TranscriptFile {
		t.Fatalf("outcome %+v %+v", o, o.Result)
	}
	// The real adapter captured the fake's stdout: the stream's last message is in it.
	if lines := gunzipLines(t, filepath.Join(work, runbundle.OutDir, runbundle.TranscriptFile)); !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, "pushed the intro")
	}) {
		t.Errorf("the transcript: %q", lines)
	}
	if _, err := runbundle.ReadOutcome(filepath.Join(work, runbundle.OutDir), time.Now()); err != nil {
		t.Errorf("the host's ReadOutcome: %v", err)
	}
	evs := decodeEvents(t, stdout.String())
	if got := eventTypes(evs); len(got) < 2 || got[0] != runbundle.EventStart || got[len(got)-1] != runbundle.EventOutcome {
		t.Errorf("event types %v", got)
	}

	// The fake ran once (no repair), as root, with the VM's environment and no credential.
	if steps := stepNames(evs); slices.ContainsFunc(steps, func(s string) bool { return strings.HasPrefix(s, "repair:") }) {
		t.Errorf("steps %v: the harness was repaired", steps)
	}
	c := readCall(t, fakeDir)
	if got := e.RemoteHead("acme/docs", vmBranch); got == "" || got != c.Pushed {
		t.Errorf("remote %s = %q, want %q", vmBranch, got, c.Pushed)
	}
	var ops []string
	for _, ef := range e.Effects() {
		ops = append(ops, ef.Op)
		if ef.Auth != "Bearer "+fakegateway.RunToken {
			t.Errorf("%s reached the dispatcher with %q", ef.Op, ef.Auth)
		}
	}
	if !slices.Equal(ops, []string{"push-branch", "comment"}) {
		t.Errorf("effects %v", ops)
	}
	gw.AssertNoClientCredential(t)
	for k, v := range c.Env {
		if hit := credential(v); hit != "" {
			t.Errorf("the harness's %s holds %s", k, hit)
		}
	}
	if _, ok := c.Env[pipeline.EnvToken]; ok {
		t.Errorf("the harness has %s", pipeline.EnvToken)
	}
	want := map[string]string{"HTTPS_PROXY": gw.URL(), "https_proxy": gw.URL(), pipeline.EnvGHToken: harness.Placeholder,
		pipeline.EnvRuntime: pipeline.RuntimeWorkspace, pipeline.EnvAPI: runbundle.DefaultDispatcherURL,
		"GIT_SSL_CAINFO": filepath.Join(state, CABundleFile)}
	switch name {
	case config.HarnessClaudeCode:
		// A root run: claude bypasses permissions only with IS_SANDBOX (UNVERIFIED, LLD risk 2).
		want["IS_SANDBOX"], want["CLAUDE_CODE_OAUTH_TOKEN"] = "1", harness.Placeholder
		if !maps.Equal(c.Hooks, hookCalls) {
			t.Errorf("the PreToolUse hooks answered %v, want %v", c.Hooks, hookCalls)
		}
	case config.HarnessCodex:
		if !slices.Contains(c.Args, "danger-full-access") || !strings.Contains(c.Auth, harnesstest.CodexAccount.AccountID) || !strings.Contains(c.Auth, harness.Placeholder) {
			t.Errorf("codex's args %q and auth.json %s", c.Args, c.Auth)
		}
	}
	for k, v := range want {
		if c.Env[k] != v {
			t.Errorf("the harness's %s = %q, want %q", k, c.Env[k], v)
		}
	}
	if c.UID != 0 || !strings.HasPrefix(c.Factory, filepath.Join(work, "bin")+string(filepath.Separator)) {
		t.Errorf("the harness ran as uid %d with factory %q", c.UID, c.Factory)
	}

	// The CA bundle is the image's roots followed by the gateway CA.
	roots, err := os.ReadFile(systemRoots)
	if err != nil {
		t.Fatalf("the image's CA roots: %v", err)
	}
	ca, err := os.ReadFile(filepath.Join(state, CABundleFile))
	if err != nil || !bytes.HasPrefix(ca, roots) || !bytes.Contains(ca, gw.CA().CertPEM()) {
		t.Errorf("the CA bundle (%d bytes, %v) isn't the roots then the gateway CA", len(ca), err)
	}
	for _, dir := range []string{work, state, bundle} {
		scanNoCredential(t, dir)
	}
}

// fakeHarness writes the wrapper script --harness-bin points at, in a new dir that also
// gets the fake's record.
func fakeHarness(t *testing.T, name string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + gitcred.ShellQuote(exe) + " " + fakeHarnessArg + " " + gitcred.ShellQuote(dir) + " " + gitcred.ShellQuote(name) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "harness"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// guardBin is a dir of claude and codex stubs that exit 97, first on the run's PATH: a run
// that fell back to the harness on PATH fails instead of running a real one.
func guardBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"claude", "codex"} {
		script := "#!/bin/sh\necho 'the real " + n + " must not run in a test' >&2\nexit 97\n"
		if err := os.WriteFile(filepath.Join(dir, n), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readCall(t *testing.T, dir string) harnessCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "call.json"))
	if err != nil {
		t.Fatalf("the fake harness's record: %v", err)
	}
	var c harnessCall
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeHarnessMain is the fake harness: a run (anything but --version) records its call,
// does the task's work with the workspace's git and `factory`, puts claude's hook calls to
// its settings, writes $FACTORY_RESULT (and codex's last message), and prints the stream.
func fakeHarnessMain(dir, name string, args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("0.0.0 (fake)")
		return 0
	}
	if err := fakeHarnessRun(dir, name, args); err != nil {
		fmt.Fprintln(os.Stderr, "fake harness:", err)
		return 3
	}
	fmt.Print(fakeStreams[name])
	return 0
}

func fakeHarnessRun(dir, name string, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	c := harnessCall{Args: args, Dir: cwd, UID: os.Getuid(), Env: map[string]string{}}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		c.Env[k] = v
	}
	if c.Factory, err = exec.LookPath("factory"); err != nil {
		return err
	}
	repo := cwd
	run := func(bin string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s %v: %w\n%s", bin, args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if err := os.WriteFile(filepath.Join(repo, "intro.md"), []byte("# Intro\n"), 0o600); err != nil {
		return err
	}
	for _, a := range [][]string{{"checkout", "--quiet", "-b", vmBranch}, {"add", "-A"}, {"commit", "--quiet", "--no-gpg-sign", "-m", "intro"}} {
		if _, err := run("git", a...); err != nil {
			return err
		}
	}
	if c.Pushed, err = run("git", "rev-parse", "HEAD"); err != nil {
		return err
	}
	for _, a := range [][]string{{"push"}, {"comment", "--body", "Pushed the intro."}} {
		if _, err := run(c.Factory, a...); err != nil {
			return err
		}
	}
	switch name {
	case config.HarnessClaudeCode:
		if c.Hooks, err = runHooks(flagValue(args, "--settings"), cwd); err != nil {
			return err
		}
	case config.HarnessCodex:
		auth, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"))
		if err != nil {
			return err
		}
		c.Auth = string(auth)
		if last := flagValue(args, "--output-last-message"); last != "" {
			if err := os.WriteFile(last, []byte(goodResult), 0o600); err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(os.Getenv(pipeline.EnvResult), []byte(goodResult), 0o600); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "call.json"), b, 0o600)
}

// runHooks puts each of hookCalls, as a Bash tool call, to the settings' PreToolUse hooks
// (`sh -c`, the call as JSON on stdin, as claude does) and returns their exit codes.
func runHooks(settingsFile, cwd string) (map[string]int, error) {
	raw, err := os.ReadFile(settingsFile)
	if err != nil {
		return nil, err
	}
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if len(s.Hooks.PreToolUse) != 1 || len(s.Hooks.PreToolUse[0].Hooks) != 1 {
		return nil, fmt.Errorf("want one PreToolUse hook: %s", raw)
	}
	hook := s.Hooks.PreToolUse[0].Hooks[0].Command
	out := map[string]int{}
	for command := range hookCalls {
		in, err := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command},
			"cwd": cwd, "hook_event_name": "PreToolUse", "session_id": "s-1"})
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("/bin/sh", "-c", hook)
		cmd.Stdin = bytes.NewReader(in)
		err = cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			out[command] = 0
		case errors.As(err, &ee):
			out[command] = ee.ExitCode()
		default:
			return nil, err
		}
	}
	return out, nil
}

// flagValue is the value after name in args, or "".
func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

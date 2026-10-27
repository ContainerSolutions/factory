package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// vmWorkspace is workspace(t, writable) as a VM workspace's pipeline hands it over in gateway
// mode: no credential socket, no FACTORY_TOKEN, the gateway's proxy and CA, and GH_TOKEN the
// placeholder.
func vmWorkspace(t *testing.T, writable bool) harness.Workspace {
	t.Helper()
	ws := workspace(t, writable)
	ws.Env = []string{"FACTORY_API=http://dispatcher.factory.internal", "FACTORY_RESULT=" + ws.Result, "FACTORY_RUNTIME=workspace",
		"GH_TOKEN=" + harness.Placeholder, "HOME=" + ws.Home, "HTTPS_PROXY=http://192.168.64.1:3128", "NO_PROXY=localhost,127.0.0.1",
		"PATH=" + ws.Bin + ":/usr/bin:/bin", "SSL_CERT_FILE=/var/lib/factory-agent/run/ca-bundle.pem", "TMPDIR=" + ws.Tmp}
	return ws
}

// noBinary is a claude path that doesn't exist: an adapter that must not run claude gets it
// (rather than "claude" on PATH, the real one), so a regression fails the run fast.
func noBinary(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-claude")
}

// vmAdapter is the adapter with uid as the run's user, running binary
// (noBinary when it is "").
func vmAdapter(t *testing.T, uid int, binary string) *Adapter {
	t.Helper()
	if binary == "" {
		binary = noBinary(t)
	}
	return New(Options{
		Binary: binary, Clock: clock.NewFake(t0), KillGrace: 200 * time.Millisecond,
		Config: config.Harness{Default: "claude-sonnet-5-5"},
		Getuid: func() int { return uid },
	})
}

// golden compares got, with the workspace root replaced by $ROOT, against testdata/vm/<name>
// (-update rewrites it).
func golden(t *testing.T, ws harness.Workspace, name string, got []byte) {
	t.Helper()
	got = bytes.ReplaceAll(got, []byte(ws.Root), []byte("$ROOT"))
	p := filepath.Join("testdata", "vm", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs:\n%s", p, got)
	}
}

// TestVMPrepare pins the files Prepare writes: the settings have no sandbox
// and no permission rules but keep the plugins, the effort and the hook (with --write / when
// the checkout is writable), the MCP config is the host's, and the scratch CLAUDE_CONFIG_DIR
// exists.
func TestVMPrepare(t *testing.T) {
	t.Parallel()
	for _, writable := range []bool{true, false} {
		name := map[bool]string{true: "read-write", false: "read-only"}[writable]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ws := vmWorkspace(t, writable)
			if err := vmAdapter(t, 1000, "").Prepare(context.Background(), ws, task("claude-sonnet-5-5", "max", AuthSubscription)); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(ws.Home, ".factory-claude")
			if got := listDir(t, dir); !slices.Equal(got, []string{"mcp.json", "settings.json"}) {
				t.Errorf("scratch dir: %q", got)
			}
			if fi, err := os.Stat(filepath.Join(ws.Home, ".claude")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Errorf("the scratch config dir: %v %v", fi, err)
			}
			settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
			var doc map[string]any
			if err := json.Unmarshal(settings, &doc); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"sandbox", "permissions"} {
				if _, ok := doc[k]; ok {
					t.Errorf("the VM settings have a %s block:\n%s", k, settings)
				}
			}
			golden(t, ws, "settings-"+name+".golden.json", settings)
			// The MCP config doesn't depend on the checkout: one golden, which the read-write
			// case writes on -update.
			mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
			if writable || !*update {
				golden(t, ws, "mcp.golden.json", mcp)
			}
			scanFiles(t, ws.Root)
		})
	}
}

// TestVMRunEnv pins claude's environment: the pipeline's, the scratch
// config dir and the placeholder as the auth mode's credential; no USER, nothing of the
// worker's; IS_SANDBOX only as root.
func TestVMRunEnv(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		auth string
		uid  int
		// env is added to the workspace's environment, which the run must override.
		env []string
	}{
		{"subscription", AuthSubscription, 1000, []string{"ANTHROPIC_API_KEY=sk-ant-api03-worker-key"}},
		{"apikey-root", AuthAPIKey, 0, []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-worker-token", "CLAUDE_CONFIG_DIR=/home/admin/.claude"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := vmWorkspace(t, true)
			ws.Env = append(ws.Env, tt.env...)
			env, err := vmAdapter(t, tt.uid, "").runEnv(ws, session{Auth: tt.auth})
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(env)
			golden(t, ws, "env-"+tt.name+".golden", []byte(strings.Join(env, "\n")+"\n"))
		})
	}
	if _, err := vmAdapter(t, 1000, "").runEnv(vmWorkspace(t, true), session{Auth: "sso"}); err == nil || !strings.Contains(err.Error(), `unknown auth "sso"`) {
		t.Errorf("an unknown auth: %v", err)
	}
}

// TestVMRun runs the fake claude: the placeholder credential and the scratch
// config dir reach it, the hook lets the file tools write anywhere in a writable workspace
// and still refuses git push and gh writes, a read-only workspace keeps its disallowed write
// tools, and Repair resumes the session with the same environment.
func TestVMRun(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		writable  bool
		auth      string
		wantHooks []int // exit codes for: write home/, edit repo/, git push, gh pr create
	}{
		{"read-write", true, AuthSubscription, []int{0, 0, hookExitBlock, hookExitBlock}},
		{"read-only, no resolved auth", false, "", []int{hookExitBlock, hookExitBlock, hookExitBlock, hookExitBlock}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := vmWorkspace(t, tt.writable)
			fc := newFakeClaude(t, fakePlan{Runs: []fakeRun{
				{Stream: "claude-multiturn.jsonl", Result: `{"status":"done"}`, ToolCalls: []json.RawMessage{
					toolCall("Write", map[string]any{"file_path": filepath.Join(ws.Home, "notes.md")}),
					toolCall("Edit", map[string]any{"file_path": "docs/a.md"}),
					toolCall("Bash", map[string]any{"command": "git push origin HEAD"}),
					toolCall("Bash", map[string]any{"command": "gh pr create --fill"}),
				}},
				{Stream: "claude-success.jsonl"},
			}})
			a := vmAdapter(t, 1000, fc.Binary)
			if err := a.Prepare(context.Background(), ws, task("", "high", tt.auth)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Run(context.Background(), ws, task("claude-opus-5-5", "high", tt.auth), "the prompt\n", &harnesstest.Recorder{}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Repair(context.Background(), ws, []string{"/outcome: missing"}, &harnesstest.Recorder{}); err != nil {
				t.Fatal(err)
			}
			calls := fc.calls()
			if len(calls) != 2 {
				t.Fatalf("%d claude runs, want the run and the repair", len(calls))
			}
			for i, c := range calls {
				want := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(ws.Home, ".claude"), "CLAUDE_CODE_OAUTH_TOKEN": harness.Placeholder,
					"GH_TOKEN": harness.Placeholder, "HTTPS_PROXY": "http://192.168.64.1:3128", "DISABLE_AUTOUPDATER": "1"}
				for k, v := range want {
					if c.Env[k] != v {
						t.Errorf("call %d: env %s = %q, want %q", i, k, c.Env[k], v)
					}
				}
				for _, k := range []string{"USER", "ANTHROPIC_API_KEY", "IS_SANDBOX", "FACTORY_TOKEN", "FACTORY_GIT_CREDENTIAL_SOCKET"} {
					if v, ok := c.Env[k]; ok {
						t.Errorf("call %d: env has %s=%q", i, k, v)
					}
				}
				if slices.Contains(c.Args, "--disallowedTools") == tt.writable {
					t.Errorf("call %d: a writable checkout (%v) with args %q", i, tt.writable, c.Args)
				}
				if strings.Contains(c.Settings, `"sandbox"`) || strings.Contains(c.Settings, `"permissions"`) {
					t.Errorf("call %d: settings\n%s", i, c.Settings)
				}
			}
			if calls[1].flag("--resume") != "5d3b1f0e-8c2a-4e57-9b1d-2a6c7e9f0a11" {
				t.Errorf("repair args %q", calls[1].Args)
			}
			var codes []int
			for _, h := range calls[0].Hooks {
				codes = append(codes, h.Exit)
			}
			if !slices.Equal(codes, tt.wantHooks) || !strings.Contains(calls[0].Hooks[2].Stderr, "factory push") {
				t.Errorf("hook answers %+v, want %v", calls[0].Hooks, tt.wantHooks)
			}
		})
	}
}

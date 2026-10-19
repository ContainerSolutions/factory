package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The owner's login in tests: it must never be read, copied or written by the adapter.
const ownerLogin = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-owner-never-copied","refreshToken":"sk-ant-ort01-owner-never-copied"}}`

// tokens that must never reach a file the adapter writes or a command line.
var testSecrets = []string{"sk-ant-oat01-owner-never-copied", "sk-ant-ort01-owner-never-copied", "ghs_taskTokenNeverOnDisk0123456789",
	"agent-token-never-on-disk", "sk-ant-api03-worker-key"}

// ownerConfigDir is a signed-in dedicated CLAUDE_CONFIG_DIR (Linux layout: the login in a file).
func ownerConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{credentialsFile: ownerLogin, stateFile: `{"hasCompletedOnboarding":true}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// workspace is what the pipeline hands an adapter: its directories, a checkout, the skills in
// the plugin layout (two plugins with a "review" skill), the schema, the environment, and a
// `factory` CLI that is the test binary's `fx`.
func workspace(t *testing.T, writable bool) harness.Workspace {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tsk_1")
	ws := harness.Workspace{
		Root: root, Repo: filepath.Join(root, "repo"), RepoWritable: writable,
		Skills: filepath.Join(root, "skills"), Out: filepath.Join(root, "out"),
		Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), Bin: filepath.Join(root, "bin"),
		Preamble: "You are running unattended inside CCF Factory (task tsk_1, task spec implement).",
	}
	ws.WorkDir = ws.Repo
	ws.Result = filepath.Join(ws.Out, "result.json")
	ws.Schema = filepath.Join(root, "result.schema.json")
	ws.Mounts = []harness.Mount{{Repo: "acme/docs", Dir: ws.Repo, Writable: writable}, {Repo: "acme/spec", Dir: filepath.Join(root, "ro", "acme", "spec")}}
	for _, d := range []string{ws.Repo, ws.Skills, ws.Out, ws.Home, ws.Tmp, ws.Bin, ws.Mounts[1].Dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ws.Schema, []byte("{\n  \"type\": \"object\",\n  \"required\": [\"status\"]\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"ccf-docs/.claude-plugin/plugin.json":         `{"name": "ccf-docs"}`,
		"ccf-docs/skills/docs-style/SKILL.md":         "---\nname: docs-style\n---\n",
		"ccf-review/.claude-plugin/plugin.json":       `{"name": "ccf-review"}`,
		"ccf-review/skills/review/SKILL.md":           "---\nname: review\n---\nccf-review\n",
		"lisa/.claude-plugin/plugin.json":             `{"name": "lisa"}`,
		"lisa/skills/review/SKILL.md":                 "---\nname: review\n---\nlisa\n",
		"ccf-docs/skills/docs-style/scripts/check.sh": "#!/bin/sh\necho ok\n",
	}
	for name, body := range files {
		p := filepath.Join(ws.Skills, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range [][2]string{{"ccf-docs", "docs-style"}, {"ccf-review", "review"}, {"lisa", "review"}} {
		ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: s[0], Name: s[1], Dir: filepath.Join(ws.Skills, s[0], "skills", s[1])})
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexec " + gitcred.ShellQuote(exe) + " fx \"$@\"\n"
	if err := os.WriteFile(filepath.Join(ws.Bin, "factory"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	ws.Hosts = []string{"github.com", "factory.example.dev"}
	ws.Env = []string{"FACTORY_API=http://127.0.0.1:1", "FACTORY_GIT_CREDENTIAL_SOCKET=/tmp/fxgc-1/s", "FACTORY_RESULT=" + ws.Result, "FACTORY_TOKEN=agent-token-never-on-disk",
		"GH_TOKEN=ghs_taskTokenNeverOnDisk0123456789", "HOME=" + ws.Home, "PATH=" + ws.Bin + ":" + os.Getenv("PATH"), "TMPDIR=" + ws.Tmp}
	return ws
}

// sandboxed finds every OS sandbox tool.
func sandboxed(string) (string, error) { return "/usr/bin/true", nil }

func task(model, effort, auth string) v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{
		Harness: &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{Name: Name, Model: model, ReasoningEffort: effort, Auth: auth}},
		Limits:  &v1alpha1.Limits{MaxTurns: 30, MaxCostUSD: 2.5},
	}}
}

// settingsDoc is the generated settings file, decoded.
type settingsDoc struct {
	EnabledPlugins map[string]any `json:"enabledPlugins"`
	EffortLevel    string         `json:"effortLevel"`
	Sandbox        struct {
		Enabled                   *bool    `json:"enabled"`
		AllowUnsandboxedCommands  *bool    `json:"allowUnsandboxedCommands"`
		AutoAllowBashIfSandboxed  bool     `json:"autoAllowBashIfSandboxed"`
		ExcludedCommands          []string `json:"excludedCommands"`
		EnableWeakerNestedSandbox *bool    `json:"enableWeakerNestedSandbox"`
		Network                   struct {
			AllowUnixSockets  []string `json:"allowUnixSockets"`
			AllowLocalBinding *bool    `json:"allowLocalBinding"`
		} `json:"network"`
	} `json:"sandbox"`
	Permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
	Hooks struct {
		PreToolUse []struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"PreToolUse"`
	} `json:"hooks"`
}

func TestPrepareLayout(t *testing.T) {
	for _, writable := range []bool{true, false} {
		t.Run(map[bool]string{true: "read-write", false: "read-only"}[writable], func(t *testing.T) {
			owner := ownerConfigDir(t)
			before := listDir(t, owner)
			ws := workspace(t, writable)
			a := New(Options{LookPath: sandboxed, Config: config.Harness{ConfigDir: owner}, Environ: workerEnv()})
			if err := a.Prepare(context.Background(), ws, task("claude-sonnet-5-5", "max", AuthSubscription)); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(ws.Home, ".factory-claude")
			if got := listDir(t, dir); !slices.Equal(got, []string{"mcp.json", "settings.json"}) {
				t.Errorf("scratch dir: %q", got)
			}
			var s settingsDoc
			b, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
			if s.EnabledPlugins == nil || len(s.EnabledPlugins) != 0 || s.EffortLevel != "high" || len(s.Hooks.PreToolUse) != 1 {
				t.Fatalf("settings:\n%s", b)
			}
			// The OS sandbox: on, no escape, nothing exempt, only the credential helper's socket.
			sb := s.Sandbox
			if sb.Enabled == nil || !*sb.Enabled || sb.AllowUnsandboxedCommands == nil || *sb.AllowUnsandboxedCommands ||
				sb.ExcludedCommands == nil || len(sb.ExcludedCommands) != 0 || sb.EnableWeakerNestedSandbox == nil || *sb.EnableWeakerNestedSandbox ||
				!slices.Equal(sb.Network.AllowUnixSockets, []string{"/tmp/fxgc-1/s"}) || sb.Network.AllowLocalBinding == nil || *sb.Network.AllowLocalBinding {
				t.Errorf("sandbox:\n%s", b)
			}
			allow := []string{"Edit(/" + ws.Out + "/**)", "Edit(/" + ws.Tmp + "/**)"}
			if writable {
				allow = append(allow, "Edit(/"+ws.Repo+"/**)")
			}
			allow = append(allow, "Edit(/"+ws.Home+"/**)",
				"WebFetch(domain:github.com)", "WebFetch(domain:api.github.com)", "WebFetch(domain:*.githubusercontent.com)",
				"WebFetch(domain:factory.example.dev)", "WebFetch(domain:api.anthropic.com)")
			if !slices.Equal(s.Permissions.Allow, allow) {
				t.Errorf("allow rules\n  %q\nwant\n  %q", s.Permissions.Allow, allow)
			}
			for _, w := range []string{"Edit(/" + dir + "/**)", "Read(//Users/owner/.ssh)", "Read(//Users/owner/.ssh/**)", "Read(//Users/owner/.config/gh/**)",
				"Read(//Users/owner/.factory-agent/codex-home/**)", "Read(/" + owner + "/**)"} {
				if !slices.Contains(s.Permissions.Deny, w) {
					t.Errorf("deny rules %q lack %q", s.Permissions.Deny, w)
				}
			}
			h := s.Hooks.PreToolUse[0]
			if h.Matcher != "Write|Edit|MultiEdit|NotebookEdit|Bash" || len(h.Hooks) != 1 || h.Hooks[0].Type != "command" || h.Hooks[0].Timeout != 30 {
				t.Errorf("hook %+v", h)
			}
			wantCmd := gitcred.ShellQuote(filepath.Join(ws.Bin, "factory")) + " claude-hook pre-tool-use --write " + gitcred.ShellQuote(ws.Out) +
				" --write " + gitcred.ShellQuote(ws.Tmp)
			if writable {
				wantCmd += " --write " + gitcred.ShellQuote(ws.Repo)
			}
			if h.Hooks[0].Command != wantCmd {
				t.Errorf("hook command\n  %s\nwant\n  %s", h.Hooks[0].Command, wantCmd)
			}
			var m map[string]map[string]struct {
				Type    string            `json:"type"`
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			}
			b, _ = os.ReadFile(filepath.Join(dir, "mcp.json"))
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			f := m["mcpServers"]["factory"]
			if f.Type != "stdio" || f.Command != filepath.Join(ws.Bin, "factory") || !slices.Equal(f.Args, []string{"mcp"}) ||
				len(f.Env) != 8 || f.Env["FACTORY_TOKEN"] != "${FACTORY_TOKEN}" || f.Env["GH_TOKEN"] != "${GH_TOKEN}" {
				t.Errorf("mcp.json:\n%s", b)
			}
			scanFiles(t, ws.Root)
			if got := listDir(t, owner); !slices.Equal(got, before) {
				t.Errorf("the config dir changed: %q, was %q", got, before)
			}
			if b, _ := os.ReadFile(filepath.Join(owner, credentialsFile)); string(b) != ownerLogin {
				t.Errorf("the owner's login changed: %q", b)
			}
		})
	}
}

func TestPrepareWithoutEffort(t *testing.T) {
	ws := workspace(t, true)
	if err := New(Options{LookPath: sandboxed, Config: config.Harness{ConfigDir: ownerConfigDir(t)}}).Prepare(context.Background(), ws, task("", "", "")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(ws.Home, ".factory-claude", "settings.json"))
	if strings.Contains(string(b), "effortLevel") {
		t.Errorf("an effort without one asked:\n%s", b)
	}
}

func TestPrepareRefusals(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]config.Harness{
		"no config dir":       {},
		"relative config dir": {ConfigDir: "claude"},
		"missing config dir":  {ConfigDir: filepath.Join(t.TempDir(), "nope")},
		"not a directory":     {ConfigDir: file},
	} {
		t.Run(name, func(t *testing.T) {
			if err := New(Options{LookPath: sandboxed, Config: h}).Prepare(context.Background(), workspace(t, true), task("", "", "")); err == nil {
				t.Error("Prepare succeeded")
			}
		})
	}
	owner := ownerConfigDir(t)
	for name, o := range map[string]Options{
		"no OS sandbox on the platform": {GOOS: "windows", LookPath: sandboxed},
		"no bubblewrap":                 {GOOS: "linux", LookPath: func(n string) (string, error) { return "", errors.New(n + " not found") }},
	} {
		t.Run(name, func(t *testing.T) {
			o.Config = config.Harness{ConfigDir: owner}
			err := New(o).Prepare(context.Background(), workspace(t, true), task("", "", ""))
			if err == nil || !strings.Contains(err.Error(), "never runs claude without it") {
				t.Errorf("Prepare = %v", err)
			}
		})
	}
	a := New(Options{LookPath: sandboxed, Config: config.Harness{ConfigDir: owner}})
	t.Run("a plugin without its manifest", func(t *testing.T) {
		ws := workspace(t, true)
		if err := os.Remove(filepath.Join(ws.Skills, "lisa", ".claude-plugin", "plugin.json")); err != nil {
			t.Fatal(err)
		}
		if err := a.Prepare(context.Background(), ws, task("", "", "")); err == nil || !strings.Contains(err.Error(), "plugin lisa") {
			t.Errorf("Prepare = %v", err)
		}
	})
	t.Run("a skill outside a plugin", func(t *testing.T) {
		ws := workspace(t, true)
		ws.SkillList = append(ws.SkillList, harness.Skill{Name: "loose", Dir: ws.Skills})
		if err := a.Prepare(context.Background(), ws, task("", "", "")); err == nil {
			t.Error("Prepare succeeded")
		}
	})
	t.Run("no scratch HOME", func(t *testing.T) {
		ws := workspace(t, true)
		ws.Home = ""
		if err := a.Prepare(context.Background(), ws, task("", "", "")); err == nil {
			t.Error("Prepare succeeded")
		}
	})
}

func TestPluginDirsAndRoots(t *testing.T) {
	ws := workspace(t, true)
	want := []string{filepath.Join(ws.Skills, "ccf-docs"), filepath.Join(ws.Skills, "ccf-review"), filepath.Join(ws.Skills, "lisa")}
	if got := pluginDirs(ws); !slices.Equal(got, want) {
		t.Errorf("plugin dirs %q", got)
	}
	ws.Mounts[1].Writable = true
	if got := writableRoots(ws); !slices.Equal(got, []string{ws.Out, ws.Tmp, ws.Repo, ws.Mounts[1].Dir}) {
		t.Errorf("writable roots %q", got)
	}
	ws.RepoWritable, ws.Mounts[0].Writable, ws.Mounts[1].Writable = false, false, false
	if got := writableRoots(ws); !slices.Equal(got, []string{ws.Out, ws.Tmp}) {
		t.Errorf("read-only roots %q", got)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// scanFiles fails if a regular file under dir holds a token.
func scanFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, s := range testSecrets {
			if strings.Contains(string(b), s) {
				t.Errorf("a token is on disk in %s", p)
			}
		}
		return nil
	})
}

func TestSandboxSupport(t *testing.T) {
	look := func(have ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			if slices.Contains(have, n) {
				return "/usr/bin/" + n, nil
			}
			return "", errors.New("not found")
		}
	}
	tests := []struct {
		goos string
		have []string
		want string
	}{
		{"darwin", []string{"sandbox-exec"}, ""},
		{"linux", []string{"bwrap", "socat"}, ""},
		{"linux", []string{"socat"}, "needs bwrap on PATH"},
		{"linux", nil, "needs bwrap and socat on PATH"},
		{"darwin", nil, "needs sandbox-exec"},
		{"windows", []string{"bwrap", "socat"}, "isn't supported on windows"},
	}
	for _, tt := range tests {
		err := New(Options{GOOS: tt.goos, LookPath: look(tt.have...)}).sandboxSupport()
		if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s with %v: %v, want %q", tt.goos, tt.have, err, tt.want)
		}
	}
}

func TestSandboxDeniedReads(t *testing.T) {
	ws := harness.Workspace{Root: "/Users/owner/.factory-agent/work/tsk_1"}
	got := sandboxDeniedReads(ws, "/Users/owner", "/Users/owner/.factory-agent/claude")
	if !slices.Contains(got, "/Users/owner/.ssh") || !slices.Contains(got, "/Users/owner/.factory-agent/claude") {
		t.Errorf("denied %q", got)
	}
	// A path that holds the workspace isn't denied; without a HOME only the config dir is.
	got = sandboxDeniedReads(harness.Workspace{Root: "/Users/owner/.ssh/work/tsk_1"}, "/Users/owner", "")
	if slices.Contains(got, "/Users/owner/.ssh") {
		t.Errorf("the workspace's own ancestor is denied: %q", got)
	}
	if got := sandboxDeniedReads(ws, "", "/cfg"); !slices.Equal(got, []string{"/cfg"}) {
		t.Errorf("no HOME: %q", got)
	}
}

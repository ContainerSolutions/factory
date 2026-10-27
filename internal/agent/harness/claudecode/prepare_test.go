package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The owner's login in tests: it must never be read, copied or written by the adapter.
const ownerLogin = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-owner-never-copied","refreshToken":"sk-ant-ort01-owner-never-copied"}}`

// tokens that must never reach a file the adapter writes or a command line.
var testSecrets = []string{"sk-ant-oat01-owner-never-copied", "sk-ant-ort01-owner-never-copied", "ghs_taskTokenNeverOnDisk0123456789",
	"agent-token-never-on-disk", "sk-ant-api03-worker-key"}

// ownerConfigDir is a signed-in dedicated CLAUDE_CONFIG_DIR (Linux layout: the login in a
// file), which the adapter must never read: the token scans find its tokens if it does.
func ownerConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".credentials.json": ownerLogin, ".claude.json": `{"hasCompletedOnboarding":true}`} {
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
	ws.Env = []string{"FACTORY_API=http://127.0.0.1:1", "FACTORY_GIT_CREDENTIAL_SOCKET=/tmp/fxgc-1/s", "FACTORY_RESULT=" + ws.Result, "FACTORY_TOKEN=agent-token-never-on-disk",
		"GH_TOKEN=ghs_taskTokenNeverOnDisk0123456789", "HOME=" + ws.Home, "PATH=" + ws.Bin + ":" + os.Getenv("PATH"), "TMPDIR=" + ws.Tmp}
	return ws
}

func task(model, effort, auth string) v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{
		Harness: &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{Name: Name, Model: model, ReasoningEffort: effort, Auth: auth}},
		Limits:  &v1alpha1.Limits{MaxTurns: 30, MaxCostUSD: 2.5},
	}}
}

func TestPrepareWithoutEffort(t *testing.T) {
	ws := vmWorkspace(t, true)
	if err := vmAdapter(t, 1000, "").Prepare(context.Background(), ws, task("", "", "")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(ws.Home, ".factory-claude", "settings.json"))
	if strings.Contains(string(b), "effortLevel") {
		t.Errorf("an effort without one asked:\n%s", b)
	}
}

func TestPrepareRefusals(t *testing.T) {
	a := vmAdapter(t, 1000, "")
	t.Run("a plugin without its manifest", func(t *testing.T) {
		ws := vmWorkspace(t, true)
		if err := os.Remove(filepath.Join(ws.Skills, "lisa", ".claude-plugin", "plugin.json")); err != nil {
			t.Fatal(err)
		}
		if err := a.Prepare(context.Background(), ws, task("", "", "")); err == nil || !strings.Contains(err.Error(), "plugin lisa") {
			t.Errorf("Prepare = %v", err)
		}
	})
	t.Run("a skill outside a plugin", func(t *testing.T) {
		ws := vmWorkspace(t, true)
		ws.SkillList = append(ws.SkillList, harness.Skill{Name: "loose", Dir: ws.Skills})
		if err := a.Prepare(context.Background(), ws, task("", "", "")); err == nil {
			t.Error("Prepare succeeded")
		}
	})
	t.Run("no scratch HOME", func(t *testing.T) {
		ws := vmWorkspace(t, true)
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

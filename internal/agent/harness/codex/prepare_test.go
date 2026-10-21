package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The owner's login in tests: it must never be read, copied or written by the adapter.
const ownerLogin = `{"tokens":{"access_token":"owner-access-token-never-copied","refresh_token":"owner-refresh-never-copied"}}`

// tokens that must never reach a file the adapter writes.
var testSecrets = []string{"owner-access-token-never-copied", "owner-refresh-never-copied", "ghs_taskTokenNeverOnDisk0123456789", "agent-token-never-on-disk"}

// ownerHome is a signed-in dedicated CODEX_HOME.
func ownerHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "codex-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(ownerLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// workspace is what the pipeline hands an adapter: its directories, a checkout, the skills in
// the plugin layout (two plugins with a "review" skill), the schema and the environment.
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
	for _, d := range []string{ws.Repo, ws.Skills, ws.Out, ws.Home, ws.Tmp, ws.Bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ws.Schema, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"ccf-docs/skills/docs-style/SKILL.md":         "---\nname: docs-style\n---\n",
		"ccf-docs/skills/docs-style/scripts/check.sh": "#!/bin/sh\necho ok\n",
		"ccf-review/skills/review/SKILL.md":           "---\nname: review\n---\nccf-review\n",
		"lisa/skills/review/SKILL.md":                 "---\nname: review\n---\nlisa\n",
	}
	for name, body := range files {
		p := filepath.Join(ws.Skills, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o700
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(ws.Skills, "ccf-docs/skills/docs-style/link")); err != nil {
		t.Fatal(err)
	}
	for _, s := range [][2]string{{"ccf-docs", "docs-style"}, {"ccf-review", "review"}, {"lisa", "review"}} {
		ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: s[0], Name: s[1], Dir: filepath.Join(ws.Skills, s[0], "skills", s[1])})
	}
	ws.Env = []string{"FACTORY_API=http://127.0.0.1:1", "FACTORY_RESULT=" + ws.Result, "FACTORY_TOKEN=agent-token-never-on-disk",
		"GH_TOKEN=ghs_taskTokenNeverOnDisk0123456789", "HOME=" + ws.Home, "PATH=" + ws.Bin + ":" + os.Getenv("PATH"), "TMPDIR=" + ws.Tmp}
	return ws
}

func task(model, effort string) v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{
		Harness: &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{Name: "codex", Model: model, ReasoningEffort: effort}}}}
}

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			rel += " -> " + target
		case info.IsDir():
			rel += "/"
		case info.Mode()&0o111 != 0:
			rel += " (x)"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPrepareLayout(t *testing.T) {
	owner := ownerHome(t)
	ws := workspace(t, true)
	a := New(Options{Config: config.Harness{Home: owner}})
	if err := a.Prepare(context.Background(), ws, task("gpt-5-codex", "high")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(ws.Home, ".codex")
	want := []string{
		"AGENTS.md",
		"auth.json -> " + filepath.Join(owner, "auth.json"),
		"config.toml",
		"skills/",
		"skills/ccf-review-review/",
		"skills/ccf-review-review/SKILL.md",
		"skills/docs-style/",
		"skills/docs-style/SKILL.md",
		"skills/docs-style/scripts/",
		"skills/docs-style/scripts/check.sh (x)",
		"skills/lisa-review/",
		"skills/lisa-review/SKILL.md",
	}
	if got := listTree(t, home); !slices.Equal(got, want) {
		t.Errorf("CODEX_HOME:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if b, _ := os.ReadFile(filepath.Join(home, "skills/lisa-review/SKILL.md")); !strings.Contains(string(b), "lisa") {
		t.Errorf("lisa-review is %q", b)
	}
	agents, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if !strings.Contains(string(agents), ws.Preamble) {
		t.Errorf("AGENTS.md lacks the preamble:\n%s", agents)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	for _, want := range []string{
		"check_for_update_on_startup = false",
		"[sandbox_workspace_write]\nnetwork_access = true\nwritable_roots = [\"" + ws.Out + "\", \"" + ws.Tmp + "\", \"" + filepath.Join(ws.Home, ".cache") + "\"]",
		"[shell_environment_policy]\ninherit = \"all\"\nignore_default_excludes = true",
		"[mcp_servers.factory]\ncommand = \"" + filepath.Join(ws.Bin, "factory") + "\"\nargs = [\"mcp\"]\n" +
			`env_vars = ["FACTORY_API", "FACTORY_RESULT", "FACTORY_TOKEN", "GH_TOKEN", "HOME", "PATH", "TMPDIR"]`,
	} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config.toml lacks %q:\n%s", want, cfg)
		}
	}
	scanFiles(t, ws.Root)
	// The owner's login is untouched.
	if b, _ := os.ReadFile(filepath.Join(owner, "auth.json")); string(b) != ownerLogin {
		t.Errorf("the owner's login changed: %q", b)
	}
	// Removing the workspace removes the link, not the owner's login.
	if err := os.RemoveAll(ws.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(owner, "auth.json")); err != nil {
		t.Errorf("the owner's login is gone with the workspace: %v", err)
	}
}

// scanFiles fails if a regular file under dir holds a token (the owner's login is reached
// only through the symlink, which isn't followed).
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

func TestPrepareRefusals(t *testing.T) {
	ws := workspace(t, true)
	for name, h := range map[string]config.Harness{
		"no home":       {},
		"relative home": {Home: "codex-home"},
		"not signed in": {Home: t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			if err := New(Options{Config: h}).Prepare(context.Background(), ws, task("", "")); err == nil {
				t.Error("Prepare succeeded")
			}
		})
	}
	// Two skills that would land in the same folder.
	ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: "", Name: "docs-style", Dir: ws.SkillList[0].Dir})
	ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: "", Name: "docs-style", Dir: ws.SkillList[0].Dir})
	if err := New(Options{Config: config.Harness{Home: ownerHome(t)}}).Prepare(context.Background(), ws, task("", "")); err == nil {
		t.Error("clashing skill folders: Prepare succeeded")
	}
}

// TestConfigGitDirs: config.toml's writable_roots list the git dir of every writable
// checkout (codex keeps .git read-only otherwise), and never a read-only checkout's.
func TestConfigGitDirs(t *testing.T) {
	mkdir := func(t *testing.T, dirs ...string) {
		t.Helper()
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	write := func(t *testing.T, p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		// setup turns the workspace into the case and returns the git dirs config.toml lists.
		setup func(t *testing.T, ws *harness.Workspace) []string
	}{
		"rw checkout": {func(t *testing.T, ws *harness.Workspace) []string {
			mkdir(t, filepath.Join(ws.Repo, ".git"))
			ws.Mounts = []harness.Mount{{Repo: "o/r", Dir: ws.Repo, Writable: true}}
			return []string{filepath.Join(ws.Repo, ".git")}
		}},
		"ro checkout": {func(t *testing.T, ws *harness.Workspace) []string {
			mkdir(t, filepath.Join(ws.Repo, ".git"))
			ws.RepoWritable = false
			ws.Mounts = []harness.Mount{{Repo: "o/r", Dir: ws.Repo}}
			return nil
		}},
		"rw, ro and rw mounts": {func(t *testing.T, ws *harness.Workspace) []string {
			ro, rw := filepath.Join(ws.Root, "repos", "ro"), filepath.Join(ws.Root, "repos", "rw")
			mkdir(t, filepath.Join(ws.Repo, ".git"), filepath.Join(ro, ".git"), filepath.Join(rw, ".git"))
			ws.Mounts = []harness.Mount{{Repo: "o/r", Dir: ws.Repo, Writable: true}, {Repo: "o/ro", Dir: ro}, {Repo: "o/rw", Dir: rw, Writable: true}}
			return []string{filepath.Join(ws.Repo, ".git"), filepath.Join(rw, ".git")}
		}},
		"relative gitdir pointer": {func(t *testing.T, ws *harness.Workspace) []string {
			gd := filepath.Join(ws.Root, "gitdirs", "repo")
			mkdir(t, gd)
			write(t, filepath.Join(ws.Repo, ".git"), "gitdir: ../gitdirs/repo\n")
			return []string{gd}
		}},
		"absolute gitdir pointer": {func(t *testing.T, ws *harness.Workspace) []string {
			gd := filepath.Join(ws.Root, "gitdirs", "repo")
			mkdir(t, gd)
			write(t, filepath.Join(ws.Repo, ".git"), "gitdir: "+gd+"\n")
			return []string{gd}
		}},
		"pointer to a missing gitdir": {func(t *testing.T, ws *harness.Workspace) []string {
			write(t, filepath.Join(ws.Repo, ".git"), "gitdir: ../gone\n")
			return nil
		}},
		"not a pointer": {func(t *testing.T, ws *harness.Workspace) []string {
			mkdir(t, filepath.Join(ws.Root, "gitdirs"))
			write(t, filepath.Join(ws.Repo, ".git"), "worktree: ../gitdirs\n")
			return nil
		}},
		"symlinked .git": {func(t *testing.T, ws *harness.Workspace) []string {
			target := filepath.Join(ws.Root, "real.git")
			mkdir(t, target)
			if err := os.Symlink(target, filepath.Join(ws.Repo, ".git")); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		"rw checkout without .git": {func(t *testing.T, ws *harness.Workspace) []string {
			return nil
		}},
		"no checkout": {func(t *testing.T, ws *harness.Workspace) []string {
			ws.Repo, ws.RepoWritable, ws.WorkDir = "", false, ws.Root
			return nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ws := workspace(t, true)
			cache := filepath.Join(ws.Home, ".cache")
			want := append([]string{ws.Out, ws.Tmp, cache}, tc.setup(t, &ws)...)
			cfg := renderConfig(ws, cache)
			line := "writable_roots = " + tomlList(want) + "\n"
			if !strings.Contains(cfg, line) {
				t.Errorf("config.toml lacks %q:\n%s", line, cfg)
			}
		})
	}
}

package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// The owner's login in tests: it must never be read, copied or written by the adapter.
const ownerLogin = `{"tokens":{"access_token":"owner-access-token-never-copied","refresh_token":"owner-refresh-never-copied"}}`

// tokens that must never reach a file the adapter writes.
var testSecrets = []string{"owner-access-token-never-copied", "owner-refresh-never-copied", "ghs_taskTokenNeverOnDisk0123456789", "agent-token-never-on-disk"}

// ownerHome is a directory holding a real-looking login, which an auth.json link could name.
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

func TestPreparePluginBins(t *testing.T) {
	ws := vmWorkspace(t, true)
	for name, body := range map[string]string{
		"lisa/skills/lisa/SKILL.md": "---\nname: lisa\n---\n",
		"ccf-review/bin/ccfr":       "#!/bin/sh\necho ccfr\n",
	} {
		p := filepath.Join(ws.Skills, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: "lisa", Name: "lisa", Dir: filepath.Join(ws.Skills, "lisa", "skills", "lisa")})
	if err := vmAdapter(t, "").Prepare(context.Background(), ws, task("", "")); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(ws.Home, ".codex", "skills")
	ccfr := filepath.Join(skills, "lisa", "..", "ccf-review", "bin", "ccfr")
	real, err := filepath.EvalSymlinks(ccfr)
	if err != nil {
		t.Fatalf("lisa's ccfr: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(ws.Skills, "ccf-review", "bin", "ccfr"))
	if st, err := os.Stat(ccfr); real != want || err != nil || st.Mode()&0o100 == 0 {
		t.Errorf("lisa's ccfr is %s (%v, %v), want the executable %s", real, st, err, want)
	}
	for _, p := range []string{"ccf-docs", "lisa/bin"} {
		if _, err := os.Lstat(filepath.Join(skills, p)); err == nil {
			t.Errorf("%s is in CODEX_HOME/skills for a plugin without bin/", p)
		}
	}
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

// TestPrepareSkillClash: two skills that would land in the same folder are refused.
func TestPrepareSkillClash(t *testing.T) {
	ws := vmWorkspace(t, true)
	ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: "", Name: "docs-style", Dir: ws.SkillList[0].Dir})
	ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: "", Name: "docs-style", Dir: ws.SkillList[0].Dir})
	if err := vmAdapter(t, "").Prepare(context.Background(), ws, task("", "")); err == nil {
		t.Error("clashing skill folders: Prepare succeeded")
	}
}

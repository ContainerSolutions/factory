package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

// testSecrets are values of the run's environment that must never reach a file the adapter
// writes: a task token, the agent token and a task variable.
var testSecrets = []string{"ghs_taskTokenNeverOnDisk0123456789", "agent-token-never-on-disk", "task-variable-never-on-disk"}

// workspace is what a VM workspace's pipeline hands the adapter: its directories, a checkout,
// the skills in the plugin layout (two plugins with a "review" skill), the schema, and the
// gateway-mode environment with the secrets above.
func workspace(t *testing.T, writable bool) harness.Workspace {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tsk_1")
	ws := harness.Workspace{
		Root: root, Repo: filepath.Join(root, "repo"), RepoWritable: writable, Skills: filepath.Join(root, "skills"),
		Out: filepath.Join(root, "out"), Home: filepath.Join(root, "home"), Bin: filepath.Join(root, "bin"),
		Schema: filepath.Join(root, "result.schema.json"), Preamble: "You are running unattended inside CCF Factory (task tsk_1).",
	}
	ws.WorkDir, ws.Result = ws.Repo, filepath.Join(ws.Out, "result.json")
	for _, s := range [][2]string{{"ccf-docs", "docs-style"}, {"ccf-review", "review"}, {"lisa", "review"}} {
		dir := filepath.Join(ws.Skills, s[0], "skills", s[1])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+s[1]+"\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ws.SkillList = append(ws.SkillList, harness.Skill{Plugin: s[0], Name: s[1], Dir: dir})
	}
	ws.Env = []string{"FACTORY_RESULT=" + ws.Result, "FACTORY_TOKEN=agent-token-never-on-disk", "GH_TOKEN=ghs_taskTokenNeverOnDisk0123456789",
		"HOME=" + ws.Home, "HTTP_PROXY=http://192.168.64.1:3128", "TASK_VARIABLE=task-variable-never-on-disk",
		"XDG_CONFIG_HOME=" + filepath.Join(ws.Home, ".config")}
	return ws
}

// adapter has the served model's limits and the default model def. Its binary doesn't exist,
// never the opencode on PATH: Prepare runs nothing.
func adapter(t *testing.T, def string) *Adapter {
	t.Helper()
	return New(Options{Binary: filepath.Join(t.TempDir(), "no-opencode"), Config: config.Harness{Default: def},
		ContextWindow: 131072, MaxOutput: 16384})
}

func task(model, effort, auth string) v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{Harness: &v1alpha1.TaskHarness{
		Resolved: &v1alpha1.HarnessResolved{Name: Name, Model: model, ReasoningEffort: effort, Auth: auth}}}}
}

// TestPrepare pins what Prepare writes, a golden per case (read-write with an effort,
// read-only, the config's default model without an effort, no skills): opencode.json and the
// instructions, new 0600 files, and the skills flattened beside the config. The instructions
// give each skill's <plugin>:<skill> the name opencode knows it by, the name in its flat
// SKILL.md (two plugins' "review" are ccf-review-review and lisa-review). The config's
// only key is the placeholder, its MCP server names no variable, every permission is allow or
// deny, and no value of the run's environment is in any file.
func TestPrepare(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name               string
		writable, noSkills bool
		model, effort, def string
	}{
		{"read-write", true, false, "Qwen/Qwen3-Coder-Next-FP8", "max", ""},
		{"read-only", false, false, "qwen3-coder", "minimal", ""},
		{"default-model", true, false, "", "", "qwen3-coder"},
		{"no-skills", true, true, "qwen3-coder", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := workspace(t, tt.writable)
			want := []string{"opencode.json", "skills/", "skills/ccf-review-review/", "skills/ccf-review-review/SKILL.md",
				"skills/docs-style/", "skills/docs-style/SKILL.md", "skills/lisa-review/", "skills/lisa-review/SKILL.md"}
			if tt.noSkills {
				ws.SkillList, want = nil, []string{"opencode.json", "skills/"}
			}
			if err := adapter(t, tt.def).Prepare(context.Background(), ws, task(tt.model, tt.effort, AuthSelfHosted)); err != nil {
				t.Fatal(err)
			}
			cfg, own := filepath.Join(ws.Home, ".config", "opencode"), filepath.Join(ws.Home, ".factory-opencode")
			if got := listTree(t, cfg); !slices.Equal(got, want) || !slices.Equal(listTree(t, own), []string{"instructions.md"}) {
				t.Errorf("opencode's config dir:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			for _, p := range want {
				if dir, ok := strings.CutSuffix(p, "/SKILL.md"); ok {
					if b, err := os.ReadFile(filepath.Join(cfg, p)); err != nil || !strings.Contains(string(b), "\nname: "+filepath.Base(dir)+"\n") {
						t.Errorf("%s doesn't name the skill %s: %q %v", p, filepath.Base(dir), b, err)
					}
				}
			}
			b := readNew(t, filepath.Join(cfg, "opencode.json"))
			golden(t, ws, "config-"+tt.name+".golden.json", b)
			golden(t, ws, "instructions-"+tt.name+".golden.md", readNew(t, filepath.Join(own, "instructions.md")))
			checkConfig(t, b)
			scanFiles(t, ws.Root)
		})
	}
}

// readNew reads the file at p, which must be a regular 0600 file.
func readNew(t *testing.T, p string) []byte {
	t.Helper()
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v %v", p, fi, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkConfig checks opencode.json's meaning, so -update can't bless a regression: the
// placeholder is the provider's key, once; the MCP server names no variable; and no
// permission asks.
func checkConfig(t *testing.T, b []byte) {
	t.Helper()
	var doc struct {
		Provider map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"provider"`
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Provider["factory"].Options["apiKey"] != harness.Placeholder || bytes.Count(b, []byte(harness.Placeholder)) != 1 {
		t.Errorf("the placeholder isn't the one key:\n%s", b)
	}
	if _, env := doc.MCP["factory"]["environment"]; env || doc.MCP["factory"]["type"] != "local" || bytes.Contains(b, []byte(`"ask"`)) {
		t.Errorf("mcp.factory %v, or a permission asks:\n%s", doc.MCP["factory"], b)
	}
}

// TestRenderConfig: the provider's proxy is written only when given (setProviderProxy is off
// for the pinned release).
func TestRenderConfig(t *testing.T) {
	t.Parallel()
	ws := workspace(t, true)
	for _, proxy := range []string{"", "http://192.168.64.1:3128"} {
		b, err := renderConfig(ws, configInput{model: "m", proxy: proxy, instructions: "/i.md", contextWindow: 8, maxOutput: 4})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(b), `"proxy": "`+proxy+`"`); got != (proxy != "") || strings.Contains(string(b), `"proxy"`) != got {
			t.Errorf("proxy %q:\n%s", proxy, b)
		}
	}
}

// TestPrepareRefusals: a run without a model, without the served model's limits, on another
// auth or without a HOME is refused, and so is an opencode.json already there (a file, or a
// link to one elsewhere), which is never written over.
func TestPrepareRefusals(t *testing.T) {
	t.Parallel()
	planted := func(mk func(t *testing.T, p string) error) func(*testing.T, *Adapter, *harness.Workspace) {
		return func(t *testing.T, _ *Adapter, ws *harness.Workspace) {
			dir := filepath.Join(ws.Home, ".config", "opencode")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := mk(t, filepath.Join(dir, "opencode.json")); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tt := range []struct {
		name, model, auth string
		change            func(t *testing.T, a *Adapter, ws *harness.Workspace)
		want              string
	}{
		{"no model", "", "", nil, "names no model"},
		{"no limits", "m", "", func(_ *testing.T, a *Adapter, _ *harness.Workspace) { a.o.MaxOutput = 0 }, "maximum output aren't set"},
		{"subscription", "m", "subscription", nil, `unknown auth "subscription"`},
		{"no HOME", "m", "", func(_ *testing.T, _ *Adapter, ws *harness.Workspace) { ws.Home = "" }, "no scratch HOME"},
		{"existing file", "m", "", planted(func(_ *testing.T, p string) error { return os.WriteFile(p, []byte("{}"), 0o600) }), "the config"},
		{"existing symlink", "m", "", planted(func(t *testing.T, p string) error { return os.Symlink(filepath.Join(t.TempDir(), "x.json"), p) }), "the config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ws := adapter(t, ""), workspace(t, true)
			if tt.change != nil {
				tt.change(t, a, &ws)
			}
			if err := a.Prepare(context.Background(), ws, task(tt.model, "", tt.auth)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Prepare = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestRunEnv: the run's environment is the workspace's with OPENCODE_CONFIG naming the
// generated config and the switches on; XDG_DATA_HOME stays the system's.
func TestRunEnv(t *testing.T) {
	t.Parallel()
	ws := workspace(t, true)
	env := runEnv(ws)
	want := map[string]string{
		"OPENCODE_CONFIG": filepath.Join(ws.Home, ".config", "opencode", "opencode.json"), "OPENCODE_DISABLE_AUTOUPDATE": "1",
		"OPENCODE_DISABLE_MODELS_FETCH": "1", "OPENCODE_DISABLE_LSP_DOWNLOAD": "1", "OPENCODE_DISABLE_CLAUDE_CODE": "1",
		"HTTP_PROXY": "http://192.168.64.1:3128",
	}
	for k, v := range want {
		if got := lookupEnv(env, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(env) != len(ws.Env)+5 || lookupEnv(env, "XDG_DATA_HOME") != "" {
		t.Errorf("env %q", env)
	}
}

// golden compares got, with the workspace root replaced by $ROOT, against testdata/<name>
// (-update rewrites it).
func golden(t *testing.T, ws harness.Workspace, name string, got []byte) {
	t.Helper()
	got = bytes.ReplaceAll(got, []byte(ws.Root), []byte("$ROOT"))
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want, err := os.ReadFile(p); err != nil || !bytes.Equal(got, want) {
		t.Errorf("%s differs (%v; -update rewrites it):\n%s", p, err, got)
	}
}

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// scanFiles fails if a regular file under dir holds a test secret.
func scanFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, s := range testSecrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("a value of the run's environment is on disk in %s", p)
			}
		}
		return nil
	})
}

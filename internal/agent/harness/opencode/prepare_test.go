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

// testSecrets are values in the run's environment that must never reach a file the adapter
// writes: a task token, the agent token and a task variable.
var testSecrets = []string{"ghs_taskTokenNeverOnDisk0123456789", "agent-token-never-on-disk", "task-variable-never-on-disk"}

// workspace is what a VM workspace's pipeline hands the adapter: its directories, a checkout,
// the skills in the plugin layout (two plugins with a "review" skill), the schema and the
// gateway-mode environment, plus the secrets above.
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
	for _, d := range []string{ws.Repo, ws.Out, ws.Home, ws.Tmp, ws.Bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"ccf-docs/skills/docs-style/SKILL.md": "---\nname: docs-style\n---\n",
		"ccf-review/skills/review/SKILL.md":   "---\nname: review\n---\n",
		"lisa/skills/review/SKILL.md":         "---\nname: review\n---\n",
	} {
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
	ws.Env = []string{"FACTORY_API=http://dispatcher.factory.internal", "FACTORY_RESULT=" + ws.Result, "FACTORY_RUNTIME=workspace",
		"FACTORY_TOKEN=agent-token-never-on-disk", "GH_TOKEN=ghs_taskTokenNeverOnDisk0123456789", "HOME=" + ws.Home,
		"HTTP_PROXY=http://192.168.64.1:3128", "HTTPS_PROXY=http://192.168.64.1:3128", "PATH=" + ws.Bin + ":/usr/bin",
		"TASK_VARIABLE=task-variable-never-on-disk", "TMPDIR=" + ws.Tmp, "XDG_CONFIG_HOME=" + filepath.Join(ws.Home, ".config")}
	return ws
}

// adapter is the adapter with the served model's limits and default model def. Its binary
// doesn't exist, never the opencode on PATH: Prepare runs nothing.
func adapter(t *testing.T, def string) *Adapter {
	t.Helper()
	return New(Options{
		Binary: filepath.Join(t.TempDir(), "no-opencode"), Config: config.Harness{Models: []string{"qwen3-coder"}, Default: def},
		ContextWindow: 131072, MaxOutput: 16384,
	})
}

func task(model, effort, auth string) v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{Harness: &v1alpha1.TaskHarness{
		Resolved: &v1alpha1.HarnessResolved{Name: Name, Model: model, ReasoningEffort: effort, Auth: auth}}}}
}

// golden compares got, with the workspace root replaced by $ROOT, against testdata/<name>
// (-update rewrites it).
func golden(t *testing.T, ws harness.Workspace, name string, got []byte) {
	t.Helper()
	got = bytes.ReplaceAll(got, []byte(ws.Root), []byte("$ROOT"))
	p := filepath.Join("testdata", name)
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

// TestPrepare pins what Prepare writes: opencode.json (a golden per case: read-write with an
// effort, read-only, and the config's default model without an effort) and the instructions,
// both new 0600 files; the skills flattened beside the config. The config's only key is the
// placeholder, its permissions are all allow or deny, and no value of the run's environment
// is in any file.
func TestPrepare(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		writable      bool
		model, effort string
		def           string
	}{
		{"read-write", true, "Qwen/Qwen3-Coder-Next-FP8", "max", ""},
		{"read-only", false, "qwen3-coder", "minimal", ""},
		{"default-model", true, "", "", "qwen3-coder"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := workspace(t, tt.writable)
			if err := adapter(t, tt.def).Prepare(context.Background(), ws, task(tt.model, tt.effort, AuthSelfHosted)); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(ws.Home, ".config", "opencode")
			want := []string{
				"opencode.json", "skills/",
				"skills/ccf-review-review/", "skills/ccf-review-review/SKILL.md",
				"skills/docs-style/", "skills/docs-style/SKILL.md",
				"skills/lisa-review/", "skills/lisa-review/SKILL.md",
			}
			if got := listTree(t, cfg); !slices.Equal(got, want) {
				t.Errorf("opencode's config dir:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			if got := listTree(t, filepath.Join(ws.Home, ".factory-opencode")); !slices.Equal(got, []string{"instructions.md"}) {
				t.Errorf("the adapter's dir: %q", got)
			}
			read := func(p string) []byte {
				t.Helper()
				fi, err := os.Lstat(p)
				if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
					t.Errorf("%s: %v %v", p, fi, err)
				}
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			b := read(filepath.Join(cfg, "opencode.json"))
			golden(t, ws, "config-"+tt.name+".golden.json", b)
			if tt.name != "default-model" {
				golden(t, ws, "instructions-"+tt.name+".golden.md", read(filepath.Join(ws.Home, ".factory-opencode", "instructions.md")))
			}
			checkConfig(t, b)
			scanFiles(t, ws.Root)
		})
	}
}

// checkConfig checks opencode.json's meaning, so -update can't bless a regression: the
// placeholder is the provider's key and nothing else looks like one, the MCP server names no
// variable, and every permission is allow or deny.
func checkConfig(t *testing.T, b []byte) {
	t.Helper()
	var doc struct {
		Provider map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"provider"`
		MCP        map[string]map[string]any `json:"mcp"`
		Permission map[string]any            `json:"permission"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Provider["factory"].Options["apiKey"]; got != harness.Placeholder {
		t.Errorf("provider.factory.options.apiKey = %v, want the placeholder", got)
	}
	if n := bytes.Count(b, []byte(harness.Placeholder)); n != 1 {
		t.Errorf("the placeholder is in opencode.json %d times", n)
	}
	if _, ok := doc.MCP["factory"]["environment"]; ok {
		t.Errorf("mcp.factory names variables: %v", doc.MCP["factory"])
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				walk(path+"."+k, e)
			}
		default:
			if v != "allow" && v != "deny" {
				t.Errorf("permission%s = %v, want allow or deny", path, v)
			}
		}
	}
	walk("", doc.Permission)
}

// TestRenderConfig: the effort is the model's reasoningEffort and the proxy the provider's
// proxy, each only when given (Prepare gives them as sendReasoningEffort and setProviderProxy
// say).
func TestRenderConfig(t *testing.T) {
	t.Parallel()
	ws := workspace(t, true)
	for _, tt := range []struct {
		name          string
		effort, proxy string
	}{
		{"neither", "", ""},
		{"effort", "high", ""},
		{"proxy", "", "http://192.168.64.1:3128"},
	} {
		in := configInput{model: "m", effort: tt.effort, proxy: tt.proxy, instructions: "/i.md", contextWindow: 8, maxOutput: 4}
		b, err := renderConfig(ws, in)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Provider map[string]providerDoc `json:"provider"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		p := doc.Provider["factory"]
		var effort string
		if o := p.Models["m"].Options; o != nil {
			effort = o.ReasoningEffort
		}
		if effort != tt.effort || p.Options.Proxy != tt.proxy || strings.Contains(string(b), `"proxy"`) != (tt.proxy != "") {
			t.Errorf("%s: reasoningEffort %q, proxy %q:\n%s", tt.name, effort, p.Options.Proxy, b)
		}
	}
}

// TestPrepareRefusals: a run without a model, without the served model's limits, on another
// auth or without a HOME is refused, and so is an opencode.json already there (a file, or a
// link to one elsewhere), which is never written over.
func TestPrepareRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		model  string
		auth   string
		change func(t *testing.T, a *Adapter, ws *harness.Workspace)
		want   string
	}{
		{"no model", "", "", nil, "names no model"},
		{"no limits", "m", "", func(_ *testing.T, a *Adapter, _ *harness.Workspace) { a.o.MaxOutput = 0 }, "context window and maximum output"},
		{"subscription", "m", "subscription", nil, `unknown auth "subscription"`},
		{"no HOME", "m", "", func(_ *testing.T, _ *Adapter, ws *harness.Workspace) { ws.Home = "" }, "no scratch HOME"},
		{"existing file", "m", "", func(t *testing.T, _ *Adapter, ws *harness.Workspace) {
			plant(t, ws, func(p string) error { return os.WriteFile(p, []byte(`{"model":"other/x"}`), 0o600) })
		}, "the config"},
		{"existing symlink", "m", "", func(t *testing.T, _ *Adapter, ws *harness.Workspace) {
			plant(t, ws, func(p string) error { return os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), p) })
		}, "the config"},
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

// plant makes opencode's config dir and puts something at opencode.json with mk.
func plant(t *testing.T, ws *harness.Workspace, mk func(p string) error) {
	t.Helper()
	dir := filepath.Join(ws.Home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mk(filepath.Join(dir, "opencode.json")); err != nil {
		t.Fatal(err)
	}
}

// TestRunEnv: the run's environment is the workspace's with OPENCODE_CONFIG naming the
// generated config and the switches on; XDG_DATA_HOME stays the system's.
func TestRunEnv(t *testing.T) {
	t.Parallel()
	ws := workspace(t, true)
	env := runEnv(ws)
	want := map[string]string{
		"OPENCODE_CONFIG":               filepath.Join(ws.Home, ".config", "opencode", "opencode.json"),
		"OPENCODE_DISABLE_AUTOUPDATE":   "1",
		"OPENCODE_DISABLE_MODELS_FETCH": "1",
		"OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
		"OPENCODE_DISABLE_CLAUDE_CODE":  "1",
		"HTTP_PROXY":                    "http://192.168.64.1:3128",
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
			if strings.Contains(string(b), s) {
				t.Errorf("a value of the run's environment is on disk in %s", p)
			}
		}
		return nil
	})
}

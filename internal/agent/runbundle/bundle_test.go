package runbundle

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// t0 is the tests' fixed time.
var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// fake token shapes, built so no literal token sits in the source.
var (
	ghsToken = "ghs" + "_" + strings.Repeat("A", 36)
	rtkToken = "rtk" + "_" + strings.Repeat("b", 43)
	// jwt is three base64url segments, the first two JSON objects.
	jwt = "eyJ" + "hbGciOiJSUzI1NiJ9" + "." + "eyJ" + "zdWIiOiJ1c2VyLTEifQ" + "." + strings.Repeat("c", 40)
)

func testTask(harness string) v1alpha1.Task {
	return v1alpha1.Task{
		Metadata: v1alpha1.TaskMeta{ID: "tsk_1", WorkItem: "wi_1"},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: v1alpha1.TaskSpecRef{Name: "implement"},
			Queue:    "default",
			Harness:  &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{Name: harness}},
			Prompt:   &v1alpha1.PromptRef{Ref: "prompt"},
			Context: []v1alpha1.ContextItem{
				{Kind: "issue", Ref: "acme/app#1", Trust: "untrusted"},
				{Kind: "repo", Ref: "acme/app@main"},
				{Kind: "issue", Ref: "acme/app#2"},
			},
		},
	}
}

func testManifest() Manifest {
	return Manifest{
		RunID: "run_1-1", LeaseID: "lse_1", TaskID: "tsk_1", StartedAt: t0,
		Gateway: Gateway{NoProxy: []string{"registry.example.com", "10.0.0.0/8"}},
		Git: GitAccess{
			Provider: "github", Host: "github.com", Repo: "acme/app",
			Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
			Reads:       []GitRead{{Repo: "acme/lib", ReadRepos: []string{"acme/lib"}}},
		},
		Skills: []Skill{{Name: "lisa", Plugin: "ccf", Dir: "skills/ccf/skills/lisa"}},
	}
}

func contextJSON(t *testing.T, i int, content string) []byte {
	t.Helper()
	b, err := json.Marshal(apiclient.TaskContext{Index: i, Kind: "issue", Ref: fmt.Sprintf("acme/app#%d", i+1), Trust: "untrusted", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// skillsTree is a materialised plugin dir: a skill and an executable script.
func skillsTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, mode := range map[string]os.FileMode{
		"ccf/.claude-plugin/plugin.json": 0o600,
		"ccf/skills/lisa/SKILL.md":       0o600,
		"ccf/skills/lisa/bin/lisa":       0o700,
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testFiles(t *testing.T) Files {
	return Files{
		Task:         testTask("codex"),
		Prompt:       []byte("Implement the issue.\n"),
		Context:      map[int][]byte{0: contextJSON(t, 0, "the issue's body")},
		OutputSchema: []byte(`{"type":"object"}`),
		SkillsDir:    skillsTree(t),
		CA:           []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
		Codex:        &CodexAccount{AccountID: "acct-1", UserID: "user-1", PlanType: "pro"},
	}
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs (run with -update):\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

// listing is every path under dir with its type and permissions.
func listing(t *testing.T, dir string) []byte {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %s\n", info.Mode(), filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(b.String())
}

func writeBundle(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	if err := Write(dir, testManifest(), testFiles(t)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return dir
}

func TestWriteRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(dir+".tmp/stale", 0o700); err != nil { // a leftover temp dir
		t.Fatal(err)
	}
	f := testFiles(t)
	if err := Write(dir, testManifest(), f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temp dir is left: %v", err)
	}
	checkGolden(t, "bundle.files.golden", listing(t, dir))
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "bundle.json.golden", append(raw, '\n'))

	b, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if b.TaskID() != "tsk_1" || !reflect.DeepEqual(b.Task, f.Task) {
		t.Errorf("Task = %+v", b.Task)
	}
	if string(b.Prompt) != string(f.Prompt) || string(b.OutputSchema) != string(f.OutputSchema) {
		t.Errorf("Prompt %q, OutputSchema %q", b.Prompt, b.OutputSchema)
	}
	if b.CAFile != filepath.Join(dir, "gateway", "ca.pem") {
		t.Errorf("CAFile = %q", b.CAFile)
	}
	if got := b.Skills(); len(got) != 1 || got[0].Dir != filepath.Join(dir, "skills", "ccf", "skills", "lisa") {
		t.Errorf("Skills() = %+v", got)
	}
	if c, gone, err := b.ContextItem(0); err != nil || gone || c.Content != "the issue's body" {
		t.Errorf("ContextItem(0) = %+v, %v, %v", c, gone, err)
	}
	if _, gone, err := b.ContextItem(2); err != nil || !gone {
		t.Errorf("ContextItem(2) = gone %v, %v; want gone", gone, err)
	}
	if _, _, err := b.ContextItem(1); err == nil {
		t.Error("ContextItem(1), a repo item: no error")
	}
	if b.Manifest.Version != Version || b.Manifest.Dispatcher != DefaultDispatcherURL || *b.Manifest.Codex != *f.Codex {
		t.Errorf("Manifest = %+v", b.Manifest)
	}

	// A relative dir still gives absolute paths.
	t.Chdir(filepath.Dir(dir))
	if rb, err := Read("bundle"); err != nil || rb.CAFile != b.CAFile || rb.Skills()[0].Dir != b.Skills()[0].Dir {
		t.Errorf("Read(relative) = %+v, %v", rb, err)
	}

	// Without a schema, a CA or codex.
	dir2 := filepath.Join(t.TempDir(), "bundle")
	f.Task, f.OutputSchema, f.CA, f.Codex = testTask("claude-code"), nil, nil, nil
	if err := Write(dir2, testManifest(), f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if b, err = Read(dir2); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if b.OutputSchema != nil || b.CAFile != "" || b.Manifest.Codex != nil || b.Manifest.OutputSchema != "" {
		t.Errorf("Bundle = %+v", b)
	}
}

func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	if !strings.HasPrefix(ve.Field, field) {
		t.Errorf("Field = %q (%v), want %q", ve.Field, err, field)
	}
	if strings.Contains(err.Error(), ghsToken) || strings.Contains(err.Error(), rtkToken[4:]) {
		t.Errorf("the error prints the token: %v", err)
	}
}

func TestWriteRefuses(t *testing.T) {
	big := func(n int) []byte { return []byte(strings.Repeat("a", n)) }
	tests := []struct {
		name  string
		edit  func(*Manifest, *Files)
		field string
	}{
		{"an oversize prompt", func(_ *Manifest, f *Files) { f.Prompt = big(MaxPromptBytes + 1) }, PromptFile},
		{"an oversize schema", func(_ *Manifest, f *Files) { f.OutputSchema = big(MaxSchemaBytes + 1) }, SchemaFile},
		{"an oversize CA", func(_ *Manifest, f *Files) { f.CA = big(MaxCABytes + 1) }, CAFile},
		{"an oversize manifest", func(m *Manifest, _ *Files) {
			m.Gateway.NoProxy = slices.Repeat([]string{"registry.example.com"}, 50000)
		}, ManifestFile},
		{"an oversize task", func(_ *Manifest, f *Files) { f.Task.Spec.Inputs = map[string]any{"x": string(big(MaxTaskBytes))} }, TaskFile},
		{"an oversize context item", func(_ *Manifest, f *Files) {
			f.Context[0] = contextJSON(t, 0, string(big(MaxContextBytes+1)))
		}, "context/0.json"},
		{"an oversize context file", func(_ *Manifest, f *Files) { f.Context[0] = big(MaxContextFile + 1) }, "context/0.json"},
		{"a context file for another item", func(_ *Manifest, f *Files) { f.Context[0] = contextJSON(t, 2, "x") }, "context/0.json"},
		{"a context file that isn't one", func(_ *Manifest, f *Files) { f.Context[0] = []byte("[]") }, "context/0.json"},
		{"a repo item's context", func(_ *Manifest, f *Files) { f.Context[1] = contextJSON(t, 1, "x") }, "context"},
		{"an item the task hasn't", func(_ *Manifest, f *Files) { f.Context[7] = contextJSON(t, 7, "x") }, "context"},
		{"a token in the git access", func(m *Manifest, _ *Files) { m.Git.Permissions["contents"] = ghsToken }, "git.permissions"},
		{"a token in task.json", func(_ *Manifest, f *Files) { f.Task.Spec.Inputs = map[string]any{"x": []any{"a", ghsToken}} }, TaskFile + ".spec.inputs"},
		{"codex for claude", func(_ *Manifest, f *Files) { f.Task = testTask("claude-code") }, "codex"},
		{"no codex account for codex", func(_ *Manifest, f *Files) { f.Codex = nil }, "codex"},
		{"skills without a dir", func(_ *Manifest, f *Files) { f.SkillsDir = "" }, "skills"},
		{"a skill not in the dir", func(m *Manifest, _ *Files) {
			m.Skills = append(m.Skills, Skill{Name: "gone", Plugin: "ccf", Dir: "skills/ccf/skills/gone"})
		}, "skills[1].dir"},
		{"another task", func(m *Manifest, _ *Files) { m.TaskID = "tsk_2" }, "taskId"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, f := testManifest(), testFiles(t)
			tt.edit(&m, &f)
			dir := filepath.Join(t.TempDir(), "bundle")
			wantInvalid(t, Write(dir, m, f), tt.field)
			for _, p := range []string{dir, dir + ".tmp"} {
				if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s is left: %v", p, err)
				}
			}
		})
	}

	t.Run("an existing dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := Write(dir, testManifest(), testFiles(t)); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a symlink in the skills", func(t *testing.T) {
		f := testFiles(t)
		if err := os.Symlink("/etc/passwd", filepath.Join(f.SkillsDir, "ccf", "skills", "lisa", "link")); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "bundle")
		if err := Write(dir, testManifest(), f); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(dir + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the temp dir is left: %v", err)
		}
	})
}

// A context item's content is untrusted provider text: a token-shaped example in an issue
// body is kept, not refused (workspace-run.md §Rules), on Write and on Read.
func TestContextContentNotShapeScanned(t *testing.T) {
	content := "curl -H 'Authorization: Bearer " + jwt + "' # and " + ghsToken
	m, f := testManifest(), testFiles(t)
	f.Context[0] = contextJSON(t, 0, content)
	dir := filepath.Join(t.TempDir(), "bundle")
	if err := Write(dir, m, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if tc, gone, err := b.ContextItem(0); err != nil || gone || tc.Content != content {
		t.Errorf("context item 0: %+v gone=%v %v", tc, gone, err)
	}
}

func TestValidate(t *testing.T) {
	valid := func() Manifest {
		m := testManifest()
		m.Version, m.Dispatcher, m.Prompt = Version, DefaultDispatcherURL, PromptFile
		m.Context = []ContextFile{{Index: 0, File: "context/0.json"}, {Index: 2, Gone: true}}
		return m
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid manifest: %v", err)
	}
	tests := []struct {
		name  string
		edit  func(*Manifest)
		field string
	}{
		{"an unknown version", func(m *Manifest) { m.Version = 2 }, "version"},
		{"a run token for the run id", func(m *Manifest) { m.RunID = "rtk_" }, "runId"},
		{"a run token anywhere", func(m *Manifest) { m.LeaseID = rtkToken }, "leaseId"},
		{"not a run id", func(m *Manifest) { m.RunID = "1" }, "runId"},
		{"no task id", func(m *Manifest) { m.TaskID = "" }, "taskId"},
		{"no lease id", func(m *Manifest) { m.LeaseID = "" }, "leaseId"},
		{"a GitHub token", func(m *Manifest) { m.Git.Repo = ghsToken }, "git.repo"},
		{"a token in a read scope", func(m *Manifest) { m.Git.Reads[0].ReadRepos = []string{ghsToken} }, "git.reads[0].readRepos[0]"},
		{"a token as a permission's name", func(m *Manifest) { m.Git.Permissions[ghsToken] = "read" }, "git.permissions"},
		{"an API key", func(m *Manifest) { m.Codex = &CodexAccount{"sk-ant-api03-" + strings.Repeat("x", 95), "u", "p"} }, "codex.accountId"},
		{"an agent token", func(m *Manifest) { m.Git.Host = "agt_me-host." + strings.Repeat("s", 43) }, "git.host"},
		{"a JWT for the codex account", func(m *Manifest) { m.Codex = &CodexAccount{"acct-1", jwt, "pro"} }, "codex.userId"},
		{"a ftp dispatcher", func(m *Manifest) { m.Dispatcher = "ftp://dispatcher" }, "dispatcher"},
		{"a relative dispatcher", func(m *Manifest) { m.Dispatcher = "/dispatcher" }, "dispatcher"},
		{"a dispatcher with user info", func(m *Manifest) { m.Dispatcher = "http://u:" + rtkToken[4:] + "@dispatcher" }, "dispatcher"},
		{"a dispatcher with a query", func(m *Manifest) { m.Dispatcher = "http://dispatcher?x=1" }, "dispatcher"},
		{"a dispatcher with a fragment", func(m *Manifest) { m.Dispatcher = "http://dispatcher#x" }, "dispatcher"},
		{"no prompt", func(m *Manifest) { m.Prompt = "" }, "prompt"},
		{"an absolute prompt", func(m *Manifest) { m.Prompt = "/etc/passwd" }, "prompt"},
		{"a prompt above the bundle", func(m *Manifest) { m.Prompt = "../prompt.md" }, "prompt"},
		{"an unclean prompt", func(m *Manifest) { m.Prompt = "a/../prompt.md" }, "prompt"},
		{"a backslash", func(m *Manifest) { m.OutputSchema = `..\schema.json` }, "outputSchema"},
		{"a CA above the bundle", func(m *Manifest) { m.Gateway.CAFile = "../ca.pem" }, "gateway.caFile"},
		{"a context file above the bundle", func(m *Manifest) { m.Context[0].File = "../0.json" }, "context[0].file"},
		{"a gone item with a file", func(m *Manifest) { m.Context[1].File = "context/2.json" }, "context[1]"},
		{"an item without a file", func(m *Manifest) { m.Context[0].File = "" }, "context[0]"},
		{"a skill outside skills/", func(m *Manifest) { m.Skills[0].Dir = "work/lisa" }, "skills[0].dir"},
		{"a skill in another plugin", func(m *Manifest) { m.Skills[0].Dir = "skills/other/skills/lisa" }, "skills[0].dir"},
		{"a skill name with a slash", func(m *Manifest) { m.Skills[0].Name = "../x" }, "skills[0]"},
		{"a NoProxy URL", func(m *Manifest) { m.Gateway.NoProxy = []string{"http://x.example.com"} }, "gateway.noProxy[0]"},
		{"a NoProxy single label", func(m *Manifest) { m.Gateway.NoProxy = []string{"intranet"} }, "gateway.noProxy[0]"},
		{"a partial codex account", func(m *Manifest) { m.Codex = &CodexAccount{AccountID: "a"} }, "codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid()
			tt.edit(&m)
			wantInvalid(t, m.Validate(), tt.field)
		})
	}

	// Legal names that start like a token aren't refused.
	t.Run("lookalike names", func(t *testing.T) {
		m := valid()
		m.Git.Repo = "acme/sk-platform-infrastructure"
		m.Git.Reads = []GitRead{{Repo: "acme/ghs_tools", ReadRepos: []string{"acme/github_pat_scanner", "acme/rtk_runtime-kit"}}}
		m.Gateway.NoProxy = []string{"sk-registry-internal.example.com", "ghp-mirror.internal.example.com", "agt-cache.example.com"}
		m.Skills = []Skill{{Name: "sk-ant-reviewer", Plugin: "ghs_helpers", Dir: "skills/ghs_helpers/skills/sk-ant-reviewer"}}
		if err := m.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})
}

func TestReadRefuses(t *testing.T) {
	rewrite := func(name string, data []byte) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	editManifest := func(edit func(*Manifest)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
			if err != nil {
				t.Fatal(err)
			}
			var m Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			edit(&m)
			raw, _ = json.Marshal(m)
			rewrite(ManifestFile, raw)(t, dir)
		}
	}
	symlink := func(name, target string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	big := func(n int) []byte { return []byte(strings.Repeat("a", n)) }
	tests := []struct {
		name   string
		tamper func(t *testing.T, dir string)
		field  string
	}{
		{"an oversize bundle.json", rewrite(ManifestFile, big(MaxManifestBytes+1)), ManifestFile},
		{"a bundle.json that isn't one", rewrite(ManifestFile, []byte("{")), ManifestFile},
		{"an unknown version", editManifest(func(m *Manifest) { m.Version = 2 }), "version"},
		{"a token in the manifest", editManifest(func(m *Manifest) { m.Git.Repo = ghsToken }), "git.repo"},
		{"a path out of the bundle", editManifest(func(m *Manifest) { m.Prompt = "../../etc/passwd" }), "prompt"},
		{"an oversize task.json", rewrite(TaskFile, big(MaxTaskBytes+1)), TaskFile},
		{"another task", rewrite(TaskFile, []byte(`{"metadata":{"id":"tsk_2"}}`)), "taskId"},
		{"a token in task.json", func(t *testing.T, dir string) {
			task := testTask("codex")
			task.Spec.Inputs = map[string]any{"x": rtkToken}
			raw, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			rewrite(TaskFile, raw)(t, dir)
		}, TaskFile + ".spec.inputs"},
		{"an oversize prompt", rewrite(PromptFile, big(MaxPromptBytes+1)), PromptFile},
		{"an oversize schema", rewrite(SchemaFile, big(MaxSchemaBytes+1)), SchemaFile},
		{"an oversize CA", rewrite(CAFile, big(MaxCABytes+1)), CAFile},
		{"an oversize context file", rewrite("context/0.json", big(MaxContextFile+1)), "context/0.json"},
		{"an oversize context item", func(t *testing.T, dir string) {
			rewrite("context/0.json", contextJSON(t, 0, string(big(MaxContextBytes+1))))(t, dir)
		}, "context/0.json"},
		{"a context item listed twice", editManifest(func(m *Manifest) { m.Context = append(m.Context, m.Context[0]) }), "context[2]"},
		{"a context item missing", editManifest(func(m *Manifest) { m.Context = m.Context[:1] }), "context"},
		{"a repo item", editManifest(func(m *Manifest) { m.Context[1] = ContextFile{Index: 1, Gone: true} }), "context[1]"},
		{"a symlinked prompt", symlink(PromptFile, "/etc/hosts"), PromptFile},
		{"a symlink inside the bundle", symlink(PromptFile, "task.json"), PromptFile},
		{"a symlinked context dir", symlink(ContextDir, "/tmp"), "context/0.json"},
		{"a skill that is a file", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "skills", "ccf", "skills", "lisa")
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			rewrite("skills/ccf/skills/lisa", []byte("x"))(t, dir)
		}, "skills[0].dir"},
		{"a directory for the prompt", func(t *testing.T, dir string) {
			p := filepath.Join(dir, PromptFile)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}, PromptFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeBundle(t)
			tt.tamper(t, dir)
			b, err := Read(dir)
			if b != nil {
				t.Errorf("Read returned a bundle")
			}
			wantInvalid(t, err, tt.field)
		})
	}

	t.Run("a missing schema file", func(t *testing.T) {
		dir := writeBundle(t)
		if err := os.Remove(filepath.Join(dir, SchemaFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want not exist", err)
		}
	})
}

// secretName is a field name that would hold a credential.
var secretName = regexp.MustCompile(`(?i)token|secret|key|password|credential`)

// TestNoSecretFields walks every exported struct type of the package and fails on a field
// (or a JSON name) named like a credential (workspace-run.md §Rules). It stops at the
// package's boundary: the external types the bundle serialises (task.json's v1alpha1.Task and
// a context file's apiclient.TaskContext) are covered at run time instead: Write and Read
// shape-scan every string of task.json (TestWriteRefuses, TestReadRefuses), and a context
// file's content is untrusted provider text, which PrepareBundle checks for the job's real
// tokens (TestContextContentNotShapeScanned).
func TestNoSecretFields(t *testing.T) {
	roots := []reflect.Type{
		reflect.TypeFor[Manifest](), reflect.TypeFor[Files](), reflect.TypeFor[Bundle](),
		reflect.TypeFor[Outcome](), reflect.TypeFor[Event](), reflect.TypeFor[ValidationError](),
		reflect.TypeFor[Encoder](), reflect.TypeFor[Decoder](),
	}
	pkg := reflect.TypeFor[Manifest]().PkgPath()
	seen := map[string]bool{}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, at string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || rt.PkgPath() != pkg || seen[rt.Name()] {
			return
		}
		seen[rt.Name()] = true
		for i := range rt.NumField() {
			sf := rt.Field(i)
			tag, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			if secretName.MatchString(sf.Name) || secretName.MatchString(tag) {
				t.Errorf("%s.%s is named like a secret", at, sf.Name)
			}
			walk(sf.Type, at+"."+sf.Name)
		}
	}
	for _, rt := range roots {
		walk(rt, rt.Name())
	}

	// Every exported struct type declared in the package must be reachable from the roots.
	bp, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range g.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if _, isStruct := ts.Type.(*ast.StructType); isStruct && !seen[ts.Name.Name] {
					t.Errorf("type %s isn't walked: add it to the roots", ts.Name.Name)
				}
			}
		}
	}
}

// TestImports keeps the package free of network and pipeline code: the standard library
// (no net/http), v1alpha1, the apiclient types, the runner's and execfile (standard library
// only).
func TestImports(t *testing.T) {
	p, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"github.com/ContainerSolutions/factory/internal/apis/v1alpha1",
		"github.com/ContainerSolutions/factory/internal/apiclient",
		"github.com/ContainerSolutions/factory/internal/agent/runner",
		"github.com/ContainerSolutions/factory/internal/agent/internal/execfile",
	}
	for _, imp := range p.Imports {
		std := !strings.Contains(strings.Split(imp, "/")[0], ".")
		if (std && strings.HasPrefix(imp, "net/http")) || (!std && !slices.Contains(allowed, imp)) {
			t.Errorf("imports %s", imp)
		}
	}
}

func TestLooksLikeToken(t *testing.T) {
	b64 := func(n int) string { return strings.Repeat("aB3_-", n)[:n] }
	for s, want := range map[string]bool{
		// The real shapes, alone and in text.
		ghsToken: true, "gho_" + strings.Repeat("a1", 18): true, "ghp_" + strings.Repeat("Z", 40): true,
		"github_pat_" + strings.Repeat("A", 22) + "_" + strings.Repeat("b", 59): true,
		"sk-ant-api03-" + b64(95): true, "sk-ant-oat01-" + b64(95): true,
		"sk-proj-" + b64(120): true, "sk-" + strings.Repeat("T3Bl", 12): true,
		rtkToken: true, "enr_" + b64(43): true, "arf_" + b64(43): true,
		"agt_ana-box." + b64(43): true, "token=" + ghsToken + ";": true,
		jwt: true, "id_token: " + jwt: true,
		"Authorization: Bearer " + b64(40): true, "authorization: basic dXNlcjpwYXNzd29yZDEyMw==": true,
		"-----BEGIN RSA PRIVATE KEY-----": true,

		// Lookalikes: repos, hosts, skills and ids.
		"acme/sk-platform-infrastructure": false, "sk-registry-internal.example.com": false,
		"sk-ant-reviewer": false, "acme/sk-" + strings.Repeat("a", 30) + "-infra": false,
		"ghs_short": false, "acme/ghs_tools": false, "ghp-mirror.internal.example.com": false,
		"gho_" + strings.Repeat("a", 35): false, "github_pat_scanner": false,
		"rtk_x": false, "rtk_" + b64(42): false, "rtk_" + b64(44): false, "acme/rtk_runtime-kit": false,
		"enr_docs": false, "arf_" + b64(20): false,
		"agt_me-host": false, "agt_ana-box": false, "agt_ana-box." + b64(20): false,
		"eyJ-not-a-jwt": false, "eyJhbGciOiJ.short": false,
		"Basic authentication for the API": false, "use a Bearer token": false, "bearer tokenmanagementsystem": false,
		"run_1-1": false, "lse_0123": false, "acme/app": false,
	} {
		if got := LooksLikeToken(s); got != want {
			t.Errorf("LooksLikeToken(%.24q…) = %v, want %v", s, got, want)
		}
	}
}

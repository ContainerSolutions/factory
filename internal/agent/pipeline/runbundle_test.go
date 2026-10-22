package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

// testCA stands in for the gateway CA's public half.
const testCA = "-----BEGIN CERTIFICATE-----\nZmFrZS1nYXRld2F5LWNh\n-----END CERTIFICATE-----\n"

// prepare writes the fixture job's bundle into a new directory and reads it back.
func (f *fixture) prepare(job *runner.Job, x pipeline.BundleExtras) (string, *runbundle.Bundle) {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "bundle")
	if err := f.pipeline().PrepareBundle(context.Background(), job, dir, x); err != nil {
		f.t.Fatalf("PrepareBundle: %v", err)
	}
	b, err := runbundle.Read(dir)
	if err != nil {
		f.t.Fatalf("Read: %v", err)
	}
	return dir, b
}

// checkNoToken fails if any file under dir holds one of the job's tokens or a value shaped
// like a token, and checks the scratch directory under the work root is gone.
func (f *fixture) checkNoToken(dir string) {
	t := f.t
	t.Helper()
	scanForSecrets(t, dir)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); runbundle.LooksLikeToken(string(b)) {
				t.Errorf("%s holds a value shaped like a token", p)
			}
		}
		return nil
	})
	f.checkClean("")
	if ents, err := os.ReadDir(f.workRoot); (err != nil && !errors.Is(err, fs.ErrNotExist)) || len(ents) != 0 {
		t.Errorf("the work root after PrepareBundle: %v %v", ents, err)
	}
}

func TestPrepareBundle(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		gone    bool
		schema  string
		context []runbundle.ContextFile
	}{
		{name: "everything", schema: releaseDocsSchema, context: []runbundle.ContextFile{{Index: 0, File: "context/0.json"}}},
		{name: "a gone item and no schema", gone: true, context: []runbundle.ContextFile{{Index: 0, Gone: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.disp.gone[0] = tt.gone
			f.disp.schema = tt.schema
			job := f.job(f.task(), time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC))
			dir, b := f.prepare(job, pipeline.BundleExtras{CA: []byte(testCA)})
			m := b.Manifest
			want := runbundle.Manifest{
				Version: runbundle.Version, RunID: "run_01", LeaseID: "lse_1", TaskID: taskID,
				StartedAt: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), Dispatcher: runbundle.DefaultDispatcherURL,
				Gateway: runbundle.Gateway{CAFile: runbundle.CAFile},
				Git: runbundle.GitAccess{Provider: "github-public", Host: "github.com", Repo: "acme/docs",
					Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
					Reads:       []runbundle.GitRead{{Repo: "acme/private-ro"}}},
				Prompt: runbundle.PromptFile, Context: tt.context,
				Skills: []runbundle.Skill{
					{Name: "docs-style", Plugin: "ccf-docs", Dir: "skills/ccf-docs/skills/docs-style"},
					{Name: "review", Plugin: "ccf-review", Dir: "skills/ccf-review/skills/review"},
				},
			}
			if tt.schema != "" {
				want.OutputSchema = runbundle.SchemaFile
			}
			if !reflect.DeepEqual(m, want) {
				t.Errorf("manifest\n got %+v\nwant %+v", m, want)
			}
			if string(b.Prompt) != promptBody || string(b.OutputSchema) != tt.schema {
				t.Errorf("prompt %q, schema %q", b.Prompt, b.OutputSchema)
			}
			if !reflect.DeepEqual(b.Task, job.Lease.Task) {
				t.Errorf("task.json is not the lease's task: %+v", b.Task)
			}
			tc, gone, err := pipeline.BundlePrompts{B: b}.Context(context.Background(), job, 0)
			if err != nil || gone != tt.gone || (!gone && (tc.Index != 0 || tc.Content != injection || tc.Trust != "untrusted")) {
				t.Errorf("context item 0: %+v gone=%v %v", tc, gone, err)
			}
			for _, p := range []string{"ccf-docs/.claude-plugin/plugin.json", "ccf-docs/skills/docs-style/scripts/check.sh", "ccf-review/bin/ccfr", "ccf-review/skills/review/SKILL.md"} {
				if _, err := os.Stat(filepath.Join(dir, "skills", p)); err != nil {
					t.Errorf("skills/%s: %v", p, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "skills", "ccf-docs", "skills", "docs-gif")); err == nil {
				t.Error("an unselected skill is in the bundle")
			}
			if calls := f.disp.callList(); hasCall(calls, "PUT") {
				t.Errorf("PrepareBundle uploaded: %v", calls)
			}
			f.checkNoToken(dir)
		})
	}
}

// hasCall reports whether a call of method is in calls.
func hasCall(calls []string, method string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, method+" ") {
			return true
		}
	}
	return false
}

func (d *dispatcher) callList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func TestPrepareBundleRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, f *fixture, job *runner.Job, x *pipeline.BundleExtras, dir string)
		want  string
	}{
		{"the prompt's digest", func(_ *testing.T, f *fixture, _ *runner.Job, _ *pipeline.BundleExtras, _ string) {
			f.disp.prompt = "another prompt\n"
		}, "prompt: the prompt's digest is"},
		{"a context item over the cap", func(_ *testing.T, f *fixture, _ *runner.Job, _ *pipeline.BundleExtras, _ string) {
			f.disp.items[0] = apiclient.TaskContext{Index: 0, Content: strings.Repeat("x", 4<<20+1)}
		}, "prompt: context item 0 is over"},
		{"the schema unavailable", func(_ *testing.T, f *fixture, _ *runner.Job, _ *pipeline.BundleExtras, _ string) {
			f.disp.schemaStatus = http.StatusInternalServerError
		}, "schema: the output schema: GET /tasks/" + taskID + "/output-schema: HTTP 500"},
		{"a schema that doesn't compile", func(_ *testing.T, f *fixture, _ *runner.Job, _ *pipeline.BundleExtras, _ string) {
			f.disp.schema = `{"type": 12}`
		}, "schema: the output schema"},
		{"a skill not found", func(_ *testing.T, _ *fixture, job *runner.Job, _ *pipeline.BundleExtras, _ string) {
			job.Lease.Task.Spec.Skills[0].Use = []string{"nope"}
		}, `skills: `},
		{"no run", func(_ *testing.T, _ *fixture, job *runner.Job, _ *pipeline.BundleExtras, _ string) {
			job.Lease.Lease.Run = nil
		}, "the lease names no run"},
		{"a codex account for another harness", func(_ *testing.T, _ *fixture, _ *runner.Job, x *pipeline.BundleExtras, _ string) {
			x.Codex = &runbundle.CodexAccount{AccountID: "a", UserID: "u", PlanType: "pro"}
		}, "codex"},
		{"an existing bundle", func(t *testing.T, _ *fixture, _ *runner.Job, _ *pipeline.BundleExtras, dir string) {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "already exists"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			job := f.job(f.task(), time.Now())
			x := pipeline.BundleExtras{CA: []byte(testCA)}
			dir := filepath.Join(t.TempDir(), "bundle")
			tt.setup(t, f, job, &x, dir)
			err := f.pipeline().PrepareBundle(context.Background(), job, dir, x)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("PrepareBundle: %v, want %q", err, tt.want)
			}
			if tt.name != "an existing bundle" {
				if _, err := os.Lstat(dir); err == nil {
					t.Error("a failed PrepareBundle left a bundle")
				}
			}
			if _, err := os.Lstat(dir + ".tmp"); err == nil {
				t.Error("a failed PrepareBundle left its temp dir")
			}
			f.checkNoToken(filepath.Dir(dir))
		})
	}
}

// seen is what the harness was given: the prompt, the tickets, the schema file and the skills
// (each file's path under the workspace's Skills, its executable bit and its content).
type seen struct {
	prompt, tickets, schema string
	skills                  []string
	list                    []string
}

func (s *seen) capture(t *testing.T) func(context.Context, harness.Workspace, string) error {
	return func(_ context.Context, ws harness.Workspace, prompt string) error {
		s.prompt = prompt
		tk, err := os.ReadFile(filepath.Join(ws.Repo, "tickets", "acme-docs-12.md"))
		if err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
		s.tickets = string(tk)
		sc, err := os.ReadFile(ws.Schema)
		if err != nil {
			t.Error(err)
		}
		s.schema = string(sc)
		_ = filepath.WalkDir(ws.Skills, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(ws.Skills, p)
			info, _ := d.Info()
			b, _ := os.ReadFile(p)
			s.skills = append(s.skills, fmt.Sprintf("%s x=%v %q", filepath.ToSlash(rel), info.Mode()&0o100 != 0, b))
			return nil
		})
		sort.Strings(s.skills)
		for _, sk := range ws.SkillList {
			rel, _ := filepath.Rel(ws.Skills, sk.Dir)
			s.list = append(s.list, sk.Plugin+":"+sk.Name+" "+filepath.ToSlash(rel))
		}
		return nil
	}
}

// String is all of it; the golden (summary) has the schema file's digest instead of the
// file, since it holds the whole v1alpha1 Result schema.
func (s *seen) String() string { return s.render(s.schema) }

func (s *seen) summary() string {
	sum := sha256.Sum256([]byte(s.schema))
	return s.render("sha256:" + hex.EncodeToString(sum[:]) + "\n")
}

func (s *seen) render(schema string) string {
	return "## prompt\n" + s.prompt + "\n## tickets/acme-docs-12.md\n" + s.tickets + "\n## result.schema.json\n" + schema +
		"\n## skills\n" + strings.Join(s.list, "\n") + "\n## skill files\n" + strings.Join(s.skills, "\n") + "\n"
}

// A run from a bundle (its prompt, context, schema and skills; no dispatcher client) gives the
// harness what the API-sourced run gives it, byte for byte.
func TestBundleRunMatchesAPIRun(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		gone   bool
		golden string
	}{
		{"everything", false, "bundle-run.golden"},
		{"a gone item", true, "bundle-run-gone.golden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.disp.gone[0] = tt.gone
			var api, bundled seen
			f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult), Do: api.capture(t)}}
			out, err := f.pipeline().Run(context.Background(), f.job(f.task(), time.Now()), &sink{})
			if err != nil || out.Status != runner.StatusDone {
				t.Fatalf("API run: %+v %v", out, err)
			}

			_, b := f.prepare(f.job(f.task(), time.Now()), pipeline.BundleExtras{})
			before := len(f.disp.callList())
			f.opts.Prompts = pipeline.BundlePrompts{B: b}
			f.opts.Schemas = pipeline.BundleSchemas{B: b}
			f.opts.Skills = pipeline.BundleSkills(b)
			f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult), Do: bundled.capture(t)}}
			job := f.job(f.task(), time.Now())
			job.API = nil
			out, err = f.pipeline().Run(context.Background(), job, &sink{})
			if err != nil || out.Status != runner.StatusDone || out.TranscriptRef != "" {
				t.Fatalf("bundle run: %+v %v", out, err)
			}
			if calls := f.disp.callList()[before:]; len(calls) != 0 {
				t.Errorf("the bundle run called the dispatcher: %v", calls)
			}
			if got, want := bundled.String(), api.String(); got != want {
				t.Errorf("the bundle run differs from the API run:\n--- bundle\n%s\n--- api\n%s", got, want)
			}
			if tt.gone != strings.Contains(api.prompt, "It no longer exists.") || tt.gone != (api.tickets == "") {
				t.Errorf("gone=%v, the prompt:\n%s\ntickets: %q", tt.gone, api.prompt, api.tickets)
			}
			golden(t, tt.golden, []byte(api.summary()))
		})
	}
}

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run with -update):\n%s", p, got)
	}
}

// The bundle sources refuse another task's bundle and a missing one; FetchSkills runs only in
// the pipeline.
func TestBundleSourcesRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := &runner.Job{Lease: apiclient.Lease{}}
	other.Lease.Task.Metadata.ID = "tsk_other"
	b := &runbundle.Bundle{Manifest: runbundle.Manifest{TaskID: taskID}}
	for name, call := range map[string]func() error{
		"Prompt":       func() error { _, err := pipeline.BundlePrompts{B: b}.Prompt(ctx, other); return err },
		"Context":      func() error { _, _, err := pipeline.BundlePrompts{B: b}.Context(ctx, other, 0); return err },
		"OutputSchema": func() error { _, err := pipeline.BundleSchemas{B: b}.OutputSchema(ctx, other); return err },
		"no bundle":    func() error { _, err := pipeline.BundlePrompts{}.Prompt(ctx, other); return err },
		"FetchSkills":  func() error { _, _, err := pipeline.FetchSkills{}.Skills(ctx, other, t.TempDir()); return err },
		"PrebuiltSkills dir": func() error {
			_, _, err := pipeline.PrebuiltSkills{List: []harness.Skill{{Name: "a"}}}.Skills(ctx, other, "")
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

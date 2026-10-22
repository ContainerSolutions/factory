package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
)

// BundleExtras are what a run bundle holds beyond the job (workspace-run.md
// §pipeline.PrepareBundle): the gateway's CA (the public half), the dispatcher's URL as the
// VM sees it ("": runbundle.DefaultDispatcherURL) and the codex account, set exactly when the
// task's harness is codex.
type BundleExtras struct {
	CA         []byte
	Dispatcher string
	Codex      *runbundle.CodexAccount
}

// PrepareBundle writes job's run bundle at dir, on the host, before a VM exists
// (workspace-run.md §pipeline.PrepareBundle). It fetches the rendered prompt (checked
// against spec.prompt.digest), each non-repo context item (a 404 is Gone) and the output
// schema (a 404 is no schema) through the pipeline's sources, with the bounds and refusals of
// the prompt and schema steps; materialises the skills with the pipeline's git and skills
// cache into a scratch directory under WorkRoot, which is always removed; keeps the job's git
// credentials without their tokens (runbundle.GitAccess); refuses a bundle any of whose bytes
// hold one of the job's real secrets (checkSecrets); and calls runbundle.Write, which refuses
// an existing dir. No token reaches the bundle. Its errors name the step that failed;
// a caller fails the run with harness-error, as the matching host step would.
func (p *Pipeline) PrepareBundle(ctx context.Context, job *runner.Job, dir string, x BundleExtras) error {
	task := job.Lease.Task
	id := task.Metadata.ID
	if !taskIDRE.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("task id %q can't name a workspace", id)
	}
	var runID string
	if job.Lease.Lease.Run != nil {
		runID = *job.Lease.Lease.Run
	}
	if runID == "" {
		return errors.New("the lease names no run")
	}
	start := job.StartedAt
	if start.IsZero() {
		start = p.clk.Now()
	}
	r := &run{p: p, job: job, task: task, id: id, start: start, log: p.log.With("task", id), red: newRedactor(job)}

	raw, err := p.o.Prompts.Prompt(ctx, job)
	if err == nil {
		err = checkPrompt(task, raw)
	}
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	items, err := p.bundleContext(ctx, job)
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	schema, err := p.bundleSchema(ctx, job)
	if err != nil {
		return fmt.Errorf("schema: %w", err)
	}

	if err := os.MkdirAll(p.o.WorkRoot, 0o700); err != nil {
		return fmt.Errorf("work root: %w", err)
	}
	scratch, err := os.MkdirTemp(p.o.WorkRoot, id+".bundle-")
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	defer func() {
		if rerr := removeAll(scratch); rerr != nil {
			r.log.Error("removing the bundle's scratch directory failed", "dir", scratch, "err", rerr)
		}
	}()
	r.ws = harness.Workspace{Root: scratch, Skills: filepath.Join(scratch, "skills"), Home: filepath.Join(scratch, "home"), Tmp: filepath.Join(scratch, "tmp")}
	for _, d := range []string{r.ws.Skills, r.ws.Home, r.ws.Tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
	}
	defer func() {
		if r.creds != nil {
			if cerr := r.creds.Close(); cerr != nil {
				r.log.Warn("closing the credential socket failed", "err", cerr)
			}
		}
	}()
	if err := r.startCredentials(ctx); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	list, err := p.skillFetcher(r.git, scratch, r.ws.Tmp).fetch(ctx, task.Spec.Skills, r.ws.Skills)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	skills, err := bundleSkills(r.ws.Skills, list)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}

	m := runbundle.Manifest{
		RunID: runID, LeaseID: job.Lease.Lease.Id, TaskID: id, StartedAt: start.UTC(),
		Dispatcher: x.Dispatcher, Git: gitAccess(job.Git), Skills: skills,
	}
	f := runbundle.Files{Task: task, Prompt: raw, Context: items, OutputSchema: schema, CA: x.CA, Codex: x.Codex}
	if len(skills) > 0 {
		f.SkillsDir = r.ws.Skills
	}
	if err := r.checkSecrets(m, f); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := runbundle.Write(dir, m, f); err != nil {
		return err
	}
	r.log.Debug("run bundle written", "dir", dir, "skills", len(skills))
	return nil
}

// checkSecrets refuses a bundle that would hold one of the job's real secrets (its git token,
// its read tokens and its agent token: the redactor's): every byte the bundle will have (the
// manifest, task.json, the prompt, the context items, the schema, the CA, the codex account
// and each skill file) is searched for an exact match. The error names the file, never the
// secret. runbundle's shape scan covers the manifest and task.json only; context content is
// untrusted provider text, where only the run's own tokens matter.
func (r *run) checkSecrets(m runbundle.Manifest, f runbundle.Files) error {
	secrets := r.red.secrets()
	if len(secrets) == 0 {
		return nil
	}
	files := map[string][]byte{runbundle.PromptFile: f.Prompt, runbundle.SchemaFile: f.OutputSchema, runbundle.CAFile: f.CA}
	for name, v := range map[string]any{runbundle.ManifestFile: m, runbundle.TaskFile: f.Task, runbundle.CodexFile: f.Codex} {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		files[name] = b
	}
	for i, b := range f.Context {
		files[path.Join(runbundle.ContextDir, strconv.Itoa(i)+".json")] = b
	}
	holds := func(b []byte) bool {
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				return true
			}
		}
		return false
	}
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		if holds(files[name]) {
			return fmt.Errorf("%s holds one of the job's credentials", name)
		}
	}
	if f.SkillsDir == "" {
		return nil
	}
	return filepath.WalkDir(f.SkillsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if holds(b) {
			rel, _ := filepath.Rel(f.SkillsDir, p)
			return fmt.Errorf("%s holds one of the job's credentials", path.Join(runbundle.SkillsDir, filepath.ToSlash(rel)))
		}
		return nil
	})
}

// bundleContext fetches each non-repo context item through the PromptSource, as the prompt
// step does: the JSON of each that exists, by index; a gone one is left out (Gone).
func (p *Pipeline) bundleContext(ctx context.Context, job *runner.Job) (map[int][]byte, error) {
	items := map[int][]byte{}
	for i, c := range job.Lease.Task.Spec.Context {
		if c.Kind == "repo" {
			continue
		}
		tc, gone, err := p.o.Prompts.Context(ctx, job, i)
		if err != nil {
			return nil, err
		}
		if gone {
			continue
		}
		if err := checkContext(i, tc); err != nil {
			return nil, err
		}
		tc.Index = i // the item the source answered for
		b, err := json.Marshal(tc)
		if err != nil {
			return nil, fmt.Errorf("context item %d: %w", i, err)
		}
		items[i] = b
	}
	return items, nil
}

// bundleSchema fetches the output schema through the SchemaSource and refuses what the schema
// step would: a schema that doesn't compile, or that BundleSchema can't make self-contained.
// nil is no schema (a 404). The bundle keeps the document as served; the VM's schema step
// bundles it.
func (p *Pipeline) bundleSchema(ctx context.Context, job *runner.Job) ([]byte, error) {
	spec, err := p.o.Schemas.OutputSchema(ctx, job)
	if err != nil {
		return nil, fmt.Errorf("the output schema: %w", err)
	}
	if spec == nil {
		return nil, nil
	}
	if _, err := compileSpecSchema(job.Lease.Task.Spec.TaskSpec.Name, spec); err != nil {
		return nil, err
	}
	if _, err := BundleSchema(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// bundleSkills is list as the manifest has it: each Dir relative to the bundle
// (skills/<plugin>/skills/<name>), skillsDir being the bundle's skills/.
func bundleSkills(skillsDir string, list []harness.Skill) ([]runbundle.Skill, error) {
	out := make([]runbundle.Skill, 0, len(list))
	for _, s := range list {
		rel, err := filepath.Rel(skillsDir, s.Dir)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("skill %s:%s is not under the skills directory", s.Plugin, s.Name)
		}
		out = append(out, runbundle.Skill{Name: s.Name, Plugin: s.Plugin, Dir: path.Join(runbundle.SkillsDir, filepath.ToSlash(rel))})
	}
	return out, nil
}

// gitAccess is the job's git credentials without their tokens: the task credential's
// provider, host, repo and permissions, and each read credential's scope.
func gitAccess(c *runner.Credentials) runbundle.GitAccess {
	if c == nil {
		return runbundle.GitAccess{}
	}
	g := c.Get()
	a := runbundle.GitAccess{Provider: g.Provider, Host: g.Host, Repo: g.Repo, Permissions: maps.Clone(g.Permissions)}
	for _, rc := range c.Reads() {
		a.Reads = append(a.Reads, runbundle.GitRead{Repo: rc.Repo, ReadRepos: slices.Clone(rc.ReadRepos)})
	}
	return a
}

// errNoBundle is a bundle source without its bundle.
var errNoBundle = errors.New("no run bundle")

// bundleFor is b when it is the job's task's bundle.
func bundleFor(b *runbundle.Bundle, job *runner.Job) (*runbundle.Bundle, error) {
	if b == nil {
		return nil, errNoBundle
	}
	if id := job.TaskID(); id != b.TaskID() {
		return nil, fmt.Errorf("the run bundle is task %s's, not %s's", b.TaskID(), id)
	}
	return b, nil
}

// BundlePrompts is the PromptSource of a run bundle: its prompt.md and context/<n>.json (a
// Gone item is the 404 case). renderPrompt checks the digest, as for the API's.
type BundlePrompts struct {
	B *runbundle.Bundle
}

var _ PromptSource = BundlePrompts{}

// Prompt implements PromptSource.
func (s BundlePrompts) Prompt(_ context.Context, job *runner.Job) ([]byte, error) {
	b, err := bundleFor(s.B, job)
	if err != nil {
		return nil, err
	}
	return b.Prompt, nil
}

// Context implements PromptSource.
func (s BundlePrompts) Context(_ context.Context, job *runner.Job, i int) (apiclient.TaskContext, bool, error) {
	b, err := bundleFor(s.B, job)
	if err != nil {
		return apiclient.TaskContext{}, false, err
	}
	return b.ContextItem(i)
}

// BundleSchemas is the SchemaSource of a run bundle: its output-schema.json, or nil when the
// bundle has none (the API answered 404).
type BundleSchemas struct {
	B *runbundle.Bundle
}

var _ SchemaSource = BundleSchemas{}

// OutputSchema implements SchemaSource.
func (s BundleSchemas) OutputSchema(_ context.Context, job *runner.Job) ([]byte, error) {
	b, err := bundleFor(s.B, job)
	if err != nil {
		return nil, err
	}
	return b.OutputSchema, nil
}

// BundleSkills is the PrebuiltSkills of a run bundle: its skills/ and its manifest's skills.
func BundleSkills(b *runbundle.Bundle) PrebuiltSkills {
	var list []harness.Skill
	for _, s := range b.Skills() {
		list = append(list, harness.Skill{Name: s.Name, Plugin: s.Plugin, Dir: s.Dir})
	}
	return PrebuiltSkills{Dir: b.Path(runbundle.SkillsDir), List: list}
}

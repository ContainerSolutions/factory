package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"text/template"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/rules"
)

// withTaskSpec is cat with name's TaskSpec changed by mut (the TaskSpec, its spec and the
// catalog are copied: the loaded ones are shared; mut replaces fields, it doesn't write
// through them).
func withTaskSpec(c *catalog.Catalog, name string, mut func(*catalog.CompiledTaskSpec)) *catalog.Catalog {
	ts := *c.TaskSpecs[name]
	spec := *ts.Spec
	ts.Spec = &spec
	if mut != nil {
		mut(&ts)
	}
	out := *c
	out.TaskSpecs = maps.Clone(c.TaskSpecs)
	out.TaskSpecs[name] = &ts
	return &out
}

// recordSkills resolves every skill to its source at commit (or fails with err), recording
// the source@ref it was asked for.
type recordSkills struct {
	commit string
	err    error
	calls  []string
}

func (r *recordSkills) Resolve(_ context.Context, source, ref string) (string, string, error) {
	r.calls = append(r.calls, source+"@"+ref)
	return source, r.commit, r.err
}

// stored is t as the store gives it back (a JSON round trip).
func stored(t *testing.T, task *v1alpha1.Task) *v1alpha1.Task {
	t.Helper()
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var out v1alpha1.Task
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestRecompile(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan() // primary link: docs#42
	sub := docsRepo + "#43"
	e.seed(provider.KindIssue, sub, nil)
	compiled, origPrompt, err := compileTask(e.ctx, compileInput{
		cat: e.cat, wi: wi, ev: v1alpha1.Event{ID: "ev", Source: ghProvider}, ruleSet: "rs", rule: "r",
		taskID: "tsk_o", now: e.clk.Now(), kindOf: e.rule.apply.objectKind,
		d: &rules.Dispatch{
			TaskSpec: "implement", Queue: "poc", Inputs: map[string]any{"issue": sub, "base": "dev"},
			Context: []string{sub}, Requires: &v1alpha1.Requires{Labels: []string{"gpu"}},
		},
	}, &recordSkills{commit: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	compiled.Metadata.Labels["pack"] = "docs"
	orig := stored(t, compiled)
	opts := func(id string) RecompileOptions { return RecompileOptions{TaskID: id, Now: e.clk.Now()} }

	t.Run("an unchanged catalog compiles the same task", func(t *testing.T) {
		task, prompt, err := Recompile(e.ctx, e.cat, &recordSkills{commit: "c1"}, wi, orig, opts(orig.Metadata.ID))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(task.Spec, compiled.Spec) {
			t.Errorf("spec = %+v\nwant   %+v", task.Spec, compiled.Spec)
		}
		if !reflect.DeepEqual(prompt, origPrompt) {
			t.Errorf("prompt = %+v, want %+v", prompt, origPrompt)
		}
		want := v1alpha1.Provenance{RuleSet: "rs", Rule: "r", CatalogSHA: e.cat.SHA} // the event is the caller's
		if *task.Metadata.Provenance != want || !maps.Equal(task.Metadata.Labels, compiled.Metadata.Labels) ||
			task.Metadata.WorkItem != wi.Metadata.Name || task.Status.Phase != v1alpha1.TaskPending {
			t.Errorf("metadata %+v status %+v", task.Metadata, task.Status)
		}
	})

	newPrompt := template.Must(template.New("p").Parse("v2 {{.task.id}} {{.inputs.issue}} {{.item.id}} {{len .context}}"))
	changed := withTaskSpec(e.cat, "implement", func(ts *catalog.CompiledTaskSpec) {
		ts.Digest = "sha256:changed"
		ts.Prompt = &catalog.Prompt{Path: "prompts/implement-v2.md.tmpl", Template: newPrompt}
		ts.Spec.Spec.Harness = &v1alpha1.HarnessPolicy{Default: &v1alpha1.HarnessRequest{Name: "claude-code"}}
		ts.Spec.Spec.Skills = []v1alpha1.SkillRef{{Source: "ccf-skills", Ref: "v0.9.0", Use: []string{"lisa"}}}
		ts.Spec.Spec.Limits = &v1alpha1.Limits{MaxWallClock: "20m"}
		ts.Spec.Spec.Effects = &v1alpha1.EffectsPolicy{Allow: []v1alpha1.EffectOp{v1alpha1.OpPROpen}}
	})

	t.Run("a changed TaskSpec is recompiled", func(t *testing.T) {
		skills := &recordSkills{commit: "c2"}
		task, prompt, err := Recompile(e.ctx, changed, skills, wi, orig, opts("tsk_n"))
		if err != nil {
			t.Fatal(err)
		}
		wantText := "v2 tsk_n " + sub + " " + wi.Metadata.Name + " 3"
		sum := sha256.Sum256([]byte(wantText))
		if prompt.Text != wantText || prompt.Digest != "sha256:"+hex.EncodeToString(sum[:]) ||
			prompt.Metadata.Name != "tsk_n" || task.Spec.Prompt.Digest != prompt.Digest || prompt.Digest == origPrompt.Digest {
			t.Errorf("prompt %+v, task prompt %+v", prompt, task.Spec.Prompt)
		}
		if task.Spec.Prompt.Ref != "api://tasks/tsk_n/prompt" || task.Spec.TaskSpec.Digest != "sha256:changed" {
			t.Errorf("prompt ref %q, TaskSpec %+v", task.Spec.Prompt.Ref, task.Spec.TaskSpec)
		}
		// Kept: inputs, target, the dispatch's context (and the mount, recomputed alike),
		// requires, labels.
		if !reflect.DeepEqual(task.Spec.Inputs, compiled.Spec.Inputs) || *task.Spec.Target != *compiled.Spec.Target ||
			!slices.Equal(task.Spec.Context, compiled.Spec.Context) || !reflect.DeepEqual(task.Spec.Requires, compiled.Spec.Requires) ||
			task.Metadata.Labels["pack"] != "docs" || task.Metadata.Labels[LabelTaskSpec] != "implement" {
			t.Errorf("kept fields: %+v %+v", task.Spec, task.Metadata.Labels)
		}
		// Recomputed: harness (the TaskSpec changed), skills, limits, credentials TTL, effects.
		if task.Spec.Harness.Request.Name != "claude-code" || !slices.Equal(skills.calls, []string{"ccf-skills@v0.9.0"}) ||
			len(task.Spec.Skills) != 1 || task.Spec.Skills[0].Commit != "c2" || task.Spec.Limits.MaxWallClock != "20m" ||
			task.Spec.Credentials.Git.TTL != "30m0s" || !slices.Equal(task.Spec.Effects.Allow, []v1alpha1.EffectOp{v1alpha1.OpPROpen}) {
			t.Errorf("recomputed fields: harness %+v skills %+v (%v) limits %+v cred %+v effects %+v", task.Spec.Harness.Request,
				task.Spec.Skills, skills.calls, task.Spec.Limits, task.Spec.Credentials.Git, task.Spec.Effects)
		}
	})

	t.Run("workspace mounts are recomputed", func(t *testing.T) {
		stale := stored(t, orig)
		stale.Spec.Context = append(stale.Spec.Context, v1alpha1.ContextItem{Kind: "repo", Provider: ghProvider, Ref: "old/repo@x", Mount: "/work/repo"})
		task, _, err := Recompile(e.ctx, e.cat, KeepSkills{}, wi, stale, opts("tsk_m"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(task.Spec.Context, compiled.Spec.Context) {
			t.Errorf("context = %+v, want %+v", task.Spec.Context, compiled.Spec.Context)
		}
	})

	t.Run("harness", func(t *testing.T) {
		requested := stored(t, orig)
		requested.Spec.Harness.Request = &v1alpha1.HarnessRequest{Name: "claude-code"} // a dispatch's request
		for _, tc := range []struct {
			name     string
			cat      *catalog.Catalog
			override *v1alpha1.HarnessRequest
			want     string
		}{
			{name: "an unchanged TaskSpec keeps the request", cat: e.cat, want: "claude-code"},
			{name: "a changed TaskSpec takes its default", cat: withTaskSpec(e.cat, "implement", func(ts *catalog.CompiledTaskSpec) {
				ts.Digest = "sha256:changed"
			}), want: "codex"},
			{name: "the override wins", cat: changed, override: &v1alpha1.HarnessRequest{Name: "codex-next"}, want: "codex-next"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				o := opts("tsk_h")
				o.Harness = tc.override
				task, _, err := Recompile(e.ctx, tc.cat, KeepSkills{}, wi, requested, o)
				if err != nil {
					t.Fatal(err)
				}
				if got := task.Spec.Harness.Request.Name; got != tc.want || task.Spec.Harness.Resolved != nil {
					t.Errorf("harness = %+v, want %s", task.Spec.Harness, tc.want)
				}
			})
		}
	})

	t.Run("refused", func(t *testing.T) {
		noSpec := *e.cat
		noSpec.TaskSpecs = maps.Clone(e.cat.TaskSpecs)
		delete(noSpec.TaskSpecs, "implement")
		noQueue := *e.cat
		noQueue.Queues = maps.Clone(e.cat.Queues)
		delete(noQueue.Queues, "poc")
		undeclared := stored(t, orig)
		undeclared.Spec.Inputs["gone"] = "x"
		broken := withTaskSpec(e.cat, "implement", func(ts *catalog.CompiledTaskSpec) {
			ts.Prompt = &catalog.Prompt{Path: "prompts/broken.md.tmpl", Template: template.Must(template.New("p").Option("missingkey=error").Parse("{{.nope.x}}"))}
		})
		for _, tc := range []struct {
			name         string
			cat          *catalog.Catalog
			task         *v1alpha1.Task
			skills       SkillResolver
			notInCatalog bool
			invalid      bool // neither: transient
		}{
			{name: "TaskSpec gone", cat: &noSpec, task: orig, notInCatalog: true, invalid: true},
			{name: "queue gone", cat: &noQueue, task: orig, notInCatalog: true, invalid: true},
			{name: "an input the TaskSpec doesn't declare", cat: e.cat, task: undeclared, invalid: true},
			{name: "a prompt that doesn't render", cat: broken, task: orig, invalid: true},
			{name: "skills unresolved", cat: e.cat, task: orig, skills: &recordSkills{err: errors.New("ls-remote: 503")}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, _, err := Recompile(e.ctx, tc.cat, tc.skills, wi, tc.task, opts("tsk_r"))
				if err == nil {
					t.Fatal("recompiled")
				}
				if errors.Is(err, ErrNotInCatalog) != tc.notInCatalog || errors.Is(err, ErrInvalidDispatch) != tc.invalid {
					t.Errorf("err = %v: not in catalog %v, invalid %v", err, errors.Is(err, ErrNotInCatalog), errors.Is(err, ErrInvalidDispatch))
				}
			})
		}
	})
}

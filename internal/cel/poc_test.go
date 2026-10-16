package cel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// examplesDir holds the documents of design/03 and design/10 as decoded fixtures.
const examplesDir = "../apis/v1alpha1/testdata/examples"

func newTestEnv(t *testing.T, opts ...Option) *Env {
	t.Helper()
	env, err := NewEnv(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// loadExamples decodes the example documents matching glob.
func loadExamples(t *testing.T, glob string) []v1alpha1.Object {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(examplesDir, glob))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no examples match %s: %v", glob, err)
	}
	sort.Strings(paths)
	var out []v1alpha1.Object
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		objs, err := v1alpha1.Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, objs...)
	}
	return out
}

// toTree converts a typed value to its YAML tree, as the catalog sees it.
func toTree(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

// TestPoCExpressionsCompile is the task's done criterion: every where and "=" expression of
// design/10-poc.md (the 10-* fixtures, plus the alternative rule and the sampling example
// that 10 only shows in comments and prose) compiles against the environment, uses only
// documented fields and has a cost estimate under the limit.
func TestPoCExpressionsCompile(t *testing.T) {
	env := newTestEnv(t)
	idx := ProgramIndex{}
	var wheres []*Program
	for _, obj := range loadExamples(t, "10-*.yaml") {
		switch o := obj.(type) {
		case *v1alpha1.Rule:
			p, err := env.Compile(o.Spec.Where, Where)
			if err != nil {
				t.Errorf("rule %s: where: %v", o.GetName(), err)
				continue
			}
			wheres = append(wheres, p)
			sub, err := SubstituteRule(o, o.Spec.Params)
			if err != nil {
				t.Errorf("rule %s: substitute: %v", o.GetName(), err)
				continue
			}
			if err := env.CompileTree(toTree(t, sub.Spec.Actions), idx); err != nil {
				t.Errorf("rule %s: actions: %v", o.GetName(), err)
			}
		case *v1alpha1.TaskSpec:
			if o.Spec.Workspace == nil {
				continue
			}
			for _, r := range o.Spec.Workspace.Repos {
				if err := env.CompileTree(r.Ref, idx); err != nil {
					t.Errorf("taskspec %s: workspace ref: %v", o.GetName(), err)
				}
			}
		}
	}
	// Expressions 10 has only in a comment (the per-release alternative) and in prose
	// (sampling for the validation queue).
	for _, src := range []string{"!event.release.semver.prerelease", "hash(item.id) % 10 == 0"} {
		p, err := env.Compile(src, Where)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		wheres = append(wheres, p)
	}
	if err := env.CompileTree("=event.repo + '@' + event.release.tag", idx); err != nil {
		t.Error(err)
	}

	// 15 rules in 10 plus the two extras; the "=" leaves of 10's actions and TaskSpecs.
	if len(wheres) != 17 {
		t.Errorf("compiled %d where expressions, want 17", len(wheres))
	}
	wantValues := []string{
		"event.object.ref",
		"{'train': item.state.train}",
		"run.result.plan",
		"size(run.result.plan)",
		"item.state.plan[0].issue",
		"item.state.stackNext + 1",
		"item.state.plan[item.state.stackNext].issue",
		"run.result.branch",
		"run.outcome",
		"task.target.repo + '@' + inputs.base",
		"event.repo + '@' + event.release.tag",
	}
	for _, src := range wantValues {
		if idx[src] == nil {
			t.Errorf("value %q was not compiled", src)
		}
	}
	if len(idx) != len(wantValues) {
		t.Errorf("compiled %d values, want %d: %v", len(idx), len(wantValues), keys(idx))
	}

	all := wheres
	for _, p := range idx {
		all = append(all, p)
	}
	for _, p := range all {
		if fe := p.UnknownFields(); len(fe) > 0 {
			t.Errorf("%q: unknown fields: %v", p.Source(), fe)
		}
		_, maxCost, err := p.EstimateCost()
		if err != nil {
			t.Errorf("%q: estimate: %v", p.Source(), err)
		} else if maxCost > DefaultCostLimit {
			t.Errorf("%q: estimated cost %d over the limit %d", p.Source(), maxCost, DefaultCostLimit)
		}
	}
}

// TestDomainModelExampleCompiles covers the 03 rule, whose harness is a whole-value param.
func TestDomainModelExampleCompiles(t *testing.T) {
	env := newTestEnv(t)
	for _, obj := range loadExamples(t, "03-06-rule.yaml") {
		r := obj.(*v1alpha1.Rule)
		if _, err := env.Compile(r.Spec.Where, Where); err != nil {
			t.Fatal(err)
		}
		sub, err := SubstituteRule(r, r.Spec.Params)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.CompileTree(toTree(t, sub.Spec.Actions), ProgramIndex{}); err != nil {
			t.Fatal(err)
		}
	}
}

func keys(idx ProgramIndex) []string {
	out := make([]string, 0, len(idx))
	for k := range idx {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

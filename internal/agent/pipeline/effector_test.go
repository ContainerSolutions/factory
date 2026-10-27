package pipeline

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

func TestEffectorConfigFor(t *testing.T) {
	ws := harness.Workspace{Repo: "/w/repo", Schema: "/w/result.schema.json"}
	tests := []struct {
		name string
		spec v1alpha1.TaskRunSpec
		want EffectorConfig
	}{
		{"nothing allowed", v1alpha1.TaskRunSpec{Provider: "gh"},
			EffectorConfig{Task: "t", Provider: "gh", Allow: []string{}, RepoDir: "/w/repo", Schema: "/w/result.schema.json"}},
		{"the repo from the target's ref", v1alpha1.TaskRunSpec{Target: &v1alpha1.Target{Provider: "gh2", Ref: "acme/api#7"},
			Effects: &v1alpha1.EffectsPolicy{Allow: []v1alpha1.EffectOp{v1alpha1.OpComment}, Labels: []string{"a:*"}}},
			EffectorConfig{Task: "t", Provider: "gh2", Repo: "acme/api", Allow: []string{"comment"}, Labels: []string{"a:*"}, RepoDir: "/w/repo", Schema: "/w/result.schema.json"}},
		{"the repo from the credential", v1alpha1.TaskRunSpec{Credentials: &v1alpha1.TaskCredentials{Git: &v1alpha1.GitCredentialSpec{Repo: "acme/docs"}},
			Output: &v1alpha1.OutputSpec{Outcomes: []string{"done"}}},
			EffectorConfig{Task: "t", Repo: "acme/docs", Allow: []string{}, Outcomes: []string{"done"}, RepoDir: "/w/repo", Schema: "/w/result.schema.json"}},
	}
	for _, tt := range tests {
		got := EffectorConfigFor(v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "t"}, Spec: tt.spec}, ws)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestValidateResult(t *testing.T) {
	spec := []byte(`{"type": "object", "required": ["data"]}`)
	if p := ValidateResult("t", "s", spec, []string{"ok"}, []byte(`{"status": "done", "outcome": "ok", "data": {}}`)); len(p) != 0 {
		t.Errorf("valid: %q", p)
	}
	if p := ValidateResult("t", "s", spec, []string{"ok"}, []byte(`{"status": "done", "outcome": "ok"}`)); len(p) != 1 || !strings.Contains(p[0], "data") {
		t.Errorf("no data: %q", p)
	}
	if p := ValidateResult("t", "s", []byte(`{`), nil, []byte(`{"status": "done"}`)); len(p) != 1 || !strings.Contains(p[0], "the output schema") {
		t.Errorf("a broken schema: %q", p)
	}
	gen, err := v1alpha1Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if p := ValidateResult("t", "s", gen[v1alpha1.ResultSchemaFile], nil, []byte(`{"status": "noop"}`)); len(p) != 0 {
		t.Errorf("the Result schema as the spec's: %q", p)
	}
}

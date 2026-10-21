package codex

import (
	"slices"
	"strings"
	"testing"
)

func TestNativeEffort(t *testing.T) {
	tests := []struct{ model, effort, want string }{
		{"gpt-5", "minimal", "minimal"},
		{"gpt-5", "low", "low"},
		{"gpt-5", "medium", "medium"},
		{"gpt-5", "high", "high"},
		{"gpt-5", "max", "high"},
		{"gpt-5-codex", "minimal", "low"},
		{"gpt-5-codex", "max", "high"},
		{"gpt-5.1-codex-mini", "minimal", "low"},
		{"gpt-5.1-codex-max", "max", "xhigh"},
		{"gpt-5.1-codex-max", "minimal", "low"},
		{"gpt-5.1-codex-max-2026-01", "max", "xhigh"},
		{"", "max", "high"},
		{"gpt-5", "turbo", "turbo"},
	}
	for _, tt := range tests {
		if got := nativeEffort(tt.model, tt.effort); got != tt.want {
			t.Errorf("nativeEffort(%q, %q) = %q, want %q", tt.model, tt.effort, got, tt.want)
		}
	}
}

// schemaArgs are the --output-schema arguments execArgs passes for p: none while
// useOutputSchema is off.
func schemaArgs(p string) []string {
	if !useOutputSchema {
		return nil
	}
	return []string{flagOutputSchema, p}
}

// checkSchemaArgs fails when args don't pass the schema p as useOutputSchema says.
func checkSchemaArgs(t *testing.T, args []string, p string) {
	t.Helper()
	i := slices.Index(args, flagOutputSchema)
	switch {
	case useOutputSchema && (i < 0 || i+1 >= len(args) || args[i+1] != p):
		t.Errorf("args %q don't pass the schema %s", args, p)
	case !useOutputSchema && i >= 0:
		t.Errorf("args %q pass %s while useOutputSchema is off", args, flagOutputSchema)
	}
}

func TestExecArgs(t *testing.T) {
	base := []string{"exec", "--json"}
	tail := func(extra ...string) []string { return append(extra, "-") }
	tests := []struct {
		name string
		o    execOptions
		want []string
	}{
		{"read-write with schema", execOptions{Model: "gpt-5-codex", Effort: "high", Sandbox: sandboxWorkspaceWrite, WorkDir: "/w/repo", Schema: "/w/s.json", LastMessage: "/w/last"},
			append(slices.Clone(base), append([]string{"--model", "gpt-5-codex", "-c", `model_reasoning_effort="high"`, "-c", `approval_policy="never"`,
				"--sandbox", "workspace-write", "--cd", "/w/repo", "--skip-git-repo-check"}, append(schemaArgs("/w/s.json"), tail("--output-last-message", "/w/last")...)...)...)},
		{"read-only checkout: workspace-write from out/, max on a codex-max model", execOptions{Model: "gpt-5.1-codex-max", Effort: "max", Sandbox: sandboxWorkspaceWrite, WorkDir: "/w/out"},
			append(slices.Clone(base), tail("--model", "gpt-5.1-codex-max", "-c", `model_reasoning_effort="xhigh"`, "-c", `approval_policy="never"`,
				"--sandbox", "workspace-write", "--cd", "/w/out", "--skip-git-repo-check")...)},
		{"minimal on a codex model, no workdir", execOptions{Model: "gpt-5-codex", Effort: "minimal", Sandbox: sandboxWorkspaceWrite},
			append(slices.Clone(base), tail("--model", "gpt-5-codex", "-c", `model_reasoning_effort="low"`, "-c", `approval_policy="never"`,
				"--sandbox", "workspace-write", "--skip-git-repo-check")...)},
		{"no model or effort: codex's defaults", execOptions{Sandbox: sandboxWorkspaceWrite},
			append(slices.Clone(base), tail("-c", `approval_policy="never"`, "--sandbox", "workspace-write", "--skip-git-repo-check")...)},
		{"resume", execOptions{Model: "gpt-5", Effort: "medium", Sandbox: sandboxWorkspaceWrite, WorkDir: "/w/repo", LastMessage: "/w/last", Resume: "thread-1"},
			append(slices.Clone(base), tail("--model", "gpt-5", "-c", `model_reasoning_effort="medium"`, "-c", `approval_policy="never"`,
				"--sandbox", "workspace-write", "--cd", "/w/repo", "--skip-git-repo-check", "--output-last-message", "/w/last", "resume", "thread-1")...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := execArgs(tt.o); !slices.Equal(got, tt.want) {
				t.Errorf("execArgs =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// TestExecArgsEveryEffort: every normalised effort maps to a value codex takes, for every
// model family the mapping knows about.
func TestExecArgsEveryEffort(t *testing.T) {
	native := []string{"minimal", "low", "medium", "high", "xhigh"}
	for _, model := range []string{"gpt-5", "gpt-5-codex", "gpt-5.1-codex-max", "o4-mini"} {
		for effort := range effortMapping {
			args := execArgs(execOptions{Model: model, Effort: effort})
			i := slices.Index(args, "-c")
			if i < 0 || !strings.HasPrefix(args[i+1], keyEffort+"=") {
				t.Fatalf("%s/%s: no effort in %q", model, effort, args)
			}
			v := strings.Trim(strings.TrimPrefix(args[i+1], keyEffort+"="), `"`)
			if !slices.Contains(native, v) {
				t.Errorf("%s/%s: effort %q", model, effort, v)
			}
		}
	}
}

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		"plain":           `"plain"`,
		`C:\path "x"`:     `"C:\\path \"x\""`,
		"line\nbreak\tab": `"line\nbreak\tab"`,
		"bell\x07":        `"bell\u0007"`,
		"ünïcode":         `"ünïcode"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
	if got := tomlList([]string{"a", `b"`}); got != `["a", "b\""]` {
		t.Errorf("tomlList = %s", got)
	}
}

func TestCapabilities(t *testing.T) {
	c := New(Options{}).Capabilities()
	if c.Harness != "codex" || c.EffortMapping["max"] != "high" || c.EffortMapping["minimal"] != "minimal" || !slices.Contains(c.Features, "mcp") {
		t.Errorf("capabilities %+v", c)
	}
	c.EffortMapping["max"] = "changed"
	if effortMapping["max"] != "high" {
		t.Error("Capabilities shares the mapping")
	}
}

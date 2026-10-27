package cel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

func TestMergeParams(t *testing.T) {
	declared := map[string]any{"queue": "triage", "maxImplementRuns": float64(2), "harness": map[string]any{"name": "codex"}}
	defaults := map[string]any{"queue": "default", "undeclared": "ignored"}
	overrides := map[string]any{"harness": map[string]any{"name": "claude-code"}}
	got, err := MergeParams(declared, defaults, overrides)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"queue": "default", "maxImplementRuns": float64(2), "harness": map[string]any{"name": "claude-code"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if declared["queue"] != "triage" {
		t.Error("MergeParams modified its input")
	}
	// Overrides beat defaults.
	got, err = MergeParams(declared, defaults, map[string]any{"queue": "fast"})
	if err != nil || got["queue"] != "fast" {
		t.Errorf("got %v, %v", got, err)
	}
	// An override of an undeclared param is an error.
	_, err = MergeParams(declared, nil, map[string]any{"nope": 1, "queue": "x", "alsoNope": 2})
	var pe *ParamError
	if !errors.Is(err, ErrUnknownParam) || !errors.As(err, &pe) || pe.Param != "alsoNope" {
		t.Errorf("got %v", err)
	}
}

func TestSubstitute(t *testing.T) {
	params := map[string]any{
		"queue": "triage", "n": float64(3), "ratio": 0.5, "on": true, "none": nil,
		"harness": map[string]any{"name": "codex", "config": map[string]any{"model": "gpt-5*"}},
		"list":    []any{"a", "b"},
		"tricky":  "{{ params.queue }}",
		"cel":     "=event.repo",
	}
	tests := []struct {
		name string
		in   any
		want any
	}{
		{"whole value keeps the type", "{{ params.harness }}", map[string]any{"name": "codex", "config": map[string]any{"model": "gpt-5*"}}},
		{"whole value number", "{{params.n}}", float64(3)},
		{"whole value null", "{{ params.none }}", nil},
		{"whole value list", "{{ params.list }}", []any{"a", "b"}},
		{"whitespace inside the braces", "{{   params.queue\t}}", "triage"},
		{"interpolation", "queue {{ params.queue }} x{{ params.n }} {{ params.ratio }} {{ params.on }}", "queue triage x3 0.5 true"},
		{"two placeholders are a string", "{{ params.queue }}{{ params.queue }}", "triagetriage"},
		{"surrounding space makes it a string", " {{ params.n }}", " 3"},
		{"substituted text is not rescanned", "{{ params.tricky }}!", "{{ params.queue }}!"},
		{"a param can produce CEL", "{{ params.cel }}", "=event.repo"},
		{"CEL with an embedded param", "=event.repo == '{{ params.queue }}'", "=event.repo == 'triage'"},
		{"no placeholder", "plain {{ not closed", "plain {{ not closed"},
		{"tree", map[string]any{"{{ params.queue }}": []any{"{{ params.n }}", float64(1), map[string]any{"q": "{{ params.queue }}"}}},
			map[string]any{"{{ params.queue }}": []any{float64(3), float64(1), map[string]any{"q": "triage"}}}},
		{"non-strings pass through", []any{float64(1), true, nil}, []any{float64(1), true, nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Substitute(tt.in, params)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}

	// A substituted structure is a copy.
	got, _ := Substitute("{{ params.harness }}", params)
	got.(map[string]any)["name"] = "changed"
	if params["harness"].(map[string]any)["name"] != "codex" {
		t.Error("substituted value shares the param")
	}
}

func TestSubstituteErrors(t *testing.T) {
	params := map[string]any{"queue": "triage", "harness": map[string]any{"name": "codex"}, "list": []any{1}, "none": nil}
	tests := []struct {
		name  string
		in    any
		cause error
		path  string
	}{
		{"unknown param", "{{ params.nope }}", ErrUnknownParam, ""},
		{"unknown param in a string", map[string]any{"a": []any{"x {{ params.nope }}"}}, ErrUnknownParam, "a[0]"},
		{"not params", "{{ inputs.x }}", ErrPlaceholder, ""},
		{"go template syntax", "{{ .run.result }}", ErrPlaceholder, ""},
		{"nested selection", "{{ params.harness.name }}", ErrPlaceholder, ""},
		{"empty", "{{}}", ErrPlaceholder, ""},
		{"map in a string", "h={{ params.harness }}", ErrInterpolate, ""},
		{"list in a string", "{{ params.list }}s", ErrInterpolate, ""},
		{"null in a string", "x{{ params.none }}", ErrInterpolate, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Substitute(tt.in, params)
			var pe *ParamError
			if !errors.Is(err, tt.cause) || !errors.As(err, &pe) || pe.Path != tt.path {
				t.Fatalf("got %v (%#v), want %v at %q", err, pe, tt.cause, tt.path)
			}
		})
	}
	// Every failing leaf is reported.
	_, err := Substitute(map[string]any{"a": "{{ params.x }}", "b": "{{ params.y }}"}, params)
	if errs := err.(interface{ Unwrap() []error }).Unwrap(); len(errs) != 2 {
		t.Errorf("got %v", err)
	}
}

func TestSubstituteRule(t *testing.T) {
	var rule *v1alpha1.Rule
	var ruleSet *v1alpha1.RuleSet
	for _, obj := range loadExamples(t, "03-0[67]-*.yaml") {
		switch o := obj.(type) {
		case *v1alpha1.Rule:
			rule = o
		case *v1alpha1.RuleSet:
			ruleSet = o
		}
	}
	if rule.Spec.Actions[0].Dispatch.Harness.Expr != "{{ params.harness }}" {
		t.Fatalf("fixture changed: %+v", rule.Spec.Actions[0].Dispatch.Harness)
	}

	// The rule's own params: harness becomes the object, queue the string.
	sub, err := SubstituteRule(rule, rule.Spec.Params)
	if err != nil {
		t.Fatal(err)
	}
	d := sub.Spec.Actions[0].Dispatch
	if d.Queue != "triage" || d.Harness.IsExpr() || d.Harness.Value.Name != "codex" ||
		d.Harness.Value.Config == nil || d.Harness.Value.Config.Model.Values[0] != "gpt-5*" {
		t.Errorf("got dispatch %+v harness %+v", d, d.Harness)
	}
	if !reflect.DeepEqual(d.Context, []string{"event.object"}) || sub.Spec.Actions[1].Set["triage"] != "running" {
		t.Errorf("other values changed: %+v", sub.Spec.Actions)
	}
	if rule.Spec.Actions[0].Dispatch.Queue != "{{ params.queue }}" || !rule.Spec.Actions[0].Dispatch.Harness.IsExpr() {
		t.Error("SubstituteRule modified its input")
	}

	// Through the RuleSet: its defaults replace the rule's default queue, and an override
	// changes the harness.
	params, err := MergeParams(rule.Spec.Params, ruleSet.Spec.Defaults, map[string]any{
		"harness": map[string]any{"name": "claude-code", "config": map[string]any{"model": "claude-{sonnet,opus}-5-*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sub, err = SubstituteRule(rule, params)
	if err != nil {
		t.Fatal(err)
	}
	if d := sub.Spec.Actions[0].Dispatch; d.Queue != "default" || d.Harness.Value.Name != "claude-code" {
		t.Errorf("got %+v %+v", d, d.Harness)
	}

	// A param of the wrong type for its field.
	for name, p := range map[string]map[string]any{
		"harness as a plain string": {"queue": "q", "harness": "codex"},
		"queue as a number":         {"queue": float64(1), "harness": map[string]any{"name": "codex"}},
		"harness with a bad field":  {"queue": "q", "harness": map[string]any{"nme": "codex"}},
	} {
		_, err := SubstituteRule(rule, p)
		var pe *ParamError
		if !errors.Is(err, ErrParamType) || !errors.As(err, &pe) || pe.Path != "spec.actions" {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// A harness param that is itself an expression stays an expression.
	sub, err = SubstituteRule(rule, map[string]any{"queue": "q", "harness": "=params.h"})
	if err != nil || sub.Spec.Actions[0].Dispatch.Harness.Expr != "=params.h" {
		t.Errorf("got %v, %v", sub, err)
	}
	// An unknown param in the actions.
	_, err = SubstituteRule(rule, map[string]any{"queue": "q"})
	var pe *ParamError
	if !errors.Is(err, ErrUnknownParam) || !errors.As(err, &pe) || pe.Path != "spec.actions[0].dispatch.harness" || pe.Param != "harness" {
		t.Errorf("got %v", err)
	}
}

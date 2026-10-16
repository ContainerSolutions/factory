package cel

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCELSourceAndUnescape(t *testing.T) {
	tests := []struct {
		in        string
		src       string
		isCEL     bool
		unescaped string
	}{
		{in: "=event.object.ref", src: "event.object.ref", isCEL: true, unescaped: "=event.object.ref"},
		{in: "=", src: "", isCEL: true, unescaped: "="},
		{in: "main", unescaped: "main"},
		{in: `\=not cel`, unescaped: "=not cel"},
		{in: `\\=one backslash`, unescaped: `\=one backslash`},
		{in: `\n stays`, unescaped: `\n stays`},
		{in: " =leading space is literal", unescaped: " =leading space is literal"},
		{in: "", unescaped: ""},
	}
	for _, tt := range tests {
		src, ok := CELSource(tt.in)
		if ok != tt.isCEL || src != tt.src {
			t.Errorf("CELSource(%q) = %q, %v; want %q, %v", tt.in, src, ok, tt.src, tt.isCEL)
		}
		if got := Unescape(tt.in); got != tt.unescaped {
			t.Errorf("Unescape(%q) = %q, want %q", tt.in, got, tt.unescaped)
		}
	}
}

func TestEvalTree(t *testing.T) {
	env := newTestEnv(t)
	// build-stack-next's dispatch inputs and set, plus nesting, escapes and other literals.
	tree := map[string]any{
		"mode": "new", "target": "pr",
		"issue":     "=item.state.plan[item.state.stackNext].issue",
		"base":      "=run.result.branch",
		"stackNext": "=item.state.stackNext + 1",
		"checks":    []any{"style", "=run.inputs.mode", []any{"=1 + 1"}},
		"nested":    map[string]any{"verifyAgainst": "={'train': item.state.train}", "literal": `\=x`, "n": float64(3), "b": true, "null": nil},
		"empty":     map[string]any{},
	}
	idx := ProgramIndex{}
	if err := env.CompileTree(tree, idx); err != nil {
		t.Fatal(err)
	}
	if len(idx) != 6 {
		t.Errorf("compiled %d leaves, want 6: %v", len(idx), keys(idx))
	}
	got, err := EvalTree(context.Background(), tree, pocActivation(), idx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"mode": "new", "target": "pr",
		"issue":     "docs#2",
		"base":      "fx/docs-2",
		"stackNext": int64(2),
		"checks":    []any{"style", "new", []any{int64(2)}},
		"nested":    map[string]any{"verifyAgainst": map[string]any{"train": "compliance-framework/workflows#123"}, "literal": "=x", "n": float64(3), "b": true, "null": nil},
		"empty":     map[string]any{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	// The input is not modified.
	if tree["issue"] != "=item.state.plan[item.state.stackNext].issue" || tree["nested"].(map[string]any)["literal"] != `\=x` {
		t.Error("EvalTree modified its input")
	}

	// Scalars and non-tree values pass through.
	for _, v := range []any{"=1 + 2", "plain", float64(1), nil, []string{"=not a tree leaf"}} {
		idx := ProgramIndex{}
		if err := env.CompileTree(v, idx); err != nil {
			t.Fatal(err)
		}
		if _, err := EvalTree(context.Background(), v, Activation{}, idx); err != nil {
			t.Errorf("%v: %v", v, err)
		}
	}
}

func TestCompileTreeErrors(t *testing.T) {
	env := newTestEnv(t)
	tree := map[string]any{
		"a":    "=event.repo ==",
		"b":    []any{"ok", "=nope.x"},
		"good": "=event.repo",
	}
	idx := ProgramIndex{}
	err := env.CompileTree(tree, idx)
	var errs []*CompileError
	for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
		var ce *CompileError
		if !errors.As(e, &ce) {
			t.Fatalf("%v is not a *CompileError", e)
		}
		errs = append(errs, ce)
	}
	if len(errs) != 2 || errs[0].Path != "a" || errs[0].Code != CodeParse || errs[1].Path != "b[1]" || errs[1].Code != CodeCheck {
		t.Errorf("got %v", err)
	}
	if idx["event.repo"] == nil || len(idx) != 1 {
		t.Errorf("index %v, want only the good leaf", keys(idx))
	}
}

func TestEvalTreeErrors(t *testing.T) {
	env := newTestEnv(t)
	tree := map[string]any{"inputs": map[string]any{"issue": "=item.state.plan[0].isue", "z": "=1/0"}}
	idx := ProgramIndex{}
	if err := env.CompileTree(tree, idx); err != nil {
		t.Fatal(err)
	}
	_, err := EvalTree(context.Background(), tree, pocActivation(), idx)
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Path != "inputs.issue" || ee.Code != CodeEval || !errors.Is(err, ErrMissing) {
		t.Fatalf("got %v", err)
	}
	// A leaf missing from the index.
	_, err = EvalTree(context.Background(), []any{"=1 + 1"}, Activation{}, ProgramIndex{})
	if !errors.As(err, &ee) || ee.Path != "[0]" || !errors.Is(err, ErrNotCompiled) {
		t.Fatalf("got %v", err)
	}
}

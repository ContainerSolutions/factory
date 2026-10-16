package cel

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestActivationNumbers(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		name string
		in   any
		want any
	}{
		{"whole float64 is an int", float64(3), int64(3)},
		{"fractional float64 stays a double", 2.5, 2.5},
		{"negative zero is int 0", math.Copysign(0, -1), int64(0)},
		{"float64 beyond int64 stays a double", 1e19, 1e19},
		{"float32", float32(4), int64(4)},
		{"json.Number int", json.Number("42"), int64(42)},
		{"json.Number float", json.Number("0.25"), 0.25},
		{"json.Number whole float", json.Number("1e3"), int64(1000)},
		{"int", 7, int64(7)},
		{"nested in a typed slice", []map[string]float64{{"n": 1}}, []any{map[string]any{"n": int64(1)}}},
		{"typed map", map[string]int{"a": 1}, map[string]any{"a": int64(1)}},
		{"array", [2]string{"a", "b"}, []any{"a", "b"}},
		{"bytes", []byte("hi"), []byte("hi")},
		{"null", nil, nil},
	}
	p, err := env.Compile("inputs.v", Value)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.EvalValue(context.Background(), Activation{Inputs: map[string]any{"v": tt.in}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v (%T), want %#v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
	// An invalid json.Number is an evaluation error.
	_, err = p.EvalValue(context.Background(), Activation{Inputs: map[string]any{"v": json.Number("x")}})
	if !errors.As(err, new(*EvalError)) {
		t.Errorf("got %v", err)
	}
	// Whole numbers work with int arithmetic; fractions need doubles.
	q, _ := env.Compile("inputs.a + 1 == 4 && inputs.b + 0.5 == 3.0", Where)
	if ok, err := q.EvalBool(context.Background(), Activation{Inputs: map[string]any{"a": float64(3), "b": 2.5}}); err != nil || !ok {
		t.Errorf("got %v, %v", ok, err)
	}
}

func TestErrorMessages(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&CompileError{Code: CodeParse, Expr: "a ==", Path: "inputs.x", Issues: []Issue{{Line: 1, Column: 5, Message: "Syntax error"}, {Message: "more"}}},
			`inputs.x: cel.parse: "a ==": 1:5: Syntax error; more`},
		{&EvalError{Code: CodeEval, Expr: "run.spec", Line: 1, Column: 4, Message: "no such key: spec"},
			`cel.eval: "run.spec" at 1:4: no such key: spec`},
		{&EvalError{Code: CodeLimit, Expr: "x", Path: "set.a", Message: "cost limit 10 exceeded"},
			`set.a: cel.limit: "x": cost limit 10 exceeded`},
		{&ParamError{Path: "spec.actions[0].dispatch.queue", Message: "{{ params.q }}: no such param"},
			`spec.actions[0].dispatch.queue: params: {{ params.q }}: no such param`},
		{&ParamError{Message: `override "q" is not a declared param of the rule`},
			`params: override "q" is not a declared param of the rule`},
		{&FieldError{Expr: "event.relase", Field: "event.relase", Line: 1, Column: 6},
			`cel.field: "event.relase" at 1:6: unknown field event.relase`},
		{&FieldError{Expr: "x", Field: "event.y"}, `cel.field: "x": unknown field event.y`},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("got  %s\nwant %s", got, tt.want)
		}
	}
	if Where.String() != "where" || Value.String() != "value" || ExprKind(0).String() != "ExprKind(0)" {
		t.Error("ExprKind.String")
	}
}

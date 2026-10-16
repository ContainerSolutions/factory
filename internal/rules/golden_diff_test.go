package rules

import (
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func TestDiffGolden(t *testing.T) {
	dispatch := func(spec string, inputs map[string]any) ActionIntent {
		return ActionIntent{Dispatch: &Dispatch{TaskSpec: spec, Queue: "poc", Inputs: inputs}}
	}
	dec := func(rule string, a ActionIntent, v v1alpha1.RailVerdict) GoldenDecision {
		return GoldenDecision{Decision: Decision{RuleSet: "rs", Rule: rule, Action: a, Rail: v}, Inputs: []string{"item"}}
	}
	setX := ActionIntent{Set: &Set{Key: "x", Value: 1}}
	base := func() GoldenEvaluation {
		return GoldenEvaluation{
			Event: "e1", WorkItem: "wi",
			Matches: []GoldenMatch{
				{Match: Match{RuleSet: "rs", Rule: "a", Result: Matched}, Inputs: []string{"item"}},
				{Match: Match{RuleSet: "rs", Rule: "b", Result: NotMatched}, Inputs: []string{"item"}},
			},
			Decisions: []GoldenDecision{
				dec("a", setX, Allowed()),
				dec("a", dispatch("review", map[string]any{"target": "pr"}), Allowed()),
			},
			StateAfter: map[string]any{"x": 1, "plan": map[string]any{"layers": []any{map[string]any{"repo": "docs"}}}},
		}
	}
	tests := []struct {
		name   string
		change func(g *GoldenEvaluation)
		want   string
	}{
		{name: "same", change: func(*GoldenEvaluation) {}, want: ""},
		{
			name: "rail verdict",
			change: func(g *GoldenEvaluation) {
				g.Decisions[1].Rail = Suppressed(RailKillSwitch, "item has factory:stop")
			},
			want: "~ decision rs/a: dispatch review → poc [allowed] -> [suppressed kill-switch: item has factory:stop]",
		},
		{
			name: "rule stops matching",
			change: func(g *GoldenEvaluation) {
				g.Matches[0].Result = WhereError
				g.Matches[0].Error = "no such key: plan"
				g.Decisions = g.Decisions[:0]
				g.StateAfter = map[string]any{"plan": g.StateAfter["plan"]}
			},
			want: `~ rule rs/a: matched -> error: no such key: plan
- decision rs/a: set x [allowed]
    {"set":{"key":"x","value":1}}
- decision rs/a: dispatch review → poc [allowed]
    {"dispatch":{"taskSpec":"review","queue":"poc","inputs":{"target":"pr"}}}
- state x: 1`,
		},
		{
			name: "rule removed and added, priority",
			change: func(g *GoldenEvaluation) {
				g.Matches[1] = GoldenMatch{Match: Match{RuleSet: "rs", Rule: "c", Result: Duplicate, FiredVia: "other"}}
				g.Matches[0].Priority = 100
			},
			want: `~ rule rs/a
    priority: 0 -> 100
- rule rs/b: not-matched
+ rule rs/c: duplicate via other`,
		},
		{
			name: "decision reordered and input changed",
			change: func(g *GoldenEvaluation) {
				d := dispatch("review", map[string]any{"target": "stack", "checks": []any{"build"}})
				g.Decisions = []GoldenDecision{dec("a", d, Allowed()), g.Decisions[0]}
			},
			want: `~ decision rs/a: dispatch review → poc
    dispatch.inputs.checks: (none) -> ["build"]
    dispatch.inputs.target: "pr" -> "stack"
~ decision order
    was: rs/a: set x; rs/a: dispatch review → poc
    now: rs/a: dispatch review → poc; rs/a: set x`,
		},
		{
			name: "state and event",
			change: func(g *GoldenEvaluation) {
				g.Event = "e2"
				g.StateAfter = map[string]any{
					"x":    2,
					"plan": map[string]any{"layers": []any{map[string]any{"repo": "docs", "base": "main", "pr": "compliance-framework/docs#57"}}},
					"new":  "yes",
				}
			},
			want: `~ evaluation event: e1 -> e2
~ state plan
    plan.layers[0].base: (none) -> "main"
    plan.layers[0].pr: (none) -> "compliance-framework/docs#57"
~ state x: 1 -> 2
+ state new: "yes"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := base()
			tc.change(&g)
			var lines []string
			for _, c := range DiffGolden(base(), g) {
				lines = append(lines, c.Lines()...)
			}
			got := strings.Join(lines, "\n")
			if got != tc.want {
				t.Errorf("diff:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

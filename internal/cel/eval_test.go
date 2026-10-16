package cel

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pocActivation is a hand-built activation for the 10 rules. Maps hold JSON-decoded values
// (numbers are float64), as the store and the catalog produce them.
func pocActivation() Activation {
	return Activation{
		Event: map[string]any{
			"id": "ev-1", "type": "issue.closed", "source": "github-public",
			"repo":   "compliance-framework/workflows",
			"object": map[string]any{"kind": "issue", "ref": "compliance-framework/workflows#123", "number": float64(123)},
			"issue":  map[string]any{"labels": []any{"train:done"}, "title": "Release train 2026-10"},
			"train":  map[string]any{"month": "2026-10", "manifest": "repos.yaml", "dryRun": false},
			"release": map[string]any{
				"tag": "v1.2.0", "previousTag": "v1.1.3",
				"semver": map[string]any{"major": float64(1), "minor": float64(2), "patch": float64(0), "prerelease": false},
			},
			"review": map[string]any{"state": "changes_requested", "commit": "abc"},
		},
		Actor: map[string]any{"id": "github-public:ccf-factory", "tier": "maintainer", "isBot": true, "isFactory": true, "identity": "author"},
		Item: map[string]any{
			"id": "wi-42",
			"state": map[string]any{
				"release": "drafting", "train": "compliance-framework/workflows#123",
				"plan":      []any{map[string]any{"issue": "docs#1"}, map[string]any{"issue": "docs#2"}, map[string]any{"issue": "docs#3"}},
				"stackNext": float64(1), "stackTotal": float64(3), "review": "approved",
			},
			"counters": map[string]any{"implement": float64(2), "docs-review": 3},
			"runs": map[string]any{"implement": []any{
				map[string]any{"id": "run-1", "outcome": "pr-opened"},
				map[string]any{"id": "run-2", "outcome": "pr-updated"},
			}},
			"pr": map[string]any{
				"draft": false, "head": "abc", "labels": []any{"docs"},
				"checks":     map[string]any{"state": "success", "settled": true},
				"approvedBy": []any{map[string]any{"identity": "reviewer", "commit": "abc"}},
			},
			"prs": []map[string]any{
				{"checks": map[string]any{"state": "success"}},
				{"checks": map[string]any{"state": "success"}},
			},
			"deploy": map[string]any{"state": "success", "containsMerge": true},
		},
		Run: map[string]any{
			"spec": "implement", "outcome": "pr-opened",
			"inputs": map[string]any{"mode": "new", "target": "pr"},
			"result": map[string]any{"branch": "fx/docs-2", "plan": []any{map[string]any{"issue": "docs#1"}, map[string]any{"issue": "docs#2"}}},
		},
		Params: map[string]any{"trainManifest": "repos.yaml", "allowDryRun": false, "maxImplementRuns": float64(3)},
		Inputs: map[string]any{"base": "fx/docs-1"},
		Task:   map[string]any{"id": "task-1", "target": map[string]any{"repo": "compliance-framework/docs"}},
		Now:    time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}
}

func TestEvalBool(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"empty where is true", "", true},
		{"docs-on-release-train", `event.repo == 'compliance-framework/workflows' && has(event.train) &&
			event.train.manifest == params.trainManifest &&
			(('train:done' in event.issue.labels && !event.train.dryRun) ||
			 (params.allowDryRun && 'train:dry-run' in event.issue.labels))`, true},
		{"review-release-docs", "actor.isFactory && has(item.state.release)", true},
		{"fix-release-docs-review: count from float counters", "has(item.state.release) && event.review.state == 'changes_requested' && item.count('implement') < 3", true},
		{"count at the cap", "item.count('docs-review') < 3", false},
		{"count of an unknown spec is 0", "item.count('merge') == 0", true},
		{"count against a float param", "item.count('implement') < params.maxImplementRuns", true},
		{"merge-release-docs", `has(item.state.release) &&
			item.pr.approvedBy.exists(a, a.identity == 'reviewer' && a.commit == item.pr.head) &&
			item.pr.checks.state == 'success' &&
			!item.pr.labels.exists(l, l in ['do-not-merge', 'factory:hold', 'factory:stop'])`, true},
		{"build-stack-next: arithmetic on JSON state", "run.spec == 'implement' && run.inputs.mode == 'new' && run.outcome == 'pr-opened' && item.state.stackNext + 1 < item.state.stackTotal", true},
		{"merge-whole-stack: typed list of maps", "has(item.state.plan) && item.state.review == 'approved' && item.prs.all(p, p.checks.state == 'success')", true},
		{"announce-ptbr-live", "has(item.state.plan) && !has(item.state.announced) && item.deploy.state == 'success' && item.deploy.containsMerge", true},
		{"plan-maintainer-epics", "actor.tier == 'maintainer'", true},
		{"review-human-prs", "!actor.isFactory && !actor.isBot && !item.pr.draft", false},
		{"per-release alternative", "!event.release.semver.prerelease", true},
		{"lastRun", "item.lastRun('implement').outcome == 'pr-updated'", true},
		{"lastRun of an unknown spec is null", "item.lastRun('merge') == null", true},
		{"hash sampling", "hash(item.id) % 10 == hash('wi-42') % 10", true},
		{"hash vector", "hash('') == 2166136261 && hash('a') == 3826002220", true},
		{"semver.bump minor", "semver.bump(event.release.previousTag, event.release.tag) == 'minor'", true},
		{"semver.bump major", "semver.bump('1.9.9', 'v2.0.0') == 'major'", true},
		{"semver.bump patch ignores prerelease", "semver.bump('v1.2.3', 'v1.2.4-rc.1') == 'patch'", true},
		{"semver.bump none", "semver.bump('v1.2.3-rc.1', 'v1.2.3') == 'none'", true},
		{"glob", "glob('compliance-framework/*', event.repo) && !glob('other/*', event.repo)", true},
		{"glob alternation", "glob('claude-{sonnet,opus}-5-*', 'claude-opus-5-1')", true},
		{"glob regex", "glob('re:^v[0-9]+', event.release.tag)", true},
		{"now from the activation", "now > timestamp('2026-10-16T00:00:00Z') && now < timestamp('2026-10-17T00:00:00Z')", true},
		{"standard functions", "event.repo.startsWith('compliance') && event.repo.contains('/') && event.repo.matches('^[a-z-]+/[a-z]+$') && size(item.state.plan) == 3", true},
		{"cross-type comparison", "event.object.number == 123 && event.object.number > 122.5", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := env.Compile(tt.src, Where)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.EvalBool(context.Background(), pocActivation())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvalValue(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		src  string
		want any
	}{
		{"event.object.ref", "compliance-framework/workflows#123"},
		{"{'train': item.state.train}", map[string]any{"train": "compliance-framework/workflows#123"}},
		{"size(run.result.plan)", int64(2)},
		{"run.result.plan", []any{map[string]any{"issue": "docs#1"}, map[string]any{"issue": "docs#2"}}},
		{"item.state.plan[0].issue", "docs#1"},
		{"item.state.stackNext + 1", int64(2)},
		{"item.state.plan[item.state.stackNext].issue", "docs#2"},
		{"run.result.branch", "fx/docs-2"},
		{"task.target.repo + '@' + inputs.base", "compliance-framework/docs@fx/docs-1"},
		{"event.repo + '@' + event.release.tag", "compliance-framework/workflows@v1.2.0"},
		{"event.object.number", int64(123)},
		{"1.5 * 2.0", 3.0},
		{"null", nil},
		{"now", time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)},
		{"duration('90s')", 90 * time.Second},
		{"[1u, b'x']", []any{uint64(1), []byte("x")}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			p, err := env.Compile(tt.src, Value)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.EvalValue(context.Background(), pocActivation())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		name     string
		src      string
		kind     ExprKind
		code     Code
		line     int
		col      int
		contains string
	}{
		{"syntax", "event.repo ==", Where, CodeParse, 1, 14, ""},
		{"syntax on line 2", "event.repo == 'x' &&\n  (", Where, CodeParse, 2, 4, ""},
		{"unknown variable", "evnt.repo == 'x'", Where, CodeCheck, 1, 1, "undeclared reference to 'evnt'"},
		{"unknown function", "sha256(item.id) == 'x'", Where, CodeCheck, 1, 7, "undeclared reference to 'sha256'"},
		{"wrong argument type", "hash(1) == 2", Where, CodeCheck, 1, 5, "no matching overload"},
		{"where must be bool", "size(item.state)", Where, CodeCheck, 0, 0, "where must return bool"},
		{"empty value", "", Value, CodeParse, 0, 0, "empty expression"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := env.Compile(tt.src, tt.kind)
			var ce *CompileError
			if !errors.As(err, &ce) {
				t.Fatalf("got %v, want *CompileError", err)
			}
			if ce.Code != tt.code || ce.Expr != tt.src || len(ce.Issues) == 0 {
				t.Fatalf("got %+v", ce)
			}
			is := ce.Issues[0]
			if is.Line != tt.line || is.Column != tt.col {
				t.Errorf("position %d:%d, want %d:%d (%v)", is.Line, is.Column, tt.line, tt.col, err)
			}
			if tt.contains != "" && !strings.Contains(is.Message, tt.contains) {
				t.Errorf("message %q does not contain %q", is.Message, tt.contains)
			}
		})
	}
	if _, err := env.Compile("true", ExprKind(9)); err == nil {
		t.Error("invalid kind compiled")
	}
}

func TestEvalErrors(t *testing.T) {
	big := make([]any, 20_000)
	for i := range big {
		big[i] = float64(i)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name  string
		env   []Option
		src   string
		kind  ExprKind
		ctx   context.Context
		act   func(*Activation)
		code  Code
		cause error
		line  int
	}{
		{name: "missing key: run fields on a non-run event", src: "run.spec == 'merge'", kind: Where,
			act: func(a *Activation) { a.Run = nil }, code: CodeEval, cause: ErrMissing, line: 1},
		{name: "missing key in a value", src: "item.state.plan[0].isue", kind: Value, code: CodeEval, cause: ErrMissing, line: 1},
		{name: "index out of range", src: "item.state.plan[7].issue", kind: Value, code: CodeEval, cause: ErrMissing, line: 1},
		{name: "wrong type", src: "item.state.release + 1", kind: Value, code: CodeEval, cause: ErrType, line: 1},
		{name: "where returns a non-bool", src: "item.state.release", kind: Where, code: CodeEval, cause: ErrType},
		{name: "function error", src: "semver.bump('one', 'v1.0.0') == 'major'", kind: Where, code: CodeEval, line: 1},
		{name: "bad glob", src: "glob('{a', 'a')", kind: Where, code: CodeEval, line: 1},
		{name: "count on a bad counter", src: "item.count('x') == 0", kind: Where,
			act: func(a *Activation) { a.Item = map[string]any{"counters": map[string]any{"x": "two"}} }, code: CodeEval},
		// The default cost limit; a long timeout so the race detector's slowdown can't hit it.
		{name: "cost limit", env: []Option{WithTimeout(time.Minute)}, src: "inputs.xs.all(x, x >= 0)", kind: Where,
			act: func(a *Activation) { a.Inputs = map[string]any{"xs": big} }, code: CodeLimit, cause: ErrCostLimit},
		{name: "timeout", env: []Option{WithTimeout(time.Nanosecond), WithCostLimit(1 << 40), WithInterruptCheckFrequency(1)},
			src: "inputs.xs.all(x, x >= 0)", kind: Where,
			act: func(a *Activation) { a.Inputs = map[string]any{"xs": big} }, code: CodeLimit, cause: ErrTimeout},
		{name: "caller canceled", src: "true", kind: Where, ctx: canceled, code: CodeCanceled, cause: context.Canceled},
		{name: "non-string map key in a value", src: "{1: 'a'}", kind: Value, code: CodeEval, cause: ErrType},
		{name: "type value is not YAML", src: "type(1)", kind: Value, code: CodeEval, cause: ErrType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, tt.env...)
			p, err := env.Compile(tt.src, tt.kind)
			if err != nil {
				t.Fatal(err)
			}
			a := pocActivation()
			if tt.act != nil {
				tt.act(&a)
			}
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			if tt.kind == Where {
				_, err = p.EvalBool(ctx, a)
			} else {
				_, err = p.EvalValue(ctx, a)
			}
			var ee *EvalError
			if !errors.As(err, &ee) {
				t.Fatalf("got %v, want *EvalError", err)
			}
			if ee.Code != tt.code || ee.Expr != tt.src {
				t.Errorf("got code %s expr %q, want %s %q (%v)", ee.Code, ee.Expr, tt.code, tt.src, err)
			}
			if tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Errorf("%v is not %v", err, tt.cause)
			}
			if tt.line != 0 && (ee.Line != tt.line || ee.Column == 0) {
				t.Errorf("position %d:%d, want line %d", ee.Line, ee.Column, tt.line)
			}
		})
	}
}

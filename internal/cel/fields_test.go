package cel

import (
	"reflect"
	"testing"
)

func TestUnknownFields(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		src  string
		want []string
	}{
		{"event.release.tag == 'v1' && has(item.state.anything.at.all)", nil},
		{"event.release.tagg == 'v1'", []string{"event.release.tagg"}},
		{"has(event.relase)", []string{"event.relase"}},
		{"item.pr.approvedBy.exists(a, a.identty == 'reviewer')", []string{"item.pr.approvedBy[].identty"}},
		{"item.prs.all(p, p.checks.stat == 'success')", []string{"item.prs[].checks.stat"}},
		{"event.train.repos[0].nme == 'api' || event.train.repos.exists(r, r.versions[0] == 'v1')", []string{"event.train.repos[].nme"}},
		{"event['release']['tagg'] == 'x'", []string{"event.release.tagg"}},
		{"event.repo.owner == 'x'", []string{"event.repo.owner"}},
		{"run.inputs.anything == run.result.whatever.deep && params.x == inputs.y", nil},
		{"task.target.repo + task.targt.repo == ''", []string{"task.targt"}},
		{"[1, 2].exists(event, event > 1)", nil}, // a comprehension variable shadows the variable
		{"actor.tier == 'maintainer' && actor.teir == 'x' && provider.labels.family == 'github'", []string{"actor.teir"}},
		{"item.count('implement') < 3 && item.lastRun('merge').anything == 1", nil},
		{"{'a': event.objct}.a == 1", []string{"event.objct"}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			p, err := env.Compile(tt.src, Where)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, fe := range p.UnknownFields() {
				if fe.Expr != tt.src || fe.Line != 1 || fe.Column == 0 {
					t.Errorf("bad error %+v", fe)
				}
				got = append(got, fe.Field)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVariables(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		src  string
		want []string
	}{
		{"task.target.repo + '@' + inputs.base", []string{"inputs", "task"}},
		{"now > timestamp('2026-01-01T00:00:00Z') && item.count('x') > 0", []string{"item", "now"}},
		{"[1].exists(event, event > 0)", []string{}},
		{"1 + 1", []string{}},
	}
	for _, tt := range tests {
		p, err := env.Compile(tt.src, Value)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Variables(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.src, got, tt.want)
		}
	}
	p, _ := env.Compile("", Where)
	if p.Variables() != nil || p.UnknownFields() != nil {
		t.Error("empty where references something")
	}
	if lo, hi, err := p.EstimateCost(); lo != 0 || hi != 0 || err != nil {
		t.Error("empty where has a cost")
	}
}

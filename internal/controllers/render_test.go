package controllers

import (
	"errors"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// TestCheckRunResult: the Result read back from a run fact (resultMap's view, or a fixture's
// data-only one) is validated against the run's TaskSpec schema.
func TestCheckRunResult(t *testing.T) {
	cat := loadPoC(t)
	plan := func(items ...map[string]any) map[string]any {
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = it
		}
		return map[string]any{"plan": out}
	}
	good := map[string]any{"issue": "compliance-framework/docs#71", "title": "Enable i18n"}
	untitled := map[string]any{"issue": "compliance-framework/docs#71"}
	res := func(data map[string]any) map[string]any {
		r := doneResult("decomposed", data)
		r.Task = "tsk_1"
		return resultMap(&r)
	}
	run := func(spec string, result map[string]any) v1alpha1.Event {
		f := map[string]any{"id": "run-1", "spec": spec, "outcome": "decomposed", "status": "done"}
		if result != nil {
			f["result"] = result
		}
		return v1alpha1.Event{Type: v1alpha1.EventFactoryRunCompleted, Data: v1alpha1.EventData{Facts: map[string]any{"run": f}}}
	}
	tests := []struct {
		name    string
		ev      v1alpha1.Event
		spec    string
		wantErr string // substring; "" for none
	}{
		{name: "resultMap of a valid result", ev: run("plan-epic", res(plan(good))), spec: "plan-epic"},
		{name: "resultMap of an invalid result", ev: run("plan-epic", res(plan(untitled))), spec: "plan-epic", wantErr: "missing property 'title'"},
		{name: "data fields only", ev: run("plan-epic", plan(good)), spec: "plan-epic"},
		{name: "data fields only, invalid", ev: run("plan-epic", plan(untitled)), spec: "plan-epic", wantErr: "missing property 'title'"},
		{name: "empty result", ev: run("plan-epic", map[string]any{}), spec: "plan-epic"},
		{name: "no result", ev: run("plan-epic", nil)},
		{name: "another event", ev: v1alpha1.Event{Type: v1alpha1.EventIssueOpened}},
		{name: "unknown TaskSpec", ev: run("nope", plan(good)), spec: "nope", wantErr: "not in the catalog"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := CheckRunResult(cat, tc.ev)
			if spec != tc.spec {
				t.Errorf("spec %q, want %q", spec, tc.spec)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("error %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error %v, want %q", err, tc.wantErr)
			}
		})
	}
	if _, err := CheckRunResult(cat, run("nope", map[string]any{})); !errors.Is(err, ErrNotInCatalog) {
		t.Errorf("unknown TaskSpec: %v, want ErrNotInCatalog", err)
	}
}

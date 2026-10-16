package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

func TestEventVocabulary(t *testing.T) {
	seen := map[EventType]bool{}
	for _, et := range EventTypes() {
		if seen[et] {
			t.Errorf("%s listed twice", et)
		}
		seen[et] = true
		providers := et.ProviderTypes()
		if len(providers) == 0 {
			t.Errorf("%s has no provider types", et)
		}
		isFactory := strings.HasPrefix(string(et), "factory.")
		if et.IsFactory() != isFactory {
			t.Errorf("%s: IsFactory() = %v", et, et.IsFactory())
		}
		if isFactory {
			if !reflect.DeepEqual(providers, []ProviderType{ProviderTypeFactory}) {
				t.Errorf("%s must map only to factory, got %v", et, providers)
			}
		} else if et.EmittedBy(ProviderTypeFactory) {
			t.Errorf("%s: the factory provider emits only factory.* types", et)
		}
		if !et.Known() {
			t.Errorf("%s: Known() = false", et)
		}
	}
	if len(seen) != 28 {
		t.Errorf("vocabulary has %d types, contracts.md lists 28", len(seen))
	}

	tests := []struct {
		et        EventType
		provider  ProviderType
		emitted   bool
		isFactory bool
	}{
		{EventIssueOpened, ProviderTypeGitHub, true, false},
		{EventPRReviewSubmitted, ProviderTypeGitHub, true, false},
		{EventDeploySucceeded, ProviderTypeGitHub, true, false},
		{EventMessageMention, ProviderTypeSlack, true, false},
		{EventMessageMention, ProviderTypeGitHub, false, false},
		{EventFactoryRunCompleted, ProviderTypeFactory, true, true},
		{EventFactoryRunCompleted, ProviderTypeGitHub, false, true},
		{EventFactoryCommandCancel, ProviderTypeSlack, false, true},
		{"factory.made.up", ProviderTypeFactory, false, true},
	}
	for _, tt := range tests {
		if got := tt.et.EmittedBy(tt.provider); got != tt.emitted {
			t.Errorf("%s.EmittedBy(%s) = %v", tt.et, tt.provider, got)
		}
		if got := tt.et.IsFactory(); got != tt.isFactory {
			t.Errorf("%s.IsFactory() = %v", tt.et, got)
		}
	}
	if EventType("pr.exploded").Known() || EventType("pr.exploded").ProviderTypes() != nil {
		t.Error("unknown type reported as known")
	}
	// ProviderTypes returns a copy.
	p := EventIssueOpened.ProviderTypes()
	p[0] = ProviderTypeSlack
	if EventIssueOpened.ProviderTypes()[0] != ProviderTypeGitHub {
		t.Error("ProviderTypes leaked the vocabulary")
	}
}

func TestEffectOpSide(t *testing.T) {
	tests := []struct {
		op   EffectOp
		side Side
		ok   bool
	}{
		{OpComment, SideAPI, true},
		{OpPRComment, SideAPI, true},
		{OpLabelAdd, SideAPI, true},
		{OpCreateIssue, SideAPI, true},
		{OpSubIssueAdd, SideAPI, true},
		{OpPRMerge, SideAPI, true},
		{OpReviewRequestChanges, SideAPI, true},
		{OpThreadResolve, SideAPI, true},
		{OpPushBranch, SideGit, true},
		{OpStackPush, SideGit, true},
		{OpStackRebase, SideGit, true},
		{"force-push", "", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.op), func(t *testing.T) {
			side, ok := tt.op.Side()
			if side != tt.side || ok != tt.ok || tt.op.Known() != tt.ok {
				t.Errorf("Side() = %q, %v; want %q, %v", side, ok, tt.side, tt.ok)
			}
		})
	}
	git := 0
	for _, op := range EffectOps() {
		if side, _ := op.Side(); side == SideGit {
			git++
		}
	}
	if n := len(EffectOps()); n != 20 || git != 3 {
		t.Errorf("%d ops (%d git), contracts.md lists 20 (3 git)", n, git)
	}
}

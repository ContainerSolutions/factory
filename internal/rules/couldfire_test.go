package rules

import (
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func TestCouldFireAndReadsItem(t *testing.T) {
	c := loadCatalog(t, "poc")
	const gh = "github-public"
	on := func(typ v1alpha1.EventType, kind, ref string) v1alpha1.Event {
		return v1alpha1.Event{ID: "e1", Type: typ, Source: gh, Data: v1alpha1.EventData{
			Object: &v1alpha1.ObjectRef{Provider: gh, Kind: kind, Ref: ref},
		}}
	}
	factory := v1alpha1.Event{ID: "e2", Type: v1alpha1.EventFactoryRunCompleted, Source: v1alpha1.FactoryProvider}
	linked := WorkItemView{Links: []v1alpha1.Link{{Provider: gh, Ref: "compliance-framework/docs#42", Kind: "issue"}}}
	outside := WorkItemView{Links: []v1alpha1.Link{{Provider: gh, Ref: "compliance-framework/local-dev#127", Kind: "pr"}}}

	for _, tc := range []struct {
		name               string
		ev                 v1alpha1.Event
		wi                 WorkItemView
		couldFire, readsIt bool
	}{
		// plan-maintainer-epics reads only actor.
		{name: "issue opened in scope", ev: on(v1alpha1.EventIssueOpened, "issue", "compliance-framework/docs#42"), couldFire: true},
		// review-human-prs reads item.pr.draft.
		{name: "pr opened in scope", ev: on(v1alpha1.EventPROpened, "pr", "compliance-framework/docs#50"), couldFire: true, readsIt: true},
		{name: "repo in no RuleSet's scope", ev: on(v1alpha1.EventPROpened, "pr", "compliance-framework/local-dev#127")},
		{name: "repo matched by no glob", ev: on(v1alpha1.EventIssueOpened, "issue", "compliance-framework/notmock-docs#1")},
		{name: "in scope, no rule on the type", ev: on(v1alpha1.EventPRLabeled, "pr", "compliance-framework/docs#50")},
		{name: "provider no RuleSet binds", ev: func() v1alpha1.Event {
			ev := on(v1alpha1.EventIssueOpened, "issue", "compliance-framework/docs#42")
			ev.Source, ev.Data.Object.Provider = "github-other", "github-other"
			return ev
		}()},
		{name: "factory event on a linked item", ev: factory, wi: linked, couldFire: true, readsIt: true},
		{name: "factory event on an unlinked item", ev: factory},
		{name: "factory event on an item out of scope", ev: factory, wi: outside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CouldFire(c, tc.ev, tc.wi); got != tc.couldFire {
				t.Errorf("CouldFire = %v, want %v", got, tc.couldFire)
			}
			if got := ReadsItem(c, tc.ev, tc.wi); got != tc.readsIt {
				t.Errorf("ReadsItem = %v, want %v", got, tc.readsIt)
			}
		})
	}
	if CouldFire(nil, factory, linked) || ReadsItem(nil, factory, linked) {
		t.Error("a nil catalog could fire")
	}
}

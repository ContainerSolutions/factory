package server

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/store"
)

// docsNext is the object the fake creates next in the docs repo (docs#42 is seeded).
const docsNext = docsRepo + "#43"

func prOpen(key string) apiclient.EffectRequest {
	return apiclient.EffectRequest{Op: v1alpha1.OpPROpen, IdempotencyKey: key,
		Args: map[string]any{"head": "factory/tsk_1", "base": "main", "title": "Docs for the train"}}
}

// linkingTask lets the task create issues as well.
func linkingTask(tk *v1alpha1.Task) {
	tk.Spec.Effects.Allow = append(tk.Spec.Effects.Allow, v1alpha1.OpCreateIssue)
}

func (e *env) storedWorkItem() (*v1alpha1.WorkItem, bool) {
	e.t.Helper()
	var wi v1alpha1.WorkItem
	switch err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: workItemID}, &wi); {
	case err == nil:
		return &wi, true
	case errors.Is(err, store.ErrNotFound):
		return nil, false
	default:
		e.t.Fatal(err)
		return nil, false
	}
}

// providerApply applies an effect on the fake as the Effector would, outside the API: the
// provider has it, the dispatcher has no record (a crash between Apply and the record).
func (e *env) providerApply(req apiclient.EffectRequest) {
	e.t.Helper()
	if _, err := e.gh.Apply(e.ctx, provider.EffectRequest{
		Op: req.Op, Target: v1alpha1.ObjectRef{Provider: ghProvider, Kind: "repo", Ref: docsRepo}, Args: req.Args,
		Identity: "author", IdempotencyKey: effectID("tsk_1", req.IdempotencyKey),
	}); err != nil {
		e.t.Fatal(err)
	}
}

// TestEffectLinksCreatedObject: the object a creating op makes joins the task's work item
// (04-flows.md: "linked to the same work item automatically"), once, and on a provider replay
// too; other ops link nothing, and a work item that's gone doesn't fail the effect.
func TestEffectLinksCreatedObject(t *testing.T) {
	primary := v1alpha1.Link{Provider: ghProvider, Ref: docsIssue, Kind: provider.KindIssue, Primary: true}
	pr := v1alpha1.Link{Provider: ghProvider, Ref: docsNext, Kind: provider.KindPR}
	tests := []struct {
		name string
		prep func(e *env)
		reqs []apiclient.EffectRequest
		// links are the work item's afterwards; nil: no work item.
		links []v1alpha1.Link
		// writes is how many requests changed the work item.
		writes int
		// replayed: the provider replayed the last Apply.
		replayed bool
	}{
		{name: "pr.open links the PR", reqs: []apiclient.EffectRequest{prOpen("pr")},
			links: []v1alpha1.Link{primary, pr}, writes: 1},
		{name: "a replay of the key links it once", reqs: []apiclient.EffectRequest{prOpen("pr"), prOpen("pr")},
			links: []v1alpha1.Link{primary, pr}, writes: 1},
		{name: "a provider replay (the PR already open) links it", prep: func(e *env) { e.providerApply(prOpen("pr")) },
			reqs: []apiclient.EffectRequest{prOpen("pr")}, links: []v1alpha1.Link{primary, pr}, writes: 1, replayed: true},
		{name: "a provider replay of a linked PR writes nothing",
			prep: func(e *env) {
				e.providerApply(prOpen("pr"))
				wi, _ := e.storedWorkItem()
				wi.Spec.Links = append(wi.Spec.Links, pr)
				if err := e.st.Update(e.ctx, wi); err != nil {
					e.t.Fatal(err)
				}
			},
			reqs: []apiclient.EffectRequest{prOpen("pr")}, links: []v1alpha1.Link{primary, pr}, writes: 0, replayed: true},
		{name: "create-issue links the issue",
			reqs:  []apiclient.EffectRequest{{Op: v1alpha1.OpCreateIssue, Args: map[string]any{"title": "Follow-up"}, IdempotencyKey: "issue"}},
			links: []v1alpha1.Link{primary, {Provider: ghProvider, Ref: docsNext, Kind: provider.KindIssue}}, writes: 1},
		{name: "label.add and comment link nothing",
			reqs: []apiclient.EffectRequest{
				{Op: v1alpha1.OpLabelAdd, Args: map[string]any{"labels": []any{"docs"}}, IdempotencyKey: "label"},
				{Op: v1alpha1.OpComment, Args: map[string]any{"body": "hi"}, IdempotencyKey: "comment"},
			},
			links: []v1alpha1.Link{primary}, writes: 0},
		{name: "a work item that's gone doesn't fail the effect",
			prep: func(e *env) {
				if err := e.st.Delete(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: workItemID}); err != nil {
					e.t.Fatal(err)
				}
			},
			reqs: []apiclient.EffectRequest{prOpen("pr")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, tok, _ := leased(t, linkingTask)
			if tt.prep != nil {
				tt.prep(e)
			}
			writes := 0
			for _, req := range tt.reqs {
				before, _ := e.storedWorkItem()
				r := e.effect(tok, req)
				if r.JSON200 == nil {
					t.Fatalf("%s: status %s: %s", req.Op, r.Status(), r.Body)
				}
				var eff v1alpha1.Effect
				if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindEffect, Name: r.JSON200.EffectId}, &eff); err != nil {
					t.Fatalf("%s not recorded: %v", req.Op, err)
				}
				if after, _ := e.storedWorkItem(); before != nil && after.Metadata.ResourceVersion != before.Metadata.ResourceVersion {
					writes++
				}
			}
			wi, ok := e.storedWorkItem()
			switch {
			case tt.links == nil && ok:
				t.Errorf("work item %+v, want none", wi)
			case tt.links != nil && !ok:
				t.Fatal("the work item is gone")
			case ok && !reflect.DeepEqual(wi.Spec.Links, tt.links):
				t.Errorf("links %+v, want %+v", wi.Spec.Links, tt.links)
			}
			if writes != tt.writes {
				t.Errorf("%d requests wrote the work item, want %d", writes, tt.writes)
			}
			applies := e.gh.Applies()
			if last := applies[len(applies)-1]; last.Result.Replayed != tt.replayed {
				t.Errorf("last Apply replayed = %v, want %v", last.Result.Replayed, tt.replayed)
			}
		})
	}
}

// TestEffectLinkedPRFiresReleaseDocsReview is the live failure: release-docs opens the docs PR
// through the Effector, and GitHub's pr.opened by the factory must find the task's work item
// (state.release set) so review-release-docs dispatches docs-review. A factory pr.opened about
// a PR no work item links still dispatches nothing.
func TestEffectLinkedPRFiresReleaseDocsReview(t *testing.T) {
	e, tok, _ := leased(t, nil)
	wi, _ := e.storedWorkItem()
	wi.Status.State = map[string]any{"release": "drafting", "train": "compliance-framework/workflows#123"}
	if err := e.st.Update(e.ctx, wi); err != nil {
		t.Fatal(err)
	}
	if r := e.effect(tok, prOpen("pr")); r.JSON200 == nil {
		t.Fatalf("pr.open: %s %s", r.Status(), r.Body)
	}
	if err := e.gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: docsRepo + "#44"}, PR: &provider.PRObservation{Head: "other"}}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{docsNext, docsRepo + "#44"} {
		raw, err := e.gh.RawEvent(fake.Webhook{Type: v1alpha1.EventPROpened, Sender: fake.Sender{Login: "ccf-factory[bot]"},
			Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: ref}})
		if err != nil {
			t.Fatal(err)
		}
		evs, err := e.gh.Normalise(e.ctx, raw)
		if err != nil || len(evs) != 1 || evs[0].Data.Actor == nil || !evs[0].Data.Actor.IsFactory {
			t.Fatalf("normalise %s: %+v %v", ref, evs, err)
		}
		if err := e.st.Create(e.ctx, &evs[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := e.rule.Reconcile(e.ctx, evs[0].ID); err != nil {
			t.Fatalf("reconcile pr.opened on %s: %v", ref, err)
		}
	}
	var tasks []*v1alpha1.Task
	if _, err := e.st.List(e.ctx, v1alpha1.KindTask, store.ListOptions{}, &tasks); err != nil {
		t.Fatal(err)
	}
	var reviews []string
	for _, tk := range tasks {
		if tk.Spec.TaskSpec.Name == "docs-review" {
			reviews = append(reviews, tk.Metadata.WorkItem+" "+tk.Metadata.Provenance.Rule)
		}
	}
	if want := []string{workItemID + " review-release-docs"}; !reflect.DeepEqual(reviews, want) {
		t.Errorf("docs-review tasks %v, want %v", reviews, want)
	}
	var wis []*v1alpha1.WorkItem
	if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &wis); err != nil || len(wis) != 1 {
		t.Errorf("%d work items (%v), want the task's only", len(wis), err)
	}
}

package controllers

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// racingStore runs race once, just before the first WorkItem Update it passes on: a
// concurrent writer, so that Update loses the compare-and-swap.
type racingStore struct {
	store.Store
	mu   sync.Mutex
	race func()
}

func (s *racingStore) Update(ctx context.Context, obj store.Object) error {
	if _, ok := obj.(*v1alpha1.WorkItem); ok {
		s.mu.Lock()
		f := s.race
		s.race = nil
		s.mu.Unlock()
		if f != nil {
			f()
		}
	}
	return s.Store.Update(ctx, obj)
}

func TestLinkObject(t *testing.T) {
	const wiID = "wi_1"
	issue := v1alpha1.Link{Provider: ghProvider, Ref: docsRepo + "#42", Kind: provider.KindIssue, Primary: true}
	pr := v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindPR, Ref: docsRepo + "#57"}
	prLink := v1alpha1.Link{Provider: ghProvider, Ref: pr.Ref, Kind: provider.KindPR}
	tests := []struct {
		name string
		// links are the stored work item's; nil: no work item.
		links []v1alpha1.Link
		ref   v1alpha1.ObjectRef
		// race updates the work item concurrently (its state) before LinkObject's write.
		race      bool
		added     bool
		wantLinks []v1alpha1.Link
	}{
		{name: "adds a non-primary link", links: []v1alpha1.Link{issue}, ref: pr, added: true, wantLinks: []v1alpha1.Link{issue, prLink}},
		{name: "already linked: no write", links: []v1alpha1.Link{issue, prLink}, ref: pr, wantLinks: []v1alpha1.Link{issue, prLink}},
		{name: "a ref without provider or kind", links: []v1alpha1.Link{issue}, ref: v1alpha1.ObjectRef{Ref: docsRepo + "#58"}, added: true,
			wantLinks: []v1alpha1.Link{issue, {Provider: ghProvider, Ref: docsRepo + "#58", Kind: provider.KindIssue}}},
		{name: "a lost compare-and-swap is retried", links: []v1alpha1.Link{issue}, ref: pr, race: true, added: true,
			wantLinks: []v1alpha1.Link{issue, prLink}},
		{name: "a work item that's gone is a no-op", ref: pr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mem := memory.New(store.Options{Logger: discardLogger()})
			s := &racingStore{Store: mem}
			if tt.links != nil {
				if err := mem.Create(ctx, &v1alpha1.WorkItem{
					TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem}, Metadata: v1alpha1.ObjectMeta{Name: wiID},
					Spec: v1alpha1.WorkItemSpec{Links: tt.links}, Status: &v1alpha1.WorkItemStatus{State: map[string]any{}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.race {
				s.race = func() {
					var wi v1alpha1.WorkItem
					if err := mem.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, &wi); err != nil {
						t.Error(err)
						return
					}
					wi.Status.State["review"] = "running"
					if err := mem.Update(ctx, &wi); err != nil {
						t.Error(err)
					}
				}
			}
			var before v1alpha1.WorkItem
			_ = mem.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, &before)
			added, err := LinkObject(ctx, s, wiID, tt.ref, ghProvider)
			if err != nil || added != tt.added {
				t.Fatalf("LinkObject = %v, %v; want %v", added, err, tt.added)
			}
			var after v1alpha1.WorkItem
			if err := mem.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, &after); err != nil {
				if tt.links == nil && isNotFoundErr(err) {
					return
				}
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Spec.Links, tt.wantLinks) {
				t.Errorf("links %+v, want %+v", after.Spec.Links, tt.wantLinks)
			}
			if !tt.added && after.Metadata.ResourceVersion != before.Metadata.ResourceVersion {
				t.Error("an unchanged work item was written")
			}
			if tt.race && after.Status.State["review"] != "running" {
				t.Errorf("state %v: the concurrent write was lost", after.Status.State)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer safe for a logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestUnlinkedFactoryEventIsLogged: a factory event about an object no work item links, for
// which no rule fires, is unexpected now that the Effector links what it creates: it is logged
// at Info. Anyone else's is not.
func TestUnlinkedFactoryEventIsLogged(t *testing.T) {
	const msg = "factory event about an object no work item links; skipped"
	tests := []struct {
		name string
		w    fake.Webhook
		info bool
	}{
		{"the factory's pr.opened (no rule fires)", fake.Webhook{Type: v1alpha1.EventPROpened, Sender: fake.Sender{Login: "ccf-factory[bot]"},
			Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: docsRepo + "#57"}}, true},
		{"the factory's pr.opened outside every scope (no rule could fire)", fake.Webhook{Type: v1alpha1.EventPROpened, Sender: fake.Sender{Login: "ccf-factory[bot]"},
			Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: "compliance-framework/elsewhere#3"}}, true},
		{"a stranger's issue", issueOpened(docsRepo+"#7", "stranger"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			var buf syncBuffer
			e.rule.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			e.seed(tt.w.Object.Kind, tt.w.Object.Ref, nil)
			evs := e.deliver(tt.w)
			e.processEvents()
			var wis []*v1alpha1.WorkItem
			if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &wis); err != nil || len(wis) != 0 {
				t.Fatalf("work items = %d (%v), want none", len(wis), err)
			}
			logged := buf.String()
			if got := strings.Contains(logged, msg); got != tt.info {
				t.Fatalf("logged at info %v, want %v:\n%s", got, tt.info, logged)
			}
			if tt.info && (!strings.Contains(logged, "event="+evs[0].ID) || !strings.Contains(logged, "ref="+tt.w.Object.Ref) ||
				!strings.Contains(logged, "type=pr.opened")) {
				t.Errorf("the log names no event, type or ref:\n%s", logged)
			}
		})
	}
}

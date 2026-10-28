package controllers

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/patterns"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

const (
	localDev     = "compliance-framework/local-dev" // a repo no PoC RuleSet scopes
	factoryLogin = "ccf-factory[bot]"               // the fake's author App (fake.DefaultIdentities)
)

// attrRecorder is a slog.Handler that keeps every record with its attributes.
type attrRecorder struct {
	mu   sync.Mutex
	recs []loggedRecord
}

type loggedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (l *attrRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *attrRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := loggedRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, rec)
	return nil
}

func (l *attrRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *attrRecorder) WithGroup(string) slog.Handler      { return l }

// dropLogs are the records about dropped events.
func (l *attrRecorder) dropLogs() []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(l.recs), func(r loggedRecord) bool { return r.attrs["reason"] == "" })
}

// syncStore is a store.Syncer, like the object store: it counts the syncs and runs onSync
// (another dispatcher's writes arriving) on each.
type syncStore struct {
	*memory.Store
	mu     sync.Mutex
	syncs  int
	onSync func(ctx context.Context) error
}

func (s *syncStore) Sync(ctx context.Context) error {
	s.mu.Lock()
	s.syncs++
	f := s.onSync
	s.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(ctx)
}

var _ store.Syncer = (*syncStore)(nil)

func (s *syncStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncs
}

// intakeEnv is the provider controller over the fake GitHub, a catalog the test picks and a
// store (memory, or a syncStore over it), logging to an attrRecorder. gh is the fake GitHub's
// Conn; nil when the test brings its own adapters.
type intakeEnv struct {
	ctx  context.Context
	clk  *clock.Fake
	mem  *memory.Store
	st   store.Store
	cat  *swapCatalog
	gh   *fake.Conn
	pc   *ProviderController
	logs *attrRecorder
}

func newIntakeEnv(t *testing.T, cat *catalog.Catalog, wrap func(*memory.Store) store.Store, adapters ...provider.Adapter) *intakeEnv {
	t.Helper()
	clk := clock.NewFake(t0)
	mem := memory.New(store.Options{Clock: clk, Logger: discardLogger()})
	var st store.Store = mem
	if wrap != nil {
		st = wrap(mem)
	}
	e := &intakeEnv{ctx: context.Background(), clk: clk, mem: mem, st: st, cat: newSwapCatalog(cat), logs: &attrRecorder{}}
	if adapters == nil {
		ad := fake.NewAdapter(fake.Options{Clock: clk})
		e.gh = ad.Conn(ghProvider)
		adapters = []provider.Adapter{ad}
	}
	reg, err := provider.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	if e.pc, err = NewProviderController(st, e.cat, reg, ProviderOptions{Clock: clk, Logger: slog.New(e.logs)}); err != nil {
		t.Fatal(err)
	}
	if err := e.pc.Connect(e.ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

// emit delivers w and reconciles its delivery, returning the delivery id.
func (e *intakeEnv) emit(t *testing.T, w fake.Webhook) string {
	t.Helper()
	raws, err := e.gh.Emit(e.ctx, e.pc, w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pc.Reconcile(e.ctx, raws[0].ID); err != nil {
		t.Fatalf("reconcile delivery %s: %v", raws[0].ID, err)
	}
	return raws[0].ID
}

func (e *intakeEnv) delivery(t *testing.T, id string) *Delivery {
	t.Helper()
	var d Delivery
	switch err := e.mem.Get(e.ctx, store.Key{Kind: KindDelivery, Name: id}, &d); {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		t.Fatal(err)
	}
	return &d
}

// has reports whether the store has an Event (kind KindEvent) or an EventStatus named id.
func (e *intakeEnv) has(t *testing.T, kind, id string) bool {
	t.Helper()
	var obj store.Object = &v1alpha1.Event{}
	if kind == KindEventStatus {
		obj = &EventStatus{}
	}
	switch err := e.mem.Get(e.ctx, store.Key{Kind: kind, Name: id}, obj); {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		t.Fatal(err)
	}
	return true
}

// link stores a work item that links ref, with an id workItemFor wouldn't give it.
func (e *intakeEnv) link(t *testing.T, ctx context.Context, kind, ref string) string {
	t.Helper()
	id := objectID("wi", "test/"+ref)
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: id},
		Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: ghProvider, Kind: kind, Ref: ref, Primary: true}}},
		Status:   &v1alpha1.WorkItemStatus{State: map[string]any{}},
	}
	if err := e.mem.Create(ctx, wi); err != nil {
		t.Fatal(err)
	}
	return id
}

// mapRuleSets returns a copy of c whose RuleSets are copies changed by f.
func mapRuleSets(c *catalog.Catalog, f func(*catalog.CompiledRuleSet)) *catalog.Catalog {
	cp := *c
	cp.RuleSets = map[string]*catalog.CompiledRuleSet{}
	for name, rs := range c.RuleSets {
		r := *rs
		r.Rules = slices.Clone(rs.Rules)
		f(&r)
		cp.RuleSets[name] = &r
	}
	return &cp
}

func mustPatterns(t *testing.T, globs ...string) []patterns.Pattern {
	t.Helper()
	out := make([]patterns.Pattern, 0, len(globs))
	for _, g := range globs {
		p, err := patterns.Compile(g)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestIntakeFilter: an event is stored when a work item links its object or a rule could fire
// for it (its where aside); anything else is dropped, logged and counted, and a delivery left
// with no stored event is deleted at once (P3 §Only events that can matter are stored).
func TestIntakeFilter(t *testing.T) {
	poc := loadPoC(t)
	otherProviders := mapRuleSets(poc, func(rs *catalog.CompiledRuleSet) {
		for i, r := range rs.Rules {
			cp := *r
			cp.Providers = []string{"github-other"}
			rs.Rules[i] = &cp
		}
	})
	mainOnly := mapRuleSets(poc, func(rs *catalog.CompiledRuleSet) { rs.Scope.Branches = mustPatterns(t, "main") })
	docsChannel := mapRuleSets(poc, func(rs *catalog.CompiledRuleSet) {
		rs.Scope = catalog.CompiledScope{Channels: mustPatterns(t, "#docs")}
	})
	const secret = "Leaked: provider text that must not be logged"
	hook := func(typ v1alpha1.EventType, kind, ref string) fake.Webhook {
		w := webhook(typ, kind, ref, "alice")
		w.Subject = secret
		return w
	}
	withFacts := func(w fake.Webhook, k, v string) fake.Webhook {
		w.Facts = map[string]any{k: v}
		return w
	}
	byFactory := func(w fake.Webhook) fake.Webhook {
		w.Sender = fake.Sender{Login: factoryLogin}
		return w
	}
	inChannel := func(ch string) fake.Webhook {
		return fake.Webhook{Type: v1alpha1.EventIssueOpened, Sender: fake.Sender{Login: "alice"}, Subject: secret,
			Facts: map[string]any{"channel": ch}}
	}

	for _, tc := range []struct {
		name   string
		cat    *catalog.Catalog
		linked string // a work item links this ref
		hook   fake.Webhook
		reason string // "" when stored
	}{
		{name: "a rule could fire, its where false", cat: poc, hook: hook(v1alpha1.EventIssueOpened, provider.KindIssue, docsRepo+"#42")},
		{name: "out of scope, unlinked", cat: poc, hook: hook(v1alpha1.EventIssueOpened, provider.KindIssue, localDev+"#1"), reason: DropOutOfScope},
		{name: "no rule on the type, unlinked", cat: poc, hook: hook(v1alpha1.EventIssueCommented, provider.KindIssue, docsRepo+"#42"), reason: DropNoRule},
		{name: "linked, no rule on the type", cat: poc, linked: docsRepo + "#42", hook: hook(v1alpha1.EventIssueCommented, provider.KindIssue, docsRepo+"#42")},
		{name: "linked, out of scope", cat: poc, linked: localDev + "#1", hook: hook(v1alpha1.EventIssueOpened, provider.KindIssue, localDev+"#1")},
		{name: "another object of the repo linked", cat: poc, linked: docsRepo + "#7", hook: hook(v1alpha1.EventIssueCommented, provider.KindIssue, docsRepo+"#42"), reason: DropNoRule},
		{name: "outside the rules' providers", cat: otherProviders, hook: hook(v1alpha1.EventIssueOpened, provider.KindIssue, docsRepo+"#42"), reason: DropNoRule},
		{name: "branch outside the scope", cat: mainOnly, hook: withFacts(hook(v1alpha1.EventPROpened, provider.KindPR, docsRepo+"#50"), "branch", "dev"), reason: DropOutOfScope},
		{name: "branch in the scope", cat: mainOnly, hook: withFacts(hook(v1alpha1.EventPROpened, provider.KindPR, docsRepo+"#50"), "branch", "main")},
		{name: "channel outside the scope", cat: docsChannel, hook: inChannel("#random"), reason: DropOutOfScope},
		{name: "channel in the scope", cat: docsChannel, hook: inChannel("#docs")},
		// The factory's own events are kept unlinked: the Effector links its object after the
		// provider call (linkCreated), and the webhook can come first.
		{name: "by the factory, out of scope, unlinked", cat: poc, hook: byFactory(hook(v1alpha1.EventPROpened, provider.KindPR, localDev+"#9"))},
		{name: "by the factory, no rule on the type, unlinked", cat: poc, hook: byFactory(hook(v1alpha1.EventIssueCommented, provider.KindIssue, docsRepo+"#42"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newIntakeEnv(t, tc.cat, nil)
			if tc.linked != "" {
				e.link(t, e.ctx, provider.KindIssue, tc.linked)
			}
			id := e.emit(t, tc.hook)
			evID := id + "/0"
			d := e.delivery(t, id)
			if tc.reason == "" {
				if !e.has(t, v1alpha1.KindEvent, evID) {
					t.Fatalf("event %s not stored", evID)
				}
				if d == nil || d.Status.Phase != DeliveryNormalised || !slices.Equal(d.Status.Events, []string{evID}) {
					t.Fatalf("delivery %+v, want it normalised into %s", d, evID)
				}
				if drops := e.pc.Drops(); len(drops) != 0 || len(e.logs.dropLogs()) != 0 {
					t.Fatalf("drops %+v, logs %+v, want none", drops, e.logs.dropLogs())
				}
				return
			}
			if e.has(t, v1alpha1.KindEvent, evID) || e.has(t, KindEventStatus, evID) {
				t.Fatalf("dropped event %s stored", evID)
			}
			if d != nil {
				t.Fatalf("delivery %+v kept, want it deleted at once", d)
			}
			want := []DropCount{{Provider: ghProvider, Type: string(tc.hook.Type), Reason: tc.reason, Count: 1}}
			if got := e.pc.Drops(); !slices.Equal(got, want) {
				t.Fatalf("drops %+v, want %+v", got, want)
			}
			logs := e.logs.dropLogs()
			if len(logs) != 1 {
				t.Fatalf("drop logs %+v, want one", logs)
			}
			l := logs[0]
			wantRef := ""
			if tc.hook.Object != nil {
				wantRef = tc.hook.Object.Ref
			}
			if l.level != slog.LevelInfo || l.attrs["type"] != string(tc.hook.Type) || l.attrs["ref"] != wantRef ||
				l.attrs["reason"] != tc.reason || l.attrs["event"] != evID || l.attrs["provider"] != ghProvider {
				t.Fatalf("drop log %+v", l)
			}
			for k, v := range l.attrs {
				if v == secret {
					t.Fatalf("drop log has provider text in %s", k)
				}
			}
		})
	}
}

// A delivery fanned out into several events keeps the ones that can matter, and the delivery.
func TestIntakeFilterFanOut(t *testing.T) {
	e := newIntakeEnv(t, loadPoC(t), nil)
	e.link(t, e.ctx, provider.KindPR, localDev+"#50")
	id := e.emit(t, fake.Webhook{Type: v1alpha1.EventPRChecksCompleted, FanOut: []v1alpha1.ObjectRef{
		{Kind: provider.KindPR, Ref: localDev + "#50"}, {Kind: provider.KindPR, Ref: localDev + "#51"},
	}})
	if !e.has(t, v1alpha1.KindEvent, id+"/0") || e.has(t, v1alpha1.KindEvent, id+"/1") {
		t.Fatal("want #50's event stored and #51's dropped")
	}
	if d := e.delivery(t, id); d == nil || !slices.Equal(d.Status.Events, []string{id + "/0"}) {
		t.Fatalf("delivery %+v, want it kept with the stored event only", d)
	}
	want := []DropCount{{Provider: ghProvider, Type: string(v1alpha1.EventPRChecksCompleted), Reason: DropNoRule, Count: 1}} // the PoC excludes both merge rules
	if got := e.pc.Drops(); !slices.Equal(got, want) {
		t.Fatalf("drops %+v, want %+v", got, want)
	}
}

// unmappedAdapter connects fake Conns that map no delivery to an event (a push, say).
type unmappedAdapter struct{ *fake.Adapter }

func (a unmappedAdapter) Connect(ctx context.Context, p v1alpha1.Provider, sr provider.SecretResolver) (provider.Conn, error) {
	c, err := a.Adapter.Connect(ctx, p, sr)
	if err != nil {
		return nil, err
	}
	return unmappedConn{c}, nil
}

type unmappedConn struct{ provider.Conn }

func (unmappedConn) Normalise(context.Context, provider.RawEvent) ([]v1alpha1.Event, error) {
	return nil, nil
}

// A delivery of a type the adapter doesn't map (no events) is deleted at once; a delivery the
// adapter rejects keeps the delivery retention, for debugging.
func TestIntakeDeliveryRetention(t *testing.T) {
	t.Run("unmapped", func(t *testing.T) {
		e := newIntakeEnv(t, loadPoC(t), nil, unmappedAdapter{fake.NewAdapter(fake.Options{})})
		if err := e.pc.Accept(e.ctx, provider.RawEvent{ID: "push-1", Provider: ghProvider, Body: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if res, err := e.pc.Reconcile(e.ctx, "push-1"); err != nil || res.RequeueAfter != 0 {
			t.Fatalf("reconcile: %+v %v", res, err)
		}
		if d := e.delivery(t, "push-1"); d != nil {
			t.Fatalf("delivery %+v kept, want it deleted at once", d)
		}
		if drops := e.pc.Drops(); len(drops) != 0 {
			t.Fatalf("drops %+v: an unmapped delivery has no events to count", drops)
		}
	})
	t.Run("failed", func(t *testing.T) {
		e := newIntakeEnv(t, loadPoC(t), nil)
		if err := e.pc.Accept(e.ctx, provider.RawEvent{ID: "fake-bad", Provider: ghProvider, Body: []byte(`{"type":"nope"}`)}); err != nil {
			t.Fatal(err)
		}
		if res, err := e.pc.Reconcile(e.ctx, "fake-bad"); err != nil || res.RequeueAfter != DefaultDeliveryRetention {
			t.Fatalf("reconcile: %+v %v", res, err)
		}
		e.clk.Advance(DefaultDeliveryRetention - 1)
		if _, err := e.pc.Reconcile(e.ctx, "fake-bad"); err != nil {
			t.Fatal(err)
		}
		if d := e.delivery(t, "fake-bad"); d == nil || d.Status.Phase != DeliveryFailed {
			t.Fatalf("failed delivery %+v, want it kept for the retention", d)
		}
		e.clk.Advance(1)
		if _, err := e.pc.Reconcile(e.ctx, "fake-bad"); err != nil {
			t.Fatal(err)
		}
		if d := e.delivery(t, "fake-bad"); d != nil {
			t.Fatalf("failed delivery %+v kept past the retention", d)
		}
	})
}

// A redelivery of a dropped (so deleted) delivery is filtered again against the catalog as it
// is then: after a rule was added, its event is stored and the rule controller evaluates it.
func TestIntakeRedeliveryAfterRuleAdded(t *testing.T) {
	poc := loadPoC(t)
	empty := *poc
	empty.RuleSets = map[string]*catalog.CompiledRuleSet{}
	e := newIntakeEnv(t, &empty, nil)
	const ref = docsRepo + "#42"
	if err := e.gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindIssue, Ref: ref}}); err != nil {
		t.Fatal(err)
	}
	w := fake.Webhook{Delivery: "42", Type: v1alpha1.EventIssueOpened, Sender: fake.Sender{Login: maintainer, Tier: "maintainer"},
		Object: &v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: ref}}

	id := e.emit(t, w)
	if e.delivery(t, id) != nil || e.has(t, v1alpha1.KindEvent, id+"/0") {
		t.Fatal("with no rule, the event was stored or its delivery kept")
	}

	e.cat.swap(poc) // a rule is added
	if got := e.emit(t, w); got != id {
		t.Fatalf("redelivered as %s, want %s", got, id)
	}
	if !e.has(t, v1alpha1.KindEvent, id+"/0") {
		t.Fatal("the redelivered event wasn't stored")
	}
	rule, err := NewRuleController(e.st, e.cat, StaticConns{ghProvider: e.gh}, RuleOptions{Clock: e.clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rule.Reconcile(e.ctx, id+"/0"); err != nil {
		t.Fatal(err)
	}
	var st EventStatus
	if err := e.mem.Get(e.ctx, store.Key{Kind: KindEventStatus, Name: id + "/0"}, &st); err != nil || st.WorkItem == "" {
		t.Fatalf("event status %+v %v, want it processed into a work item", st, err)
	}
}

// With a store that syncs (the object store), a link lookup that finds nothing syncs once and
// asks again before the event is dropped or a work item created (P3 §Reads and freshness).
func TestIntakeSyncsBeforeDropping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ref        string
		linkOnSync bool
		syncErr    error
		wantKept   bool
		wantSyncs  int
	}{
		{name: "linked by another dispatcher", ref: localDev + "#7", linkOnSync: true, wantKept: true, wantSyncs: 1},
		{name: "still unlinked", ref: localDev + "#7", wantSyncs: 1},
		{name: "a rule could fire: no lookup", ref: docsRepo + "#7", wantKept: true},
		{name: "the sync fails", ref: localDev + "#7", syncErr: errors.New("bucket down"), wantSyncs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ss *syncStore
			e := newIntakeEnv(t, loadPoC(t), func(m *memory.Store) store.Store {
				ss = &syncStore{Store: m}
				return ss
			})
			ss.onSync = func(ctx context.Context) error {
				if tc.linkOnSync {
					e.link(t, ctx, provider.KindIssue, tc.ref)
				}
				return tc.syncErr
			}
			raws, err := e.gh.Emit(e.ctx, e.pc, webhook(v1alpha1.EventIssueOpened, provider.KindIssue, tc.ref, "alice"))
			if err != nil {
				t.Fatal(err)
			}
			id := raws[0].ID
			_, err = e.pc.Reconcile(e.ctx, id)
			if tc.syncErr != nil {
				if !errors.Is(err, tc.syncErr) {
					t.Fatalf("reconcile = %v, want the sync's error", err)
				}
				if d := e.delivery(t, id); d == nil || d.Status.Phase != DeliveryReceived {
					t.Fatalf("delivery %+v, want it kept for a retry", d)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := e.has(t, v1alpha1.KindEvent, id+"/0"); got != tc.wantKept {
				t.Errorf("event stored: %v, want %v", got, tc.wantKept)
			}
			if n := ss.count(); n != tc.wantSyncs {
				t.Errorf("syncs = %d, want %d", n, tc.wantSyncs)
			}
		})
	}
}

// workItemFor shares the lookup: a work item another dispatcher linked is found after the
// sync, rather than a second one created.
func TestWorkItemForSyncsBeforeCreating(t *testing.T) {
	var ss *syncStore
	e := newIntakeEnv(t, loadPoC(t), func(m *memory.Store) store.Store {
		ss = &syncStore{Store: m}
		return ss
	})
	const ref = docsRepo + "#42"
	if err := e.gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindIssue, Ref: ref}}); err != nil {
		t.Fatal(err)
	}
	var linked string
	ss.onSync = func(ctx context.Context) error {
		if linked == "" {
			linked = e.link(t, ctx, provider.KindIssue, ref)
		}
		return nil
	}
	ev := v1alpha1.Event{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent},
		ID:       "ev-1", Type: v1alpha1.EventIssueOpened, Source: ghProvider, Time: t0,
		Data: v1alpha1.EventData{Repo: docsRepo, Object: &v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindIssue, Ref: ref},
			Provider: ghProvider, Actor: &v1alpha1.Actor{ID: ghProvider + ":" + maintainer, Tier: "maintainer"}},
	}
	rule, err := NewRuleController(e.st, e.cat, StaticConns{ghProvider: e.gh}, RuleOptions{Clock: e.clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rule.workItemFor(e.ctx, e.cat.Current(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got != linked || ss.count() != 1 {
		t.Fatalf("work item %q after %d syncs, want %q after 1", got, ss.count(), linked)
	}
	var wis []*v1alpha1.WorkItem
	if _, err := e.mem.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &wis); err != nil || len(wis) != 1 {
		t.Fatalf("work items %d %v, want 1", len(wis), err)
	}
}

// The race between an effect and its link: the factory opens a PR (conn.Apply) and the
// Effector links it to the task's work item only after that returns (linkCreated). A
// pr.opened normalised in between is kept, though no rule could fire for it and nothing links
// it yet, and once the link exists the rule controller finds the work item.
func TestIntakeKeepsFactoryEventBeforeItsLink(t *testing.T) {
	e := newIntakeEnv(t, loadPoC(t), nil)
	const pr = localDev + "#9" // out of every PoC scope: CouldFire says no
	if err := e.gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindPR, Ref: pr},
		PR: &provider.PRObservation{Head: "abc"}}); err != nil {
		t.Fatal(err)
	}
	wiID := e.link(t, e.ctx, provider.KindIssue, localDev+"#8") // the task's work item

	// The webhook first.
	id := e.emit(t, fake.Webhook{Type: v1alpha1.EventPROpened, Sender: fake.Sender{Login: factoryLogin},
		Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: pr}})
	evID := id + "/0"
	if !e.has(t, v1alpha1.KindEvent, evID) {
		t.Fatal("the factory's pr.opened was dropped before its link")
	}
	if d := e.delivery(t, id); d == nil || !slices.Equal(d.Status.Events, []string{evID}) {
		t.Fatalf("delivery %+v, want it kept with the event", d)
	}
	if drops := e.pc.Drops(); len(drops) != 0 || len(e.logs.dropLogs()) != 0 {
		t.Fatalf("drops %+v, logs %+v, want none", drops, e.logs.dropLogs())
	}

	// Then linkCreated.
	if added, err := LinkObject(e.ctx, e.st, wiID, v1alpha1.ObjectRef{Provider: ghProvider, Kind: provider.KindPR, Ref: pr}, ghProvider); err != nil || !added {
		t.Fatalf("link: %v %v", added, err)
	}
	rule, err := NewRuleController(e.st, e.cat, StaticConns{ghProvider: e.gh}, RuleOptions{Clock: e.clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rule.Reconcile(e.ctx, evID); err != nil {
		t.Fatal(err)
	}
	var st EventStatus
	if err := e.mem.Get(e.ctx, store.Key{Kind: KindEventStatus, Name: evID}, &st); err != nil || st.WorkItem != wiID {
		t.Fatalf("event status %+v %v, want it processed for %s", st, err, wiID)
	}
}

// redeliveryStore runs beforeDelete (another dispatcher acting) before the first conditional
// delete, as a leader paused between its read and its delete would see it.
type redeliveryStore struct {
	*memory.Store
	beforeDelete func(ctx context.Context)
}

func (s *redeliveryStore) DeleteIfUnchanged(ctx context.Context, k store.Key, rv string) error {
	if f := s.beforeDelete; f != nil {
		s.beforeDelete = nil
		f(ctx)
	}
	return s.Store.DeleteIfUnchanged(ctx, k, rv)
}

// TestIntakeRedeliveryRacesTheDelete: a reconcile reads a delivery whose events are all
// dropped; before it deletes it, another leader deletes it and the delivery is redelivered
// under the same id. The stale delete is at the version read, so the redelivery survives, and
// is normalised in its turn.
func TestIntakeRedeliveryRacesTheDelete(t *testing.T) {
	var rs *redeliveryStore
	e := newIntakeEnv(t, loadPoC(t), func(m *memory.Store) store.Store {
		rs = &redeliveryStore{Store: m}
		return rs
	})
	w := webhook(v1alpha1.EventIssueOpened, provider.KindIssue, localDev+"#1", "alice") // out of scope: dropped
	raws, err := e.gh.Emit(e.ctx, e.pc, w)
	if err != nil {
		t.Fatal(err)
	}
	id := raws[0].ID
	rs.beforeDelete = func(ctx context.Context) {
		// The other leader deleted it, and GitHub redelivered it.
		if err := e.mem.Delete(ctx, store.Key{Kind: KindDelivery, Name: id}); err != nil {
			t.Fatal(err)
		}
		if err := e.pc.Accept(ctx, raws[0]); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
	}
	if _, err := e.pc.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if d := e.delivery(t, id); d == nil || d.Status.Phase != DeliveryReceived {
		t.Fatalf("the redelivery after the stale delete: %+v, want it kept, not yet normalised", d)
	}
	// Its own reconcile normalises it.
	if _, err := e.pc.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if d := e.delivery(t, id); d != nil {
		t.Fatalf("the redelivery after its own reconcile: %+v, want it normalised (dropped, then deleted)", d)
	}
	if drops := e.pc.Drops(); len(drops) != 1 || drops[0].Count != 2 {
		t.Fatalf("drops %+v, want the event dropped by both reconciles", drops)
	}
}

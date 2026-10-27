package controllers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/store"
)

// logRecorder is a slog.Handler that keeps every record's level and message.
type logRecorder struct {
	mu   sync.Mutex
	recs []string // "LEVEL message"
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Level.String()+" "+r.Message)
	return nil
}

func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

// count is how many records were "LEVEL message".
func (l *logRecorder) count(level slog.Level, msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.recs {
		if r == level.String()+" "+msg {
			n++
		}
	}
	return n
}

// countingConn counts the Observe calls that reach the provider, by ref.
type countingConn struct {
	provider.Conn
	mu       sync.Mutex
	observed map[string]int
}

func (c *countingConn) Observe(ctx context.Context, ref v1alpha1.ObjectRef) (provider.Observation, error) {
	c.mu.Lock()
	c.observed[ref.Ref]++
	c.mu.Unlock()
	return c.Conn.Observe(ctx, ref)
}

// observes is how many times ref was observed; "" sums every ref.
func (c *countingConn) observes(ref string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ref != "" {
		return c.observed[ref]
	}
	n := 0
	for _, v := range c.observed {
		n += v
	}
	return n
}

// newScopeEnv is newEnv with the rule controller observing through a countingConn and
// logging to a logRecorder.
func newScopeEnv(t *testing.T) (*env, *countingConn, *logRecorder) {
	t.Helper()
	e := newEnv(t)
	cc := &countingConn{Conn: e.gh, observed: map[string]int{}}
	logs := &logRecorder{}
	e.conns = StaticConns{ghProvider: cc}
	e.cache = NewObservationCache(e.conns, e.clk, 0)
	var err error
	e.rule, err = NewRuleController(e.st, staticCatalog{e.cat}, e.conns, RuleOptions{
		Clock: e.clk, Logger: slog.New(logs), Cache: e.cache, UIBaseURL: "https://factory.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, cc, logs
}

func eventStatus(e *env, id string) (EventStatus, bool) {
	var st EventStatus
	err := e.st.Get(e.ctx, store.Key{Kind: KindEventStatus, Name: id}, &st)
	return st, err == nil
}

func webhook(typ v1alpha1.EventType, kind, ref, login string) fake.Webhook {
	return fake.Webhook{Type: typ, Sender: fake.Sender{Login: login}, Object: &v1alpha1.ObjectRef{Kind: kind, Ref: ref}}
}

// A new work item is observed only when a rule that could fire reads item: an event no
// RuleSet covers (local-dev) costs no provider call at all.
func TestNewWorkItemObservesOnlyWhatRulesRead(t *testing.T) {
	const localDev = "compliance-framework/local-dev"
	for _, tc := range []struct {
		name     string
		kind     string
		ref      string
		typ      v1alpha1.EventType
		login    string
		wantWI   bool
		observes int
	}{
		{name: "issue out of scope", kind: provider.KindIssue, ref: localDev + "#126", typ: v1alpha1.EventIssueOpened, login: maintainer},
		{name: "pr out of scope", kind: provider.KindPR, ref: localDev + "#127", typ: v1alpha1.EventPROpened, login: "alice"},
		{name: "pr in scope: item.pr is read", kind: provider.KindPR, ref: docsRepo + "#50", typ: v1alpha1.EventPROpened, login: "alice", wantWI: true, observes: 1},
		// plan-maintainer-epics reads only actor: the item is observed once, to apply.
		{name: "issue in scope by a maintainer", kind: provider.KindIssue, ref: docsRepo + "#42", typ: v1alpha1.EventIssueOpened, login: maintainer, wantWI: true, observes: 1},
		{name: "issue in scope, no rule fires", kind: provider.KindIssue, ref: docsRepo + "#42", typ: v1alpha1.EventIssueOpened, login: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, cc, logs := newScopeEnv(t)
			e.seed(tc.kind, tc.ref, nil)
			evs := e.deliver(webhook(tc.typ, tc.kind, tc.ref, tc.login))
			if n := e.processEvents(); n != len(evs) {
				t.Fatalf("processed %d events, want %d", n, len(evs))
			}
			for _, ev := range evs {
				st, ok := eventStatus(e, ev.ID)
				if !ok {
					t.Fatalf("event %s not marked processed", ev.ID)
				}
				if (st.WorkItem != "") != tc.wantWI {
					t.Errorf("event %s: work item %q, want one: %v", ev.ID, st.WorkItem, tc.wantWI)
				}
			}
			if got := findWorkItem(e, tc.ref) != nil; got != tc.wantWI {
				t.Errorf("work item exists = %v, want %v", got, tc.wantWI)
			}
			if got := cc.observes(""); got != tc.observes {
				t.Errorf("Observe calls = %d, want %d", got, tc.observes)
			}
			if n := logs.count(slog.LevelWarn, "reconcile failed; retrying"); n != 0 {
				t.Errorf("%d retries logged", n)
			}
		})
	}
}

// A forbidden observation of a new work item's object is permanent: logged once and dropped,
// not retried. A 403 rate limit stays transient.
func TestForbiddenObservationOfNewWorkItem(t *testing.T) {
	forbidden := fmt.Errorf("github: graphql Observe: FORBIDDEN: Resource not accessible by integration: %w", provider.ErrForbidden)
	rateLimited := &provider.RetryableError{RetryAfter: time.Minute, Err: fmt.Errorf("github: Observe: 403 Forbidden: secondary rate limit: %w", provider.ErrForbidden)}
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{name: "forbidden", err: forbidden, permanent: true},
		{name: "403 rate limit", err: rateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, cc, logs := newScopeEnv(t)
			pr := docsRepo + "#50"
			e.seed(provider.KindPR, pr, nil)
			e.gh.FailNext(fake.MethodObserve, tc.err)
			stop := runInBackground(t, e.rule.Run)
			evs := e.deliver(webhook(v1alpha1.EventPROpened, provider.KindPR, pr, "alice"))
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			id := evs[0].ID
			q := e.rule.Controller().Queue()
			if tc.permanent {
				eventually(t, "the event marked processed", func() bool { _, ok := eventStatus(e, id); return ok })
			} else {
				eventually(t, "the retry scheduled", func() bool { return q.NumRequeues(id) == 1 })
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}

			st, processed := eventStatus(e, id)
			if processed != tc.permanent || st.WorkItem != "" {
				t.Errorf("processed = %v (work item %q), want %v with none", processed, st.WorkItem, tc.permanent)
			}
			if findWorkItem(e, pr) != nil {
				t.Error("a work item was created")
			}
			if got := cc.observes(pr); got != 1 {
				t.Errorf("Observe calls = %d, want 1", got)
			}
			wantDropped, wantRetried, wantRequeues := 1, 0, 0
			if !tc.permanent {
				wantDropped, wantRetried, wantRequeues = 0, 1, 1
			}
			if n := logs.count(slog.LevelWarn, "reconcile failed permanently; dropped"); n != wantDropped {
				t.Errorf("dropped logged %d times, want %d", n, wantDropped)
			}
			if n := logs.count(slog.LevelWarn, "reconcile failed; retrying"); n != wantRetried {
				t.Errorf("retrying logged %d times, want %d", n, wantRetried)
			}
			if r := q.NumRequeues(id); r != wantRequeues {
				t.Errorf("NumRequeues = %d, want %d", r, wantRequeues)
			}
		})
	}
}

// An existing work item whose link the provider won't show evaluates it as gone: its other
// links still count, and the refusal is reported once.
func TestExistingWorkItemWithForbiddenLink(t *testing.T) {
	e, cc, logs := newScopeEnv(t)
	issue, pr := docsRepo+"#42", docsRepo+"#50"
	e.seed(provider.KindIssue, issue, nil)
	e.seed(provider.KindPR, pr, nil)
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: "wi_forbidden-link"},
		Spec: v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{
			{Provider: ghProvider, Ref: issue, Kind: provider.KindIssue},
			{Provider: ghProvider, Ref: pr, Kind: provider.KindPR, Primary: true},
		}},
		Status: &v1alpha1.WorkItemStatus{State: map[string]any{}},
	}
	if err := e.st.Create(e.ctx, wi); err != nil {
		t.Fatal(err)
	}
	forbidden := fmt.Errorf("github: graphql Observe: FORBIDDEN: %w", provider.ErrForbidden)

	for i, typ := range []v1alpha1.EventType{v1alpha1.EventPROpened, v1alpha1.EventPRUpdated} {
		e.clk.Advance(DefaultObservationTTL)         // re-observe both links
		e.gh.FailNext(fake.MethodObserve, forbidden) // the first link observed: the issue
		evs := e.deliver(webhook(typ, provider.KindPR, pr, "alice"))
		e.processEvents() // fails the test on a reconcile error
		st, ok := eventStatus(e, evs[0].ID)
		if !ok || st.WorkItem != wi.Metadata.Name {
			t.Fatalf("%s: status %+v, %v; want applied to %s", typ, st, ok, wi.Metadata.Name)
		}
		if got := cc.observes(issue); got != i+1 {
			t.Errorf("%s: issue observed %d times, want %d", typ, got, i+1)
		}
	}
	// review-human-prs read item.pr (the PR link) and fired on both events.
	fired := 0
	for _, d := range e.getWorkItem(wi.Metadata.Name).Status.Decisions {
		if d.Rule == "review-human-prs" && d.Fired {
			fired++
		}
	}
	if fired != 2 {
		t.Errorf("review-human-prs fired %d times, want 2", fired)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "docs-review")); n != 2 {
		t.Errorf("docs-review tasks = %d, want 2", n)
	}
	if n := logs.count(slog.LevelWarn, "link observation forbidden; evaluating it as gone"); n != 1 {
		t.Errorf("forbidden link logged %d times, want 1", n)
	}

	// Once the link observes again, a new refusal is news.
	e.clk.Advance(DefaultObservationTTL)
	evs := e.deliver(webhook(v1alpha1.EventPRUpdated, provider.KindPR, pr, "alice"))
	e.processEvents()
	e.clk.Advance(DefaultObservationTTL)
	e.gh.FailNext(fake.MethodObserve, forbidden)
	evs = append(evs, e.deliver(webhook(v1alpha1.EventPRUpdated, provider.KindPR, pr, "alice"))...)
	e.processEvents()
	for _, ev := range evs {
		if _, ok := eventStatus(e, ev.ID); !ok {
			t.Errorf("event %s not processed", ev.ID)
		}
	}
	if n := logs.count(slog.LevelWarn, "link observation forbidden; evaluating it as gone"); n != 2 {
		t.Errorf("forbidden link logged %d times after it recovered and failed again, want 2", n)
	}
}

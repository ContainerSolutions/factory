package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

const (
	pocCatalog = "../catalog/testdata/poc"
	ghProvider = "github-public"
	docsRepo   = "compliance-framework/docs"
	pocQueue   = "poc"
	pocPool    = "local"
	pocRuleSet = "poc-docs-epic"
	workItem   = "wi_1"
	// waitTimeout guards every wait in these tests; nothing sleeps.
	waitTimeout = 10 * time.Second
)

// t0 is a Friday, 09:00 UTC (10:00 in Lisbon, 10:00 in London).
var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

var (
	codexOffer = v1alpha1.HarnessOffer{
		Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"},
		ReasoningEffort: []string{"low", "medium", "high"},
	}
	claudeOffer = v1alpha1.HarnessOffer{
		Name: "claude-code", Auth: "apiKey", Models: []string{"claude-sonnet-5-5", "claude-opus-5-5"},
		ReasoningEffort: []string{"low", "medium", "high", "max"},
	}
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// swapCatalog is a CatalogProvider whose catalog a test can swap, as catalog-sync does.
type swapCatalog struct {
	p atomic.Pointer[catalog.Catalog]
}

func newSwapCatalog(c *catalog.Catalog) *swapCatalog {
	s := &swapCatalog{}
	s.p.Store(c)
	return s
}

func (s *swapCatalog) Current() *catalog.Catalog { return s.p.Load() }

// logRecorder is a slog.Handler that keeps every record (all levels), for the tests that
// check what is logged.
type logRecorder struct {
	mu      *sync.Mutex
	records *[]slog.Record
	attrs   []slog.Attr
}

func newLogRecorder() *logRecorder {
	return &logRecorder{mu: &sync.Mutex{}, records: &[]slog.Record{}}
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(h.attrs...)
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r)
	return nil
}

func (h *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(slices.Clone(h.attrs), attrs...)
	return &c
}

func (h *logRecorder) WithGroup(string) slog.Handler { return h }

// find returns the records at level with message msg.
func (h *logRecorder) find(level slog.Level, msg string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range *h.records {
		if r.Level == level && r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// attr returns the value of r's attribute key ("" when it has none).
func attr(r slog.Record, key string) string {
	v := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

// env is a scheduler in a test: the memory store, the PoC catalog, the fake GitHub, the real
// run controller (not running: its RunAPI methods are synchronous) and a fake clock.
type env struct {
	t    *testing.T
	ctx  context.Context
	clk  *clock.Fake
	st   store.Store
	cat  *catalog.Catalog
	cats *swapCatalog
	logs *logRecorder
	gh   *fake.Conn
	runs *controllers.RunController
	s    *Scheduler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	cat, errs := catalog.LoadDir(os.DirFS(pocCatalog), catalog.Options{})
	if cat == nil || catalog.HasErrors(errs) {
		t.Fatalf("load PoC catalog: %v", errs)
	}
	gh := fake.NewConn(*cat.Providers[ghProvider], fake.Options{Clock: clk})
	gh.SetOrgRole("compliance-framework", "admin", "gus", "ana", "bob")
	st := memory.New(store.Options{Clock: clk, Logger: discardLogger()})
	return newEnvOn(t, clk, st, cat, gh, Options{})
}

// newEnvOn is a scheduler on st, with the catalog cat and the fake GitHub gh (another
// instance's env shares them), after opts.
func newEnvOn(t *testing.T, clk *clock.Fake, st store.Store, cat *catalog.Catalog, gh *fake.Conn, opts Options) *env {
	t.Helper()
	cp := newSwapCatalog(cat)
	logs := newLogRecorder()
	runs, err := controllers.NewRunController(st, cp, controllers.NewFactoryProvider(st, clk), controllers.RunOptions{Clock: clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	opts.Clock, opts.Logger = clk, slog.New(logs)
	s, err := New(Deps{
		Store: st, Catalog: cp, Conns: controllers.StaticConns{ghProvider: gh}, Runs: runs,
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: context.Background(), clk: clk, st: st, cat: cat, cats: cp, logs: logs, gh: gh, runs: runs, s: s}
}

// run starts the scheduler (lease controller and tick) until the test ends.
func (e *env) run() {
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.s.Run(ctx)
	}()
	e.t.Cleanup(func() {
		cancel()
		recv(e.t, done, "scheduler to stop")
	})
}

func lendQueues(queues ...string) *v1alpha1.Lending {
	return &v1alpha1.Lending{Queues: queues, MaxConcurrent: 1}
}

// register registers login's agent on host with offers (default codex and claude-code) and
// lending, and returns its id and token.
func (e *env) register(login, host string, lending *v1alpha1.Lending, offers ...v1alpha1.HarnessOffer) (string, string) {
	e.t.Helper()
	return e.registerWith(login, host, func(r *RegisterRequest) {
		r.Lending = lending
		if len(offers) > 0 {
			r.Capabilities.Harnesses = offers
		}
	})
}

// enroll mints an enrollment for login's agent on host and returns its token.
func (e *env) enroll(login, host string) string {
	e.t.Helper()
	_, tok, err := e.s.CreateEnrollment(e.ctx, EnrollRequest{Pool: pocPool, Owner: login, Name: host, OwnerProvider: ghProvider, CreatedBy: "factory:cli"})
	if err != nil {
		e.t.Fatalf("enroll %s: %v", login, err)
	}
	return tok
}

// registerWith registers login's agent on host, with a new enrollment, after mut changes the
// request.
func (e *env) registerWith(login, host string, mut func(*RegisterRequest)) (string, string) {
	e.t.Helper()
	req := RegisterRequest{
		EnrollmentToken: e.enroll(login, host), Pool: pocPool, Host: host, AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{
			Harnesses: []v1alpha1.HarnessOffer{codexOffer, claudeOffer},
			Labels:    []string{"macos"},
		},
	}
	if mut != nil {
		mut(&req)
	}
	reg, err := e.s.Register(e.ctx, req)
	if err != nil {
		e.t.Fatalf("register %s: %v", login, err)
	}
	return reg.AgentID, reg.AgentToken
}

// authored makes agent, leased on harness, the work item's last author (implement) run; a
// reported harness ("" for none) is the one its Result names.
func (e *env) authored(agent, harness, reported string) {
	e.t.Helper()
	e.task("tsk_impl", "implement", func(t *v1alpha1.Task) {
		t.Spec.Harness = &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{Name: harness}}
		t.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskSucceeded, Agent: &agent}
	})
	st := &v1alpha1.RunStatus{StartedAt: t0.Add(-time.Hour), End: v1alpha1.RunCompleted}
	if reported != "" {
		st.Result = &v1alpha1.Result{Task: "tsk_impl", Status: v1alpha1.ResultDone, Harness: &v1alpha1.ResultHarness{Name: reported}}
	}
	if err := e.st.Create(e.ctx, &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: "run_impl-1"},
		Spec:     v1alpha1.RunSpec{Task: "tsk_impl", Attempt: 1, Agent: agent},
		Status:   st,
	}); err != nil {
		e.t.Fatal(err)
	}
}

// task creates a Pending task of spec in the poc queue (RuleSet poc-docs-epic, git credential
// on the docs repo), after mut.
func (e *env) task(id, spec string, mut func(*v1alpha1.Task)) *v1alpha1.Task {
	e.t.Helper()
	t := &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{
			ID: id, WorkItem: workItem,
			Provenance: &v1alpha1.Provenance{Event: "evt", RuleSet: pocRuleSet, Rule: "r", CatalogSHA: "sha"},
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: v1alpha1.TaskSpecRef{Name: spec},
			Queue:    pocQueue,
			Credentials: &v1alpha1.TaskCredentials{Git: &v1alpha1.GitCredentialSpec{
				Provider: ghProvider, Identity: "author", Repo: docsRepo,
				Permissions: map[string]string{"contents": "write"}, TTL: "55m",
			}},
		},
		Status: &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}
	if mut != nil {
		mut(t)
	}
	if err := e.st.Create(e.ctx, t); err != nil {
		e.t.Fatal(err)
	}
	return t
}

// running makes a task already held by agent (Running).
func running(agent string) func(*v1alpha1.Task) {
	return func(t *v1alpha1.Task) {
		t.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskRunning, Agent: &agent, LeaseExpiresAt: t0.Add(time.Hour)}
	}
}

func (e *env) getTask(id string) *v1alpha1.Task {
	e.t.Helper()
	var t v1alpha1.Task
	if err := e.st.Get(e.ctx, taskKey(id), &t); err != nil {
		e.t.Fatal(err)
	}
	return &t
}

func (e *env) getAgent(id string) *v1alpha1.Agent {
	e.t.Helper()
	a, err := e.s.getAgent(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) getLease(id string) *Lease {
	e.t.Helper()
	var l Lease
	if err := e.st.Get(e.ctx, leaseKey(id), &l); err != nil {
		e.t.Fatal(err)
	}
	return &l
}

func (e *env) counter(scope, name string) v1alpha1.BudgetCounterStatus {
	e.t.Helper()
	c, err := readCounter(e.ctx, e.st, CounterName(scope, name, e.clk.Now()))
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) setCounter(scope, name string, c v1alpha1.BudgetCounterStatus) {
	e.t.Helper()
	if _, err := addCounter(e.ctx, e.st, CounterName(scope, name, e.clk.Now()), c); err != nil {
		e.t.Fatal(err)
	}
}

// lease asks for a task without waiting.
func (e *env) lease(agent string) *Grant {
	e.t.Helper()
	g, err := e.s.Lease(e.ctx, agent, 0)
	if err != nil {
		e.t.Fatalf("lease for %s: %v", agent, err)
	}
	return g
}

func (e *env) mustLease(agent, task string) *Grant {
	e.t.Helper()
	g := e.lease(agent)
	if g == nil || g.Task.Metadata.ID != task {
		e.t.Fatalf("lease for %s: got %v, want task %s", agent, grantTask(g), task)
	}
	return g
}

func grantTask(g *Grant) string {
	if g == nil {
		return "<none>"
	}
	return g.Task.Metadata.ID
}

func (e *env) explain(task, agent string) string {
	e.t.Helper()
	r, err := e.s.Explain(e.ctx, task, agent)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) reconcile(task string) controllers.Result {
	e.t.Helper()
	res, err := e.s.Reconcile(e.ctx, task)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// dump is every stored object of the kinds the scheduler touches, as JSON.
func (e *env) dump() string {
	e.t.Helper()
	var b strings.Builder
	for _, kind := range []string{v1alpha1.KindTask, v1alpha1.KindRun, v1alpha1.KindAgent, v1alpha1.KindBudgetCounter, v1alpha1.KindEvent, KindLease, KindAgentToken} {
		var objs []store.Object
		if _, err := e.st.List(e.ctx, kind, store.ListOptions{}, &objs); err != nil {
			e.t.Fatal(err)
		}
		for _, o := range objs {
			j, err := json.Marshal(o)
			if err != nil {
				e.t.Fatal(err)
			}
			b.Write(j)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// recv receives from ch or fails the test after waitTimeout.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// eventually waits for cond without sleeping, failing the test after waitTimeout.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func ptr[T any](v T) *T { return &v }

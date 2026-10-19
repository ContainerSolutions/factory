package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/owner"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/agent/worker"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// Secrets the tests use; none may appear in a log line.
const (
	joinSecret  = "join-secret-0123456789"
	ownerSecret = "tok-gus"
	gitSecret   = "ghs_git-secret-0123456789"
	agentID     = "agt_gus-e2e"
)

// call is one request the fake dispatcher saw.
type call struct {
	Method, Path, Auth, Join, Owner string
	Body                            []byte
}

func (c call) String() string { return c.Method + " " + c.Path }

// fakeDispatcher is a scriptable dispatcher API for worker tests.
type fakeDispatcher struct {
	t  *testing.T
	ts *httptest.Server

	mu       sync.Mutex
	calls    []call
	tokens   int    // agent tokens issued
	token    string // the valid agent token
	leases   chan apiclient.Lease
	failNext map[string][]int // "METHOD /suffix" → statuses to answer first
	// heartbeat answers a heartbeat; default {cancel: false}.
	heartbeat func(n int) (int, any)
	beats     int
	changed   chan struct{}
}

func newFakeDispatcher(t *testing.T) *fakeDispatcher {
	t.Helper()
	f := &fakeDispatcher{t: t, leases: make(chan apiclient.Lease, 8), failNext: map[string][]int{}, changed: make(chan struct{})}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeDispatcher) URL() string { return f.ts.URL }

// offer queues a lease for the next poll.
func (f *fakeDispatcher) offer(l apiclient.Lease) { f.leases <- l }

// fail makes the next requests matching key ("POST /heartbeat") answer the statuses.
func (f *fakeDispatcher) fail(key string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[key] = append(f.failNext[key], statuses...)
}

// revoke invalidates the current agent token.
func (f *fakeDispatcher) revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = "revoked"
}

func (f *fakeDispatcher) setHeartbeat(h func(n int) (int, any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeat = h
}

func (f *fakeDispatcher) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// find returns the calls with method whose path ends with suffix.
func (f *fakeDispatcher) find(method, suffix string) []call {
	var out []call
	for _, c := range f.Calls() {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			out = append(out, c)
		}
	}
	return out
}

// waitFor waits until cond holds on the calls (re-checked on every request).
func (f *fakeDispatcher) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		f.mu.Lock()
		ch := f.changed
		f.mu.Unlock()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			f.t.Fatalf("timed out waiting for %s; calls: %v", what, f.Calls())
		}
	}
}

func (f *fakeDispatcher) record(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeDispatcher) scripted(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.failNext[key]; len(s) > 0 {
		f.failNext[key] = s[1:]
		return s[0], true
	}
	return 0, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "title": http.StatusText(status), "code": code})
}

func (f *fakeDispatcher) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, worker.BasePath)
	c := call{Method: r.Method, Path: path, Auth: r.Header.Get("Authorization"),
		Join: r.Header.Get(worker.HeaderJoinToken), Owner: r.Header.Get(worker.HeaderOwnerToken), Body: body}
	f.record(c) // at arrival: a parked poll counts while it waits

	seg := strings.Split(strings.Trim(path, "/"), "/")
	key := r.Method + " /" + seg[len(seg)-1]
	if status, ok := f.scripted(key); ok {
		problem(w, status, "scripted")
		return
	}
	if r.Method == http.MethodPost && path == "/agents" {
		if c.Join != joinSecret || c.Owner != ownerSecret {
			problem(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var req apiclient.RegisterAgentRequest
		if err := json.Unmarshal(body, &req); err != nil {
			problem(w, http.StatusBadRequest, "invalid")
			return
		}
		f.mu.Lock()
		f.tokens++
		f.token = fmt.Sprintf("%s.agent-secret-%d", agentID, f.tokens)
		tok := f.token
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, apiclient.AgentRegistration{AgentId: agentID, AgentToken: tok, ExpiresAt: time.Now().Add(24 * time.Hour)})
		return
	}
	f.mu.Lock()
	valid := f.token != "" && c.Auth == "Bearer "+f.token
	f.mu.Unlock()
	if !valid {
		problem(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/agents/"+agentID+"/lease":
		select {
		case l := <-f.leases:
			writeJSON(w, http.StatusOK, l)
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodPatch && path == "/agents/"+agentID:
		writeJSON(w, http.StatusOK, v1alpha1.Agent{Metadata: v1alpha1.ObjectMeta{Name: agentID}})
	case r.Method == http.MethodDelete && path == "/agents/"+agentID:
		f.mu.Lock()
		f.token = ""
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/heartbeat"):
		f.mu.Lock()
		f.beats++
		n, h := f.beats, f.heartbeat
		f.mu.Unlock()
		if h == nil {
			writeJSON(w, http.StatusOK, apiclient.HeartbeatResponse{ExpiresAt: time.Now().Add(2 * time.Minute)})
			return
		}
		status, v := h(n)
		writeJSON(w, status, v)
	case r.Method == http.MethodPost && (strings.HasSuffix(path, "/complete") || strings.HasSuffix(path, "/abandon")):
		w.WriteHeader(http.StatusNoContent)
	default:
		problem(w, http.StatusNotFound, "not-found")
	}
}

// testLease is a lease of task tsk_1 with the git secret.
func testLease(id string, limits *v1alpha1.Limits) apiclient.Lease {
	return apiclient.Lease{
		Lease: apiclient.LeaseInfo{Id: id, Agent: agentID, Task: "tsk_" + id, ExpiresAt: time.Now().Add(2 * time.Minute), HeartbeatEvery: "30s"},
		Task: v1alpha1.Task{
			Metadata: v1alpha1.TaskMeta{ID: "tsk_" + id, WorkItem: "wi_1"},
			Spec:     v1alpha1.TaskRunSpec{TaskSpec: v1alpha1.TaskSpecRef{Name: "triage"}, Queue: "poc", Limits: limits},
		},
		GitCredential: apiclient.GitCredential{
			Provider: "github-public", Host: "github.com", Username: "x-access-token", Token: gitSecret,
			Repo: "compliance-framework/docs", ExpiresAt: time.Now().Add(time.Hour),
		},
	}
}

// logBuffer is a concurrency-safe log sink.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// harness is a worker under test against a fake dispatcher.
type harness struct {
	f      *fakeDispatcher
	w      *worker.Worker
	clk    clock.Clock
	logs   *logBuffer
	owners *int
	done   chan error
	cancel context.CancelFunc
}

type harnessOpt func(*worker.Options)

func withClock(c clock.Clock) harnessOpt { return func(o *worker.Options) { o.Clock = c } }

func withLending(l v1alpha1.Lending) harnessOpt { return func(o *worker.Options) { o.Lending = l } }

func startWorker(t *testing.T, f *fakeDispatcher, r runner.Runner, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{f: f, logs: &logBuffer{}, owners: new(int), done: make(chan error, 1)}
	var mu sync.Mutex
	o := worker.Options{
		Dispatcher: f.URL(), Pool: "local", Host: "e2e", AgentVersion: "test",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex", Models: []string{"gpt-5-codex"}}}},
		Lending:      v1alpha1.Lending{Queues: []string{"poc"}},
		JoinToken:    func() (string, error) { return joinSecret, nil },
		Owner: owner.TokenFunc(func(context.Context) (string, error) {
			mu.Lock()
			*h.owners++
			mu.Unlock()
			return ownerSecret, nil
		}),
		Runner: r,
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, opt := range opts {
		opt(&o)
	}
	w, err := worker.New(o)
	if err != nil {
		t.Fatal(err)
	}
	h.w = w
	h.clk = o.Clock
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Error("the worker didn't stop")
		}
	})
	return h
}

// stop drains and waits for Run.
func (h *harness) stop(t *testing.T) error {
	t.Helper()
	h.w.Drain()
	select {
	case err := <-h.done:
		h.done <- err // for Cleanup
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't return after Drain")
		return nil
	}
}

func (h *harness) assertNoSecretsInLogs(t *testing.T, extra ...string) {
	t.Helper()
	logs := h.logs.String()
	for _, s := range append([]string{joinSecret, ownerSecret, gitSecret, "agent-secret-"}, extra...) {
		if strings.Contains(logs, s) {
			t.Errorf("a secret is in the logs: %q\n%s", s, logs)
		}
	}
}

package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/store"
)

// TestStoreReadOnlyProblem: a write the store refuses because it stopped writing answers 503
// store-read-only, with no Retry-After (nothing knows when a newer release replaces it).
func TestStoreReadOnlyProblem(t *testing.T) {
	err := fmt.Errorf("create task: %w", store.Errorf("create", store.Key{Kind: "Task", Name: "t"},
		fmt.Errorf("%w: schema 2", store.ErrReadOnly), ""))
	p := toProblem(err)
	if p.status != http.StatusServiceUnavailable || p.code != CodeStoreReadOnly || p.retryAfter != 0 {
		t.Fatalf("toProblem = %d %s retry %s, want 503 %s", p.status, p.code, p.retryAfter, CodeStoreReadOnly)
	}
}

func get(t *testing.T, h http.Handler, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String(), rec.Header()
}

// staticDrops is a DropCounter with fixed counts.
type staticDrops []controllers.DropCount

func (d staticDrops) Drops() []controllers.DropCount { return d }

// readinessStore is a store that reports its readiness, as the object store does.
type readinessStore struct {
	store.Store
	err error
}

func (r *readinessStore) Ready() error { return r.err }

func TestOps(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Intake = staticDrops{
			{Provider: "github-public", Type: "issue.commented", Reason: controllers.DropNoRule, Count: 3},
			{Provider: "github-public", Type: "pr.opened", Reason: controllers.DropOutOfScope, Count: 1},
		}
	})
	ops := e.srv.OpsHandler()

	if code, body, _ := get(t, ops, "/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz %d %q", code, body)
	}
	readyz := func(what string, want int, code string) {
		t.Helper()
		got, body, h := get(t, ops, "/readyz")
		if got != want {
			t.Errorf("readyz %s: %d %s, want %d", what, got, body, want)
		}
		if code == "" {
			return
		}
		if !strings.HasPrefix(h.Get("Content-Type"), ProblemContentType) || !strings.Contains(body, `"code":"`+code+`"`) {
			t.Errorf("readyz %s: %s %q, want the problem %s", what, h.Get("Content-Type"), body, code)
		}
		_, metrics, _ := get(t, ops, "/metrics")
		if !strings.Contains(metrics, "factory_ready 0") {
			t.Errorf("factory_ready isn't 0 %s", what)
		}
	}
	// Not ready: the catalog isn't loaded.
	e.status.set(func(s *controllers.CatalogStatus) { s.Ready = false })
	readyz("before the catalog", http.StatusServiceUnavailable, CodeCatalogNotLoaded)
	// The memory store has nothing to sync: ready once the catalog is.
	e.status.set(func(s *controllers.CatalogStatus) { s.Ready = true })
	readyz("with the catalog and the memory store", http.StatusOK, "")
	// A store that isn't synced, or has stopped writing, isn't ready; the same on any
	// instance, leader or not.
	rs := &readinessStore{Store: e.st, err: errors.New("objectstore: not synced with the bucket yet")}
	e.srv.d.Store = rs
	readyz("before the store's first sync", http.StatusServiceUnavailable, CodeUnavailable)
	rs.err = fmt.Errorf("%w: log/00000000000000000007.json was written with schema 2", store.ErrReadOnly)
	readyz("once the store stopped writing", http.StatusServiceUnavailable, CodeStoreReadOnly)
	rs.err = nil
	if code, body, _ := get(t, ops, "/readyz"); code != http.StatusOK || body != "ready\n" {
		t.Errorf("readyz: %d %q", code, body)
	}
	e.srv.d.Store = e.st

	e.task("tsk_1", nil)
	if r, _ := e.c.ListTasksWithResponse(e.ctx, &apiclient.ListTasksParams{}, bearer(cliToken)); r.StatusCode() != http.StatusOK {
		t.Fatal(r.Status())
	}
	code, body, h := get(t, ops, "/metrics")
	if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics %d %s", code, h.Get("Content-Type"))
	}
	for _, want := range []string{
		"# TYPE factory_tasks gauge", `factory_tasks{phase="Pending"} 1`, `factory_tasks{phase="Running"} 0`,
		`factory_ready 1`, `factory_sse_subscribers 0`, `factory_http_requests_total{code="200"}`,
		"# TYPE factory_events_dropped_total counter",
		`factory_events_dropped_total{provider="github-public",type="issue.commented",reason="no-rule"} 3`,
		`factory_events_dropped_total{provider="github-public",type="pr.opened",reason="out-of-scope"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics miss %q:\n%s", want, body)
		}
	}

	// The ops routes are not on the public handler, nor the API on the ops handler.
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		res, b := e.do(http.MethodGet, p, nil)
		wantProblemReply(t, "public "+p, res, b, http.StatusNotFound, CodeNotFound)
	}
	if code, _, _ := get(t, ops, BasePath+"/auth/me"); code != http.StatusNotFound {
		t.Errorf("the API on the ops handler: %d", code)
	}
}

func TestBodyLimits(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.BodyLimit, o.TranscriptLimit = 512, 4096 })
	agent, tok := e.register("gus", "mac1")
	e.task("tsk_1", nil)
	l := e.lease(agent, tok)

	big := []byte(`{"type":"resync-catalog","pad":"` + strings.Repeat("x", 600) + `"}`)
	res, b := e.do(http.MethodPost, BasePath+"/commands", big, withBearer(cliToken))
	wantProblemReply(t, "a body over the limit", res, b, http.StatusRequestEntityTooLarge, CodeTooLarge)
	// Without a Content-Length (chunked), the limit applies while reading.
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.ts.URL+BasePath+"/commands", io.MultiReader(bytes.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cliToken)
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	wantProblem(t, "a chunked body over the limit", resp, body, http.StatusRequestEntityTooLarge, CodeTooLarge)

	// Transcripts have their own, larger limit.
	blob := gzipped(t, strings.Repeat(`{"line":"abcdefghij"}`+"\n", 30)) // compresses well: under 4 KiB…
	pad := append(append([]byte{}, blob...), make([]byte, 1000)...)      // …padded past 512 bytes
	up, err := e.c.UploadTranscriptWithBodyWithResponse(e.ctx, *l.Lease.Run, "application/gzip", bytes.NewReader(pad), bearer(tok))
	if err != nil || up.JSON200 == nil {
		t.Fatalf("a transcript over the body limit but under its own: %v %s %s", err, up.Status(), up.Body)
	}
	up, err = e.c.UploadTranscriptWithBodyWithResponse(e.ctx, *l.Lease.Run, "application/gzip", bytes.NewReader(make([]byte, 5000)), bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a transcript over its limit", up.HTTPResponse, up.Body, http.StatusRequestEntityTooLarge, CodeTooLarge)
	up, err = e.c.UploadTranscriptWithBodyWithResponse(e.ctx, *l.Lease.Run, "application/gzip", strings.NewReader("not gzip"), bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a transcript that isn't gzip", up.HTTPResponse, up.Body, http.StatusBadRequest, CodeInvalid)
}

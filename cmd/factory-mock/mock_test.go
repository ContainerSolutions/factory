package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/api"
	"github.com/ContainerSolutions/factory/internal/apiclient"
)

func newTestMock(t *testing.T) (*mock, []api.Operation) {
	t.Helper()
	doc, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := api.Operations(doc)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newMock(ops, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return m, ops
}

// concrete fills a path template's parameters.
func concrete(template string) string {
	segments := strings.Split(template, "/")
	for i, s := range segments {
		if isParam(s) {
			segments[i] = "x1"
		}
	}
	return strings.Join(segments, "/")
}

func TestEveryOperationAnswersWithItsExample(t *testing.T) {
	m, ops := newTestMock(t)
	for _, op := range ops {
		if op.ContentType == api.MediaTypeSSE {
			continue // TestStream
		}
		t.Run(op.ID, func(t *testing.T) {
			req := httptest.NewRequest(op.Method, concrete(op.Path), nil)
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, req)
			if rec.Code != op.Status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, op.Status, rec.Body)
			}
			if !op.HasBody() {
				if rec.Body.Len() != 0 {
					t.Errorf("body = %q, want none", rec.Body)
				}
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != op.ContentType {
				t.Errorf("Content-Type = %q, want %q", ct, op.ContentType)
			}
			want, err := op.ExampleBody()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Errorf("body = %s, want the example %s", rec.Body, want)
			}
		})
	}
}

func TestStream(t *testing.T) {
	m, ops := newTestMock(t)
	var op api.Operation
	for _, o := range ops {
		if o.ID == "streamEvents" {
			op = o
		}
	}
	frames, err := op.ExampleFrames()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/stream?topics=workitems,agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != api.MediaTypeSSE {
		t.Fatalf("status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	r := bufio.NewReader(resp.Body)
	for i, f := range frames {
		want, err := f.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got := readFrame(t, r)
		if got != string(want) {
			t.Errorf("frame %d = %q, want %q", i, got, want)
		}
	}
	// The stream stays open after the examples: a read blocks until the client goes away.
	done := make(chan error, 1)
	go func() {
		_, err := r.ReadByte()
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("stream ended after the examples: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("read succeeded after the client went away")
	}
}

// readFrame reads one SSE frame, up to and including its blank line.
func readFrame(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read frame: %v (so far %q)", err, b.String())
		}
		b.WriteString(line)
		if line == "\n" {
			return b.String()
		}
	}
}

func TestErrors(t *testing.T) {
	m, _ := newTestMock(t)
	tests := []struct {
		name, method, path string
		status             int
		allow              string
	}{
		{"unknown path", http.MethodGet, "/api/v1/nope", http.StatusNotFound, ""},
		{"outside the base path", http.MethodGet, "/tasks/x1", http.StatusNotFound, ""},
		{"trailing slash", http.MethodGet, "/api/v1/tasks/", http.StatusNotFound, ""},
		{"empty parameter", http.MethodGet, "/api/v1/tasks//prompt", http.StatusNotFound, ""},
		{"too deep", http.MethodGet, "/api/v1/tasks/x1/prompt/more", http.StatusNotFound, ""},
		{"wrong method", http.MethodDelete, "/api/v1/tasks/x1", http.StatusMethodNotAllowed, "GET"},
		{"wrong method, two allowed", http.MethodPut, "/api/v1/agents", http.StatusMethodNotAllowed, "GET, POST"},
		{"webhook read", http.MethodGet, "/hooks/github-public", http.StatusMethodNotAllowed, "POST"},
		{"dry-run is not a catalog object", http.MethodGet, "/api/v1/catalog/dry-run", http.StatusMethodNotAllowed, "POST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var p problemBody
			dec := json.NewDecoder(rec.Body)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Status != tt.status || p.Code == "" || p.Title == "" {
				t.Errorf("problem = %+v", p)
			}
		})
	}
}

// TestGeneratedClient checks that the generated client decodes the mock's answers into the
// v1alpha1 types: the examples, the spec and the generated code agree.
func TestGeneratedClient(t *testing.T) {
	m, _ := newTestMock(t)
	srv := httptest.NewServer(m)
	defer srv.Close()
	c, err := apiclient.NewClientWithResponses(srv.URL+"/api/v1", apiclient.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	reg, err := c.RegisterAgentWithResponse(ctx, apiclient.RegisterAgentRequest{Pool: "maintainer-agents", Host: "mbp-01", AgentVersion: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if reg.JSON201 == nil || reg.JSON201.AgentId == "" {
		t.Errorf("registerAgent: %d %s", reg.StatusCode(), reg.Body)
	}

	wait := "30s"
	lease, err := c.LeaseTaskWithResponse(ctx, "agt_x", &apiclient.LeaseTaskParams{Wait: &wait})
	if err != nil {
		t.Fatal(err)
	}
	if lease.JSON200 == nil || lease.JSON200.Task.Metadata.ID == "" || lease.JSON200.Task.Spec.Harness.Resolved == nil {
		t.Errorf("leaseTask: %d %s", lease.StatusCode(), lease.Body)
	}

	task, err := c.GetTaskWithResponse(ctx, "tsk_x")
	if err != nil {
		t.Fatal(err)
	}
	if task.JSON200 == nil || task.JSON200.Status == nil || task.JSON200.Status.Phase == "" {
		t.Errorf("getTask: %d %s", task.StatusCode(), task.Body)
	}

	obj, err := c.GetCatalogObjectWithResponse(ctx, apiclient.CatalogKindTaskSpec, "triage")
	if err != nil {
		t.Fatal(err)
	}
	if obj.JSON200 == nil {
		t.Fatalf("getCatalogObject: %d %s", obj.StatusCode(), obj.Body)
	}
	spec, err := obj.JSON200.AsTaskSpec()
	if err != nil || spec.Metadata.Name != "triage" {
		t.Errorf("getCatalogObject as TaskSpec: %v, %+v", err, spec.Metadata)
	}

	transcript, err := c.GetTranscriptWithResponse(ctx, "run_x")
	if err != nil {
		t.Fatal(err)
	}
	if ct := transcript.HTTPResponse.Header.Get("Content-Type"); transcript.StatusCode() != http.StatusOK || ct != api.MediaTypeGzip {
		t.Fatalf("getTranscript: %d %q", transcript.StatusCode(), ct)
	}
	zr, err := gzip.NewReader(bytes.NewReader(transcript.Body))
	if err != nil {
		t.Fatalf("getTranscript body is not gzip: %v", err)
	}
	if lines, err := io.ReadAll(zr); err != nil || !bytes.HasPrefix(lines, []byte("{")) {
		t.Errorf("getTranscript: %v, %q", err, lines)
	}

	missing, err := c.GetTaskWithResponse(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.ApplicationproblemJSON404 == nil {
		t.Errorf("getTask with an empty id: %d %s", missing.StatusCode(), missing.Body)
	}
}

func TestRunFlags(t *testing.T) {
	if err := run([]string{"--no-such-flag"}); err == nil {
		t.Error("an unknown flag must fail")
	}
	if err := run([]string{"--version"}); err != nil {
		t.Errorf("--version: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/owner"
	"github.com/compliance-framework/factory/internal/agent/probe"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

const (
	joinSecret  = "join-secret-cmd-0123456789"
	ownerSecret = "tok-gus"
)

func TestRunVersionAndUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"version"}} {
		var out bytes.Buffer
		if code := run(context.Background(), args, &out, io.Discard); code != 0 || out.String() != "factory-agent dev\n" {
			t.Errorf("run(%v) = %d %q", args, code, out.String())
		}
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &out, io.Discard); code != 0 ||
		!strings.Contains(out.String(), "worker") || !strings.Contains(out.String(), "probe") {
		t.Errorf("help = %d %q", code, out.String())
	}
	var errOut bytes.Buffer
	if code := run(context.Background(), []string{"nope"}, io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), `unknown command "nope"`) {
		t.Errorf("unknown = %d %q", code, errOut.String())
	}
}

// writeConfig writes an agent config (0600) and its join token (0600) under dir.
func writeConfig(t *testing.T, dir, dispatcher string, extra string) string {
	t.Helper()
	tok := filepath.Join(dir, "join-token")
	if err := os.WriteFile(tok, []byte(joinSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "agent.yaml")
	cfg := "dispatcher: " + dispatcher + "\npool: local\njoinTokenFile: " + tok + "\nhost: cmd-test\n" +
		"labels: [has-local-stack, gif-tooling]\nlocalDev: ~/local-dev\n" +
		"harnesses:\n  codex:\n    home: ~/codex\n    models: [gpt-5-codex]\n    efforts: [low, high]\n    default: gpt-5-codex\n" +
		"lending:\n  queues: [poc]\n" + extra
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type fakeTooling struct{ missing string }

func (f fakeTooling) Check(context.Context) []probe.Tool {
	var out []probe.Tool
	for _, n := range append(probe.BaseTools(), probe.GhExtensions()...) {
		t := probe.Tool{Name: n, OK: n != f.missing}
		if !t.OK {
			t.Problem = "not found on PATH"
		}
		out = append(out, t)
	}
	return out
}

func probeOpts(missing string) probe.Options {
	return probe.Options{
		Tooling: fakeTooling{missing: missing},
		Labels: map[string]probe.LabelChecker{
			"gif-tooling": probe.LabelFunc(func(context.Context, *config.Config) error { return errors.New("vhs not found") }),
		},
	}
}

func homeIn(dir string) config.Options {
	return config.Options{Home: func() (string, error) { return dir, nil }}
}

func TestProbeCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "https://factory.example.dev", "")

	var out, errOut bytes.Buffer
	if code := runProbe(context.Background(), []string{"--config", p}, &out, &errOut, probeOpts(""), homeIn(dir)); code != 0 {
		t.Fatalf("probe = %d\n%s", code, errOut.String())
	}
	for _, want := range []string{
		"codex: auth subscription, models gpt-5-codex (default gpt-5-codex), efforts low, high",
		"labels: has-local-stack\n", "label gif-tooling: vhs not found", "gh-stack: ok",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("probe output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := runProbe(context.Background(), []string{"--config", p, "--json"}, &out, io.Discard, probeOpts(""), homeIn(dir)); code != 0 {
		t.Fatalf("probe --json = %d", code)
	}
	var rep probe.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("probe --json: %v\n%s", err, out.String())
	}
	if len(rep.Capabilities.Harnesses) != 1 || rep.Capabilities.Harnesses[0].Name != "codex" || len(rep.Dropped) != 1 {
		t.Errorf("report %+v", rep)
	}

	errOut.Reset()
	if code := runProbe(context.Background(), []string{"--config", p}, io.Discard, &errOut, probeOpts("jq"), homeIn(dir)); code != 1 ||
		!strings.Contains(errOut.String(), "base tool jq") {
		t.Errorf("probe without jq = %d %q", code, errOut.String())
	}
	if code := runProbe(context.Background(), []string{"--config", filepath.Join(dir, "missing.yaml")}, io.Discard, io.Discard, probeOpts(""), homeIn(dir)); code != 1 {
		t.Errorf("probe with no config = %d", code)
	}
	if code := runProbe(context.Background(), []string{"extra"}, io.Discard, io.Discard, probeOpts(""), homeIn(dir)); code != 2 {
		t.Errorf("probe extra = %d", code)
	}
}

// cmdDispatcher is a minimal dispatcher for the worker command: registration, an empty long
// poll, PATCH and DELETE.
type cmdDispatcher struct {
	mu      sync.Mutex
	patches []apiclient.AgentUpdate
	deletes int
	regs    int
}

func (c *cmdDispatcher) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents":
		if r.Header.Get("X-Factory-Join-Token") != joinSecret || r.Header.Get("X-Factory-Owner-Token") != ownerSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		c.regs++
		c.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(apiclient.AgentRegistration{AgentId: "agt_gus-cmd-test", AgentToken: "agt_gus-cmd-test.secret", ExpiresAt: time.Now().Add(time.Hour)})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/lease"):
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch:
		var u apiclient.AgentUpdate
		_ = json.Unmarshal(body, &u)
		c.mu.Lock()
		c.patches = append(c.patches, u)
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(v1alpha1.Agent{})
	case r.Method == http.MethodDelete:
		c.mu.Lock()
		c.deletes++
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (c *cmdDispatcher) state() (regs int, patches []apiclient.AgentUpdate, deletes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.regs, append([]apiclient.AgentUpdate(nil), c.patches...), c.deletes
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a concurrency-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The worker command registers, reloads lending on SIGHUP, drains on SIGTERM and exits 0.
func TestWorkerCommandSignals(t *testing.T) {
	disp := &cmdDispatcher{}
	ts := httptest.NewServer(http.HandlerFunc(disp.serve))
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	p := writeConfig(t, dir, ts.URL, "  maxConcurrent: 1\n")
	result := filepath.Join(dir, "result.json")
	if err := os.WriteFile(result, []byte(`{"task": "", "status": "done", "outcome": "triaged", "question": null}`), 0o600); err != nil {
		t.Fatal(err)
	}

	sigs := make(chan os.Signal, 4)
	errOut := &syncBuffer{}
	code := make(chan int, 1)
	go func() {
		code <- runWorker(context.Background(), []string{"--config", p, "--stub-result", result, "--log-level", "debug"}, errOut, workerDeps{
			Probe: probeOpts(""), Config: homeIn(dir), Signals: sigs,
			Owner: owner.TokenFunc(func(context.Context) (string, error) { return ownerSecret, nil }),
		})
	}()
	waitUntil(t, "registration", func() bool { r, _, _ := disp.state(); return r == 1 })

	// SIGHUP: the edited lending is PATCHed.
	if err := os.WriteFile(p, bytes.Replace(mustRead(t, p), []byte("queues: [poc]"), []byte("queues: [poc, validation]"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGHUP
	waitUntil(t, "the lending PATCH", func() bool { _, ps, _ := disp.state(); return len(ps) == 1 })
	if _, ps, _ := disp.state(); ps[0].Lending == nil || len(ps[0].Lending.Queues) != 2 {
		t.Errorf("lending PATCH %+v", ps[0])
	}

	sigs <- syscall.SIGTERM
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("worker exited %d\n%s", c, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the worker didn't drain\n%s", errOut.String())
	}
	_, ps, deletes := disp.state()
	if len(ps) != 2 || ps[1].Phase == nil || *ps[1].Phase != v1alpha1.AgentDraining || deletes != 1 {
		t.Errorf("after SIGTERM: patches %+v, deletes %d", ps, deletes)
	}
	for _, s := range []string{joinSecret, ownerSecret, "agt_gus-cmd-test.secret"} {
		if strings.Contains(errOut.String(), s) {
			t.Errorf("a secret is in the logs: %q", s)
		}
	}
	if !strings.Contains(errOut.String(), "development mode") {
		t.Errorf("no development-mode warning:\n%s", errOut.String())
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWorkerCommandRefusals(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "http://127.0.0.1:1", "")
	deps := workerDeps{Probe: probeOpts(""), Config: homeIn(dir)}
	// claude-code has no adapter until AGT-S1-T04.
	withClaude := filepath.Join(dir, "with-claude.yaml")
	cfg := strings.Replace(string(mustRead(t, p)), "harnesses:\n", "harnesses:\n  claude-code:\n    configDir: ~/claude\n    models: [claude-sonnet-5-5]\n", 1)
	if err := os.WriteFile(withClaude, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		deps workerDeps
		code int
		want string
	}{
		{"no harness adapter yet", []string{"--config", withClaude}, deps, 1, "no harness adapter for claude-code"},
		{"bad stub result", []string{"--config", p, "--stub-result", p}, deps, 1, "--stub-result"},
		{"missing base tooling", []string{"--config", p}, workerDeps{Probe: probeOpts("gh-stack"), Config: homeIn(dir)}, 1, "base tool gh-stack"},
		{"bad config", []string{"--config", filepath.Join(dir, "nope.yaml")}, deps, 1, "config"},
		{"bad log level", []string{"--log-level", "loud"}, deps, 2, "--log-level"},
		{"extra args", []string{"now"}, deps, 2, "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errOut bytes.Buffer
			if code := runWorker(context.Background(), tt.args, &errOut, tt.deps); code != tt.code || !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("runWorker = %d, want %d with %q:\n%s", code, tt.code, tt.want, errOut.String())
			}
		})
	}

	// A join token readable by others is refused at registration.
	if err := os.Chmod(filepath.Join(dir, "join-token"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(dir, "result.json")
	if err := os.WriteFile(result, []byte(`{"task": "", "status": "noop", "question": null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	if code := runWorker(context.Background(), []string{"--config", p, "--stub-result", result}, &errOut, workerDeps{
		Probe: probeOpts(""), Config: homeIn(dir),
		Owner: owner.TokenFunc(func(context.Context) (string, error) { return ownerSecret, nil }),
	}); code != 1 || !strings.Contains(errOut.String(), "chmod 600") || strings.Contains(errOut.String(), joinSecret) {
		t.Errorf("runWorker with a 0644 join token = %d:\n%s", code, errOut.String())
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/server"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

const (
	ghProvider    = "github-public"
	docsIssue     = "compliance-framework/docs#42"
	testCLIToken  = "cli-token-0123456789abcdef0123456789abcdef"
	testJoinToken = "join-token-for-the-local-pool"
	serveWait     = 15 * time.Second
)

// testEnv is the environment of a `factory serve` under test: dummy secrets and the local
// Pool's join token.
func testEnv() map[string]string {
	return map[string]string{
		envOAuthClientID:                       "client-1",
		envOAuthClientSecret:                   "oauth-secret-1",
		envSessionKey:                          "session-key-0123456789abcdef0123456789abcdef",
		envCLIToken:                            testCLIToken,
		"FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN": testJoinToken,
	}
}

func getenvOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// fakeGitHubUsers answers GET /user for "Bearer tok-<login>" (the agent owners' tokens).
func fakeGitHubUsers(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		if r.URL.Path != "/user" || !ok {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"login": login})
	}))
	t.Cleanup(ts.Close)
	return ts
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

// served is a running `factory serve`.
type served struct {
	t       *testing.T
	public  string // http://127.0.0.1:port
	ops     string
	gh      *fake.Conn
	logs    *logBuffer
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

// testConfig is the config of a dev serve on ephemeral loopback ports, with the PoC catalog.
func testConfig(t *testing.T, githubAPI string) serveConfig {
	t.Helper()
	cfg, warnings, err := parseServeConfig([]string{
		"--dev-fake-providers", "--catalog-dir", pocDir,
		"--addr", "127.0.0.1:0", "--ops-addr", "localhost:0", "--public-url", "https://factory.test",
		"--github-api-url", githubAPI, "--skills-base", "", "--shutdown-timeout", "5s", "--log-level", "debug",
	}, getenvOf(testEnv()), io.Discard)
	if err != nil {
		t.Fatalf("config: %v (warnings %v)", err, warnings)
	}
	return cfg
}

// startServe runs runServe in the background and waits until it is ready.
func startServe(t *testing.T, cfg serveConfig) *served {
	t.Helper()
	c, errs := catalog.LoadDir(os.DirFS(pocDir), catalog.Options{})
	if c == nil || catalog.HasErrors(errs) {
		t.Fatalf("load PoC catalog: %v", errs)
	}
	adapter := fake.NewAdapter(fake.Options{Groups: func(name string) (v1alpha1.GroupSpec, bool) {
		g, ok := c.Groups[name]
		if !ok {
			return v1alpha1.GroupSpec{}, false
		}
		return g.Spec, true
	}})
	gh := adapter.Conn(ghProvider)
	gh.SetOrgRole("compliance-framework", "admin", "gus")
	if err := gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: docsIssue}}); err != nil {
		t.Fatal(err)
	}

	s := runServed(t, cfg, serveDeps{Getenv: getenvOf(testEnv()), Fake: adapter})
	s.gh = gh
	return s
}

// runServed runs runServe with deps in the background and waits until it is ready.
func runServed(t *testing.T, cfg serveConfig, deps serveDeps) *served {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, logs: &logBuffer{}, cancel: cancel, done: make(chan error, 1)}
	addrs := make(chan [2]net.Addr, 1)
	ready := make(chan struct{})
	log := slog.New(slog.NewJSONHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	deps.Listening = func(pub, ops net.Addr) { addrs <- [2]net.Addr{pub, ops} }
	deps.Ready = func() { close(ready) }
	go func() { s.done <- runServe(ctx, cfg, deps, log) }()
	t.Cleanup(func() { _ = s.stop() })
	select {
	case a := <-addrs:
		s.public, s.ops = "http://"+a[0].String(), "http://"+a[1].String()
	case err := <-s.done:
		s.stopped = true
		t.Fatalf("serve exited at startup: %v\n%s", err, s.logs)
	case <-time.After(serveWait):
		t.Fatalf("serve didn't listen\n%s", s.logs)
	}
	select {
	case <-ready:
	case <-time.After(serveWait):
		t.Fatalf("serve didn't get ready\n%s", s.logs)
	}
	return s
}

// stop cancels serve (SIGTERM) and returns what runServe returned.
func (s *served) stop() error {
	if s.stopped {
		return nil
	}
	s.stopped = true
	s.cancel()
	select {
	case err := <-s.done:
		return err
	case <-time.After(serveWait):
		s.t.Errorf("serve didn't shut down\n%s", s.logs)
		return nil
	}
}

func (s *served) client(t *testing.T) *apiclient.ClientWithResponses {
	t.Helper()
	c, err := apiclient.NewClientWithResponses(s.public + server.BasePath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func bearer(tok string) apiclient.RequestEditorFn {
	return func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}

func eventuallyT(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(serveWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeEndToEnd is RT-S2-T04's done criterion (adapted): serve starts in-process with the
// fake provider and the PoC catalog, accepts a recorded webhook on /hooks/<provider>, and a
// fake agent registered through the generated client leases, heartbeats and completes the
// task; the task succeeds and its factory.run.completed event drives the next decision.
func TestServeEndToEnd(t *testing.T) {
	ctx := context.Background()
	gh := fakeGitHubUsers(t)
	s := startServe(t, testConfig(t, gh.URL))
	c := s.client(t)
	cli := bearer(testCLIToken)

	// The recorded webhook: a maintainer opens docs#42.
	body, err := os.ReadFile(filepath.Join("testdata", "serve", "issue-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{http.StatusAccepted, http.StatusOK} { // the redelivery is a duplicate
		res, err := http.Post(s.public+"/hooks/"+ghProvider, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("webhook: %s, want %d", res.Status, want)
		}
	}

	// A fake agent registers (owner gus, a maintainer) and long-polls for the plan-epic task.
	reg, err := c.RegisterAgentWithResponse(ctx, apiclient.RegisterAgentRequest{
		Pool: "local", Host: "e2e", AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending: &v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
	}, enrolled(t, ctx, c, "e2e"))
	if err != nil || reg.JSON201 == nil {
		t.Fatalf("register: %v %s %s", err, reg.Status(), reg.Body)
	}
	agent := bearer(reg.JSON201.AgentToken)
	wait := "10s"
	var lease *apiclient.Lease
	eventuallyT(t, "a lease", func() bool {
		res, err := c.LeaseTaskWithResponse(ctx, reg.JSON201.AgentId, &apiclient.LeaseTaskParams{Wait: &wait}, agent)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode() != http.StatusOK && res.StatusCode() != http.StatusNoContent {
			t.Fatalf("lease: %s %s", res.Status(), res.Body)
		}
		lease = res.JSON200
		return lease != nil
	})
	task := lease.Task.Metadata.ID
	if spec := lease.Task.Spec.TaskSpec.Name; spec != "plan-epic" {
		t.Fatalf("leased %s, want plan-epic", spec)
	}
	hb, err := c.HeartbeatLeaseWithResponse(ctx, lease.Lease.Id, apiclient.HeartbeatRequest{
		Progress: []v1alpha1.ProgressEvent{{At: time.Now().UTC(), Kind: "step", Message: "planning"}},
	}, agent)
	if err != nil || hb.JSON200 == nil || hb.JSON200.Cancel {
		t.Fatalf("heartbeat: %v %s %s", err, hb.Status(), hb.Body)
	}
	done, err := c.CompleteLeaseWithResponse(ctx, lease.Lease.Id, apiclient.CompleteRequest{Result: v1alpha1.Result{
		Task: task, Status: v1alpha1.ResultDone, Outcome: "decomposed", Summary: "two sub-issues",
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}},
		Data: map[string]any{"plan": []any{
			map[string]any{"issue": "compliance-framework/docs#43", "title": "Translate the intro"},
			map[string]any{"issue": "compliance-framework/docs#44", "title": "Translate the guides"},
		}},
	}}, agent)
	if err != nil || done.StatusCode() != http.StatusNoContent {
		t.Fatalf("complete: %v %s %s", err, done.Status(), done.Body)
	}

	// The task succeeded, and factory.run.completed dispatched implement for the first
	// sub-issue (build-stack-start), with the plan comment on the epic.
	gt, err := c.GetTaskWithResponse(ctx, task, cli)
	if err != nil || gt.JSON200 == nil || gt.JSON200.Status.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("task after complete: %v %s %s", err, gt.Status(), gt.Body)
	}
	eventuallyT(t, "the implement task from factory.run.completed", func() bool {
		res, err := c.ListTasksWithResponse(ctx, &apiclient.ListTasksParams{}, cli)
		if err != nil || res.JSON200 == nil {
			t.Fatalf("list tasks: %v %s", err, res.Status())
		}
		for _, tk := range res.JSON200.Items {
			if tk.Spec.TaskSpec.Name == "implement" && tk.Metadata.Provenance != nil &&
				tk.Metadata.Provenance.Rule == "build-stack-start" && strings.HasPrefix(tk.Metadata.Provenance.Event, "factory/run/") {
				return true
			}
		}
		return false
	})
	eventuallyT(t, "the epic-plan comment on the epic", func() bool {
		for _, a := range s.gh.Applies() {
			if a.Request.Op == v1alpha1.OpComment && a.Request.Target.Ref == docsIssue {
				return true
			}
		}
		return false
	})

	// factory dispatch injects an event through the same API.
	var out, errOut bytes.Buffer
	env := map[string]string{envCLIToken: testCLIToken}
	if code := runDispatch(ctx, []string{"--event", filepath.Join("testdata", "serve", "dispatch-event.json"), "--server", s.public},
		getenvOf(env), &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "inject/") {
		t.Fatalf("dispatch: %d %q %q", code, out.String(), errOut.String())
	}

	if err := s.stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	for _, secret := range []string{testCLIToken, testJoinToken, "oauth-secret-1", "session-key-0123456789", reg.JSON201.AgentToken} {
		if strings.Contains(s.logs.String(), secret) {
			t.Errorf("a secret is in the logs: %q", secret)
		}
	}
}

// The ops routes are only on the ops listener; the public listener has the API and the UI.
func TestServeOpsListenerOnly(t *testing.T) {
	s := startServe(t, testConfig(t, fakeGitHubUsers(t).URL))
	get := func(url string) (int, string) {
		t.Helper()
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, _ := get(s.ops + p); code != http.StatusOK {
			t.Errorf("ops %s: %d", p, code)
		}
	}
	if code, body := get(s.ops + "/metrics"); code != http.StatusOK || !strings.Contains(body, `factory_controller_panics_total{controller="rule"} 0`) ||
		!strings.Contains(body, "factory_ready 1") {
		t.Errorf("ops /metrics: %d\n%s", code, body)
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if code, body := get(s.public + p); code == http.StatusOK && !strings.Contains(body, "<html") && !strings.Contains(body, "<!doctype") {
			t.Errorf("public %s served the ops route: %d %q", p, code, body)
		}
	}
	if code, _ := get(s.ops + server.BasePath + "/auth/device-config"); code != http.StatusNotFound {
		t.Errorf("ops listener serves the API: %d", code)
	}
	if code, body := get(s.public + "/"); code != http.StatusOK || !strings.Contains(strings.ToLower(body), "<html") {
		t.Errorf("public / (UI): %d %q", code, body)
	}
	if code, _ := get(s.public + server.BasePath + "/auth/device-config"); code != http.StatusOK {
		t.Errorf("public API: %d", code)
	}
}

// Shutdown with an open SSE stream and a parked lease poll: both end, and serve returns well
// within the shutdown timeout.
func TestServeShutdownWithOpenStream(t *testing.T) {
	ctx := context.Background()
	s := startServe(t, testConfig(t, fakeGitHubUsers(t).URL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.public+server.BasePath+"/stream?topics=tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testCLIToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream: %s", res.Status)
	}
	r := bufio.NewReader(res.Body)
	if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, ": connected") {
		t.Fatalf("stream start: %q %v", line, err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, r)
		ended <- err
	}()

	start := time.Now()
	if err := s.stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("shutdown took %s with an open stream", d)
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Errorf("stream ended with %v, want a clean end", err)
		}
	case <-time.After(serveWait):
		t.Fatal("the stream didn't end")
	}
	if !strings.Contains(s.logs.String(), `"msg":"shutdown complete"`) {
		t.Errorf("no shutdown complete log:\n%s", s.logs)
	}
}

// A client connection that never sends a request doesn't hold shutdown up: once the listeners
// stop, it is closed (http.Server.Shutdown would count it as busy for 5 s, past a shorter
// --shutdown-timeout).
func TestServeShutdownWithIdleConnection(t *testing.T) {
	cfg := testConfig(t, fakeGitHubUsers(t).URL)
	cfg.ShutdownTimeout = 2 * time.Second
	s := startServe(t, cfg)
	var conns []net.Conn
	for _, addr := range []string{s.public, s.ops} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(addr, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		conns = append(conns, conn)
	}
	time.Sleep(100 * time.Millisecond) // accepted: both are StateNew
	start := time.Now()
	if err := s.stop(); err != nil {
		t.Fatalf("shutdown with idle connections: %v\n%s", err, s.logs)
	}
	if d := time.Since(start); d >= cfg.ShutdownTimeout {
		t.Errorf("shutdown took %s with idle connections", d)
	}
	if !strings.Contains(s.logs.String(), `"msg":"shutdown complete"`) {
		t.Errorf("no shutdown complete log:\n%s", s.logs)
	}
	for _, conn := range conns {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("the idle connection: read %v, want it closed (EOF)", err)
		}
	}
}

// Without --dev-fake-providers the GitHub adapter connects the catalog's github Provider:
// without its secrets startup fails clearly, naming the variable (never a value).
func TestServeGitHubProviderNeedsSecrets(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	cfg.DevFakeProviders, cfg.StateMemory = false, true // --state-memory, as a run without the fake must say
	err := runServe(context.Background(), cfg, serveDeps{Getenv: getenvOf(testEnv())}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "connect providers") || !strings.Contains(err.Error(), `"github-public"`) ||
		!strings.Contains(err.Error(), "FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET is not set") {
		t.Fatalf("runServe = %v, want the missing webhook secret", err)
	}
}

// An invalid catalog never becomes ready: startup fails after --catalog-timeout, with the
// catalog's diagnostics.
func TestServeInvalidCatalog(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	cfg.CatalogDir = brokenPoC(t)
	// Long enough for the first load attempt to finish under -race and a busy machine: the
	// error must carry that attempt's diagnostics, not "no valid catalog loaded yet".
	cfg.CatalogTimeout = 5 * time.Second
	err := runServe(context.Background(), cfg, serveDeps{Getenv: getenvOf(testEnv())}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "no valid catalog") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("runServe = %v, want the catalog's errors", err)
	}
}

func TestParseServeConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	without := func(keys ...string) map[string]string {
		m := testEnv()
		for _, k := range keys {
			delete(m, k)
		}
		return m
	}
	tests := []struct {
		name string
		args []string
		// bare: the args go as they are; otherwise a case with no store flag gets
		// --state-memory, so it checks what it is about rather than the durable-store refusal.
		bare    bool
		env     map[string]string
		wantErr []string // substrings; nil: valid
		check   func(*testing.T, serveConfig, []string)
	}{
		{name: "defaults", env: testEnv(), check: func(t *testing.T, c serveConfig, w []string) {
			if c.Addr != ":8800" || c.OpsAddr != "127.0.0.1:9090" || c.Retention != 2160*time.Hour || c.LogFormat != "json" ||
				c.CatalogRepo != "https://github.com/ContainerSolutions/factory" || c.CatalogRef != "main" || c.CatalogPath != "catalog" ||
				c.DevFakeProviders || c.PublicURL != "http://localhost:8800" || c.DeployEnvironment != "github-pages" || c.StateDir != "" ||
				c.AntiAffinityWait != scheduler.DefaultAntiAffinityWait || !c.StateMemory || c.StateURL != "" ||
				c.StateSyncInterval != objectstore.DefaultSyncInterval {
				t.Errorf("defaults %+v", c)
			}
			if len(w) != 1 || !strings.Contains(w[0], "no --public-url") {
				t.Errorf("warnings %v", w)
			}
		}},
		{name: "public URL from env", env: func() map[string]string { m := testEnv(); m[envPublicURL] = "https://x.ngrok-free.dev/"; return m }(),
			check: func(t *testing.T, c serveConfig, _ []string) {
				if c.PublicURL != "https://x.ngrok-free.dev" {
					t.Errorf("public URL %q", c.PublicURL)
				}
			}},
		{name: "secrets from env", env: testEnv(), check: func(t *testing.T, c serveConfig, _ []string) {
			if c.CLIToken != testCLIToken || c.OAuthClientID != "client-1" || c.OAuthClientSecret != "oauth-secret-1" || c.SessionKey == "" {
				t.Errorf("secrets not read from the environment")
			}
		}},
		{name: "dev flag warns", args: []string{"--dev-fake-providers"}, bare: true, env: testEnv(), check: func(t *testing.T, c serveConfig, w []string) {
			if !c.DevFakeProviders || !strings.Contains(strings.Join(w, "\n"), "development only") {
				t.Errorf("dev %v %v", c.DevFakeProviders, w)
			}
			if !c.StateMemory {
				t.Error("--dev-fake-providers alone isn't --state-memory")
			}
		}},
		{name: "no store", bare: true, env: testEnv(), wantErr: []string{"serve needs a durable store", "--state-dir", "--state-url", "--state-memory"}},
		{name: "dev flag with a state dir", args: []string{"--dev-fake-providers", "--state-dir", filepath.Join(dir, "state")}, bare: true, env: testEnv(),
			check: func(t *testing.T, c serveConfig, _ []string) {
				if c.StateMemory || c.StateDir == "" {
					t.Errorf("state memory %v, dir %q", c.StateMemory, c.StateDir)
				}
			}},
		{name: "state url", args: []string{"--state-url", "file://" + filepath.Join(dir, "bucket"), "--state-sync-interval", "5s"}, bare: true, env: testEnv(),
			check: func(t *testing.T, c serveConfig, _ []string) {
				if c.StateURL != "file://"+filepath.Join(dir, "bucket") || c.StateSyncInterval != 5*time.Second || c.StateMemory {
					t.Errorf("state url %q, sync %s, memory %v", c.StateURL, c.StateSyncInterval, c.StateMemory)
				}
			}},
		{name: "state dir and url", args: []string{"--state-dir", dir, "--state-url", "gs://b/p"}, bare: true, env: testEnv(),
			wantErr: []string{"--state-dir and --state-url are exclusive"}},
		{name: "state memory and dir", args: []string{"--state-memory", "--state-dir", dir}, bare: true, env: testEnv(),
			wantErr: []string{"--state-memory and --state-dir or --state-url are exclusive"}},
		{name: "state url without a session key", args: []string{"--state-url", "gs://b/p"}, bare: true, env: without(envSessionKey),
			wantErr: []string{"--state-url needs " + envSessionKey}},
		{name: "state url, all secrets missing", args: []string{"--state-url", "gs://b/p"}, bare: true, env: map[string]string{},
			wantErr: []string{"missing environment variables: FACTORY_OAUTH_CLIENT_ID, FACTORY_OAUTH_CLIENT_SECRET, FACTORY_CLI_TOKEN", "--state-url needs " + envSessionKey}},
		{name: "previous session key", env: func() map[string]string { m := testEnv(); m[envSessionKeyPrevious] = strings.Repeat("p", 64); return m }(),
			check: func(t *testing.T, c serveConfig, _ []string) {
				if c.SessionKeyPrevious != strings.Repeat("p", 64) {
					t.Error("FACTORY_SESSION_KEY_PREVIOUS not read")
				}
			}},
		{name: "short previous session key", env: func() map[string]string { m := testEnv(); m[envSessionKeyPrevious] = "short"; return m }(),
			wantErr: []string{envSessionKeyPrevious + " must be at least 32 bytes"}},
		{name: "zero sync interval", args: []string{"--state-sync-interval", "0s"}, env: testEnv(), wantErr: []string{"--state-sync-interval must be positive"}},
		{name: "missing secrets", env: without(envOAuthClientSecret, envCLIToken),
			wantErr: []string{"missing environment variables: FACTORY_OAUTH_CLIENT_SECRET, FACTORY_CLI_TOKEN"}},
		{name: "all secrets missing", env: map[string]string{},
			wantErr: []string{envOAuthClientID, envOAuthClientSecret, envSessionKey, envCLIToken}},
		{name: "short session key", env: func() map[string]string { m := testEnv(); m[envSessionKey] = "short"; return m }(),
			wantErr: []string{envSessionKey + " must be at least 32 bytes"}},
		{name: "short CLI token", env: func() map[string]string { m := testEnv(); m[envCLIToken] = "short"; return m }(),
			wantErr: []string{envCLIToken + " must be at least 32 characters"}},
		{name: "ops on every interface", args: []string{"--ops-addr", ":9090"}, env: testEnv(), wantErr: []string{"must be a loopback address"}},
		{name: "ops on a public IP", args: []string{"--ops-addr", "10.0.0.5:9090"}, env: testEnv(), wantErr: []string{"must be a loopback address"}},
		{name: "ops on localhost", args: []string{"--ops-addr", "localhost:9090"}, env: testEnv()},
		{name: "ops on ::1", args: []string{"--ops-addr", "[::1]:9090"}, env: testEnv()},
		{name: "ops same as public", args: []string{"--addr", "127.0.0.1:9090"}, env: testEnv(), wantErr: []string{"--ops-addr must differ"}},
		{name: "bad addr", args: []string{"--addr", "8800"}, env: testEnv(), wantErr: []string{"--addr"}},
		{name: "dir and repo", args: []string{"--catalog-dir", dir, "--catalog-repo", "https://example.com/r"}, env: testEnv(),
			wantErr: []string{"--catalog-dir and --catalog-repo are exclusive"}},
		{name: "dir and ref", args: []string{"--catalog-dir", dir, "--catalog-ref", "v1"}, env: testEnv(),
			wantErr: []string{"--catalog-dir and --catalog-ref are exclusive"}},
		{name: "dir missing", args: []string{"--catalog-dir", filepath.Join(dir, "nope")}, env: testEnv(), wantErr: []string{"is not a directory"}},
		{name: "dir is a file", args: []string{"--catalog-dir", file}, env: testEnv(), wantErr: []string{"is not a directory"}},
		{name: "empty repo", args: []string{"--catalog-repo", ""}, env: testEnv(), wantErr: []string{"must not be empty"}},
		{name: "ref like a flag", args: []string{"--catalog-ref", "--upload-pack=x"}, env: testEnv(), wantErr: []string{"must not start with '-'"}},
		{name: "relative public URL", args: []string{"--public-url", "factory.test"}, env: testEnv(), wantErr: []string{"--public-url"}},
		{name: "public URL with credentials", args: []string{"--public-url", "https://u:p@factory.test"}, env: testEnv(), wantErr: []string{"--public-url"}},
		{name: "zero retention", args: []string{"--retention", "0s"}, env: testEnv(), wantErr: []string{"--retention must be positive"}},
		{name: "bad log format", args: []string{"--log-format", "xml"}, env: testEnv(), wantErr: []string{"--log-format"}},
		{name: "bad log level", args: []string{"--log-level", "loud"}, env: testEnv(), wantErr: []string{"--log-level"}},
		{name: "bad github api url", args: []string{"--github-api-url", "api"}, env: testEnv(), wantErr: []string{"--github-api-url"}},
		{name: "extra args", args: []string{"now"}, env: testEnv(), wantErr: []string{"unexpected arguments"}},
		{name: "deploy environment", args: []string{"--deploy-environment", "production"}, env: testEnv(), check: func(t *testing.T, c serveConfig, _ []string) {
			if c.DeployEnvironment != "production" {
				t.Errorf("deploy environment %q", c.DeployEnvironment)
			}
		}},
		{name: "state dir", args: []string{"--state-dir", filepath.Join(dir, "state")}, bare: true, env: testEnv(), check: func(t *testing.T, c serveConfig, _ []string) {
			if c.StateDir != filepath.Join(dir, "state") {
				t.Errorf("state dir %q", c.StateDir)
			}
		}},
		{name: "state dir is a file", args: []string{"--state-dir", file}, bare: true, env: testEnv(), wantErr: []string{"--state-dir", "is not a directory"}},
		{name: "anti-affinity wait", args: []string{"--anti-affinity-wait", "3m"}, env: testEnv(), check: func(t *testing.T, c serveConfig, _ []string) {
			if c.AntiAffinityWait != 3*time.Minute {
				t.Errorf("anti-affinity wait %v", c.AntiAffinityWait)
			}
		}},
		{name: "anti-affinity off", args: []string{"--anti-affinity-wait", "0"}, env: testEnv(), check: func(t *testing.T, c serveConfig, _ []string) {
			if c.AntiAffinityWait != scheduler.AntiAffinityOff {
				t.Errorf("anti-affinity wait %v, want off", c.AntiAffinityWait)
			}
		}},
		{name: "negative anti-affinity wait", args: []string{"--anti-affinity-wait", "-1m"}, env: testEnv(), wantErr: []string{"--anti-affinity-wait must not be negative"}},
		{name: "empty deploy environment", args: []string{"--deploy-environment", " "}, env: testEnv(), wantErr: []string{"--deploy-environment must not be empty"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if !tt.bare {
				args = append([]string{"--state-memory"}, args...)
			}
			cfg, warnings, err := parseServeConfig(args, getenvOf(tt.env), io.Discard)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if tt.check != nil {
					tt.check(t, cfg, warnings)
				}
				return
			}
			if err == nil {
				t.Fatalf("no error, want %v", tt.wantErr)
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want %q", err, w)
				}
			}
			for _, secret := range []string{testCLIToken, "oauth-secret-1", "session-key-0123"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error carries a secret: %v", err)
				}
			}
		})
	}
}

func TestSecretEnv(t *testing.T) {
	for ref, want := range map[v1alpha1.SecretRef]string{
		{Name: "local-pool", Key: "join-token"}:        "FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN",
		{Name: "ccf-factory-app", Key: "private-key"}:  "FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY",
		{Name: "github-public-webhook", Key: "secret"}: "FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET",
		{Name: "a.b", Key: "c/d"}:                      "FACTORY_SECRET_A_B_C_D",
	} {
		if got := secretEnv(ref); got != want {
			t.Errorf("secretEnv(%v) = %s, want %s", ref, got, want)
		}
	}
	s := envSecrets(getenvOf(map[string]string{"FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN": "v"}))
	if v, err := s.Resolve(context.Background(), v1alpha1.SecretRef{Name: "local-pool", Key: "join-token"}); err != nil || string(v) != "v" {
		t.Errorf("resolve: %q %v", v, err)
	}
	if _, err := s.Resolve(context.Background(), v1alpha1.SecretRef{Name: "x", Key: "y"}); err == nil || !strings.Contains(err.Error(), "FACTORY_SECRET_X_Y is not set") {
		t.Errorf("resolve missing: %v", err)
	}
}

// enrolled mints an enrollment for gus's agent on host with the CLI token and returns the
// request editor that registers with it.
func enrolled(t *testing.T, ctx context.Context, c *apiclient.ClientWithResponses, host string) apiclient.RequestEditorFn {
	t.Helper()
	res, err := c.CreateEnrollmentWithResponse(ctx, apiclient.EnrollmentRequest{Pool: "local", Owner: "gus", Name: host}, bearer(testCLIToken))
	if err != nil || res.JSON201 == nil {
		t.Fatalf("enroll %s: %v %s", host, err, res.Body)
	}
	tok := res.JSON201.Token
	return func(_ context.Context, r *http.Request) error {
		r.Header.Set(server.HeaderEnrollmentToken, tok)
		return nil
	}
}

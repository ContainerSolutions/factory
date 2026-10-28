package worker_test

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/enroll"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/agent/worker"
	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

const (
	pocDir       = "../../catalog/testdata/poc"
	ghProvider   = "github-public"
	testCLIToken = "cli-token-0123456789abcdef0123456789abcdef"
	testOAuthKey = "oauth-secret-for-tests-only"
	testSession  = "session-key-for-tests-only-0123456789abcdef"
)

// testCatalog is the PoC catalog with one change: the maintainers Group (the local Pool's
// owners) lists the owner by login. The PoC's group is the CCF org admins, which the fake
// provider of a `factory serve` subprocess can't be seeded with.
func testCatalog(t *testing.T, login string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(pocDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(pocDir, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	group := "kind: Group\nmetadata: { name: maintainers }\nspec:\n  members:\n    - { provider: github-public, users: [" + login + "] }\n"
	if err := os.WriteFile(filepath.Join(dst, "groups", "maintainers.yaml"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// freeAddr is a loopback address with a port free right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// served is a `factory serve` subprocess.
type served struct {
	URL  string
	cmd  *exec.Cmd
	logs *logBuffer
	done chan error
	// exited is closed once the process has exited (after done got its error).
	exited chan struct{}
}

// startServe builds cmd/factory and runs `factory serve --dev-fake-providers` on the test
// catalog, with test-only secrets and GitHub (OAuth) at githubURL, and waits
// for /readyz.
func startServe(t *testing.T, githubURL string) *served {
	t.Helper()
	bin := buildBinary(t, "github.com/ContainerSolutions/factory/cmd/factory")
	addr, ops := freeAddr(t), freeAddr(t)
	cmd := exec.Command(bin, "serve", "--dev-fake-providers", "--catalog-dir", testCatalog(t, "gus"),
		"--addr", addr, "--ops-addr", ops, "--public-url", "http://"+addr,
		"--github-url", githubURL, "--github-api-url", githubURL, "--skills-base", "",
		"--shutdown-timeout", "5s", "--log-level", "debug")
	cmd.Env = append(os.Environ(),
		"FACTORY_OAUTH_CLIENT_ID=client-1",
		"FACTORY_OAUTH_CLIENT_SECRET="+testOAuthKey,
		"FACTORY_SESSION_KEY="+testSession,
		"FACTORY_CLI_TOKEN="+testCLIToken,
	)
	s := &served{URL: "http://" + addr, cmd: cmd, logs: &logBuffer{}, done: make(chan error, 1), exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = s.logs, s.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		s.done <- cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(func() { // after a failure stop wasn't called: kill it
		select {
		case <-s.exited:
		default:
			_ = cmd.Process.Kill()
			<-s.exited
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err := http.Get("http://" + ops + "/readyz")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return s
			}
		}
		select {
		case err := <-s.done:
			s.done <- err
			t.Fatalf("factory serve exited: %v\n%s", err, s.logs)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("factory serve never got ready\n%s", s.logs)
		}
	}
}

// stop sends SIGTERM and waits for a clean exit. It closes the default transport's idle
// connections first: one dialled for a request but never used is new on the server, which
// counts it as active for 5 s, the whole --shutdown-timeout.
func (s *served) stop(t *testing.T) {
	t.Helper()
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("factory serve exited with %v\n%s", err, s.logs)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("factory serve didn't stop on SIGTERM\n%s", s.logs)
	}
	if !strings.Contains(s.logs.String(), `"msg":"shutdown complete"`) {
		t.Errorf("no clean shutdown in the logs:\n%s", s.logs)
	}
}

func cliClient(t *testing.T, base string) (*apiclient.ClientWithResponses, apiclient.RequestEditorFn) {
	t.Helper()
	c, err := apiclient.NewClientWithResponses(worker.APIBase(base))
	if err != nil {
		t.Fatal(err)
	}
	return c, func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+testCLIToken)
		return nil
	}
}

// noGitHub is serve's GitHub (OAuth) URL in these tests, which nothing calls: agents are
// enrolled, and no one signs in.
func noGitHub(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// enrolled mints an enrollment for gus's agent on host with the CLI token and returns the
// agent's credential files: the token in a 0600 file, an empty state dir.
func enrolled(t *testing.T, base, host string) (enroll.Files, string) {
	t.Helper()
	c, admin := cliClient(t, base)
	res, err := c.CreateEnrollmentWithResponse(context.Background(), apiclient.EnrollmentRequest{Pool: "local", Owner: "gus", Name: host}, admin)
	if err != nil || res.JSON201 == nil {
		t.Fatalf("enroll %s: %v %s", host, err, res.Body)
	}
	file := filepath.Join(t.TempDir(), "enrollment-token")
	if err := os.WriteFile(file, []byte(res.JSON201.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return enroll.Files{StateDir: t.TempDir(), TokenFile: file}, res.JSON201.Token
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAgainstRealDispatcher runs the worker against the real dispatcher: the `factory serve`
// binary with --dev-fake-providers and the PoC catalog (owners by login, see testCatalog). A
// recorded webhook creates the plan-epic task; the worker registers with an enrollment the
// CLI token minted (its refresh token saved in the state dir), leases it, heartbeats and completes it with
// the stub runner's Result; the task succeeds with the run's progress recorded; a drain
// deregisters the agent; serve stops cleanly on SIGTERM.
func TestAgainstRealDispatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs factory serve")
	}
	ctx := context.Background()
	d := startServe(t, noGitHub(t))
	cli, admin := cliClient(t, d.URL)

	body, err := os.ReadFile(filepath.Join("testdata", "issue-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(d.URL+"/hooks/"+ghProvider, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %s", res.Status)
	}

	enr, enrollTok := enrolled(t, d.URL, "e2e")
	logs := &logBuffer{}
	stub := &stubRunner{
		Progress: []v1alpha1.ProgressEvent{{At: time.Now().UTC(), Kind: "step", Message: "planning the epic"}},
		Usage:    &runner.UsageDelta{InputTokens: 1200, OutputTokens: 300},
		Outcome: runner.Outcome{Result: &v1alpha1.Result{
			Status: v1alpha1.ResultDone, Outcome: "decomposed", Summary: "two sub-issues",
			Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"},
				Usage: v1alpha1.Usage{InputTokens: 1200, OutputTokens: 300}},
			Data: map[string]any{"plan": []any{
				map[string]any{"issue": "compliance-framework/docs#43", "title": "Translate the intro"},
				map[string]any{"issue": "compliance-framework/docs#44", "title": "Translate the guides"},
			}},
		}},
	}
	started := stub.Started()
	w, err := worker.New(worker.Options{
		Dispatcher: d.URL, Pool: "local", Host: "e2e", AgentVersion: "test",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending:    v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
		Enrollment: enr,
		Runner:     stub,
		Poll:       worker.Poll{Wait: 5 * time.Second},
		Logger:     slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	var job *runner.Job
	select {
	case job = <-started:
	case err := <-done:
		t.Fatalf("Run returned: %v\nworker:\n%s\ndispatcher:\n%s", err, logs, d.logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("no lease\nworker:\n%s", logs)
	}
	if job.Lease.Task.Spec.TaskSpec.Name != "plan-epic" || w.AgentID() != "agt_gus-e2e" {
		t.Fatalf("leased %s as %s", job.Lease.Task.Spec.TaskSpec.Name, w.AgentID())
	}
	taskID := job.TaskID()
	eventually(t, "the task to succeed", func() bool {
		r, err := cli.GetTaskWithResponse(ctx, taskID, admin)
		return err == nil && r.JSON200 != nil && r.JSON200.Status.Phase == v1alpha1.TaskSucceeded
	})
	if job.Lease.Lease.Run == nil {
		t.Fatal("the lease names no run")
	}
	run, err := cli.GetRunWithResponse(ctx, *job.Lease.Lease.Run, admin)
	if err != nil || run.JSON200 == nil || run.JSON200.Run.Status == nil {
		t.Fatalf("get run: %v", err)
	}
	var progress []string
	for _, p := range run.JSON200.Run.Status.Progress {
		progress = append(progress, p.Message)
	}
	if !strings.Contains(strings.Join(progress, "|"), "planning the epic") {
		t.Errorf("run progress %q lacks the stub's event", progress)
	}
	if r := run.JSON200.Run.Status.Result; r == nil || r.Outcome != "decomposed" {
		t.Errorf("run result %+v", r)
	}

	w.Drain()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run didn't return after Drain")
	}
	agents, err := cli.ListAgentsWithResponse(ctx, &apiclient.ListAgentsParams{}, admin)
	if err != nil || agents.JSON200 == nil || len(agents.JSON200.Items) != 1 || agents.JSON200.Items[0].Status.Phase != v1alpha1.AgentOffline {
		t.Errorf("agents after the drain: %v %+v", err, agents.JSON200)
	}
	d.stop(t)
	if !enr.Enrolled() {
		t.Error("no refresh token saved in the state dir")
	}
	refresh, _, _ := enr.Token()
	for _, s := range []string{enrollTok, refresh, "agt_gus-e2e.", testCLIToken, testOAuthKey, testSession} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a secret is in the worker logs: %q", s)
		}
		if strings.Contains(d.logs.String(), s) {
			t.Errorf("a secret is in the dispatcher logs: %q", s)
		}
	}
}

// leasePolls is an HTTP client that records each lease poll in the order it was sent, with its
// wait and the answer's status.
type leasePolls struct {
	mu    sync.Mutex
	polls []leasePoll
}

type leasePoll struct {
	wait   string
	status int // 0 until answered
}

func (p *leasePolls) Do(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/lease") {
		return http.DefaultTransport.RoundTrip(req)
	}
	p.mu.Lock()
	i := len(p.polls)
	p.polls = append(p.polls, leasePoll{wait: req.URL.Query().Get("wait")})
	p.mu.Unlock()
	res, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		p.mu.Lock()
		p.polls[i].status = res.StatusCode
		p.mu.Unlock()
	}
	return res, err
}

func (p *leasePolls) all() []leasePoll {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.polls)
}

// TestBriefPollAgainstRealDispatcher runs a brief-polling worker (wait 0s, a 2 s interval)
// against the real dispatcher (P3 §Agents poll briefly): each idle poll is answered 204 at
// once, with wait=0s on the wire, and a task created while the agent sleeps its interval is
// leased on its next poll.
func TestBriefPollAgainstRealDispatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs factory serve")
	}
	ctx := context.Background()
	d := startServe(t, noGitHub(t))
	cli, admin := cliClient(t, d.URL)
	enr, _ := enrolled(t, d.URL, "brief")
	stub := &stubRunner{Outcome: runner.Outcome{Result: &v1alpha1.Result{
		Status: v1alpha1.ResultDone, Outcome: "single", Summary: "one issue",
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}},
		Data:    map[string]any{"plan": []any{}},
	}}}
	started := stub.Started()
	polls := &leasePolls{}
	w, err := worker.New(worker.Options{
		Dispatcher: d.URL, Pool: "local", Host: "brief", AgentVersion: "test",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending:    v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
		Enrollment: enr,
		Runner:     stub,
		HTTPClient: polls,
		Poll:       worker.Poll{Interval: 2 * time.Second},
		Logger:     slog.New(slog.NewJSONHandler(&logBuffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Idle: two polls, each answered 204 at once and an interval apart.
	start := time.Now()
	eventually(t, "two idle polls answered", func() bool {
		p := polls.all()
		return len(p) >= 2 && p[1].status != 0
	})
	if el := time.Since(start); el < 2*time.Second {
		t.Errorf("two polls in %s: the agent didn't sleep the interval after the 204", el)
	}
	for i, p := range polls.all()[:2] {
		if p.status != http.StatusNoContent {
			t.Errorf("idle poll %d answered %d, want 204", i+1, p.status)
		}
	}

	// A task created while the agent sleeps: the webhook creates the plan-epic task.
	body, err := os.ReadFile(filepath.Join("testdata", "issue-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(d.URL+"/hooks/"+ghProvider, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %s", res.Status)
	}
	var sent int // polls sent before the task was seen pending
	eventually(t, "the task to be pending (or leased)", func() bool {
		sent = len(polls.all())
		r, err := cli.ListTasksWithResponse(ctx, &apiclient.ListTasksParams{}, admin)
		return err == nil && r.JSON200 != nil && len(r.JSON200.Items) > 0
	})
	var job *runner.Job
	select {
	case job = <-started:
	case err := <-done:
		t.Fatalf("Run returned: %v\n%s", err, d.logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("no lease; polls %+v", polls.all())
	}
	if job.Lease.Task.Spec.TaskSpec.Name != "plan-epic" {
		t.Fatalf("leased %s", job.Lease.Task.Spec.TaskSpec.Name)
	}
	all := polls.all()
	leased := slices.IndexFunc(all, func(p leasePoll) bool { return p.status == http.StatusOK })
	if leased < 0 || leased > sent {
		t.Errorf("leased on poll %d, but %d polls were sent before the task was pending: want it on the next one (polls %+v)", leased+1, sent, all)
	}
	for i, p := range all {
		if p.wait != "0s" {
			t.Errorf("poll %d sent wait=%q, want 0s", i+1, p.wait)
		}
	}
	eventually(t, "the task to succeed", func() bool {
		r, err := cli.GetTaskWithResponse(ctx, job.TaskID(), admin)
		return err == nil && r.JSON200 != nil && r.JSON200.Status.Phase == v1alpha1.TaskSucceeded
	})

	w.Drain()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run didn't return after Drain")
	}
	d.stop(t)
}

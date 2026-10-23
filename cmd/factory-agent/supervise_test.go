package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/login"
	"github.com/compliance-framework/factory/internal/agent/owner"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/agent/workspace"
	"github.com/compliance-framework/factory/internal/agent/workspace/tart"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// TestMain answers `fx …` as factory-agent does: the host pipeline's skills fetch runs the
// test binary as its git credential helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fx" {
		os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// supDriver is an in-process workspace.Driver: each VM is a directory under root, and Exec
// plays factory-agent run by writing a done outcome and a transcript. Every call is recorded.
type supDriver struct {
	root   string
	listed []string

	mu    sync.Mutex
	calls []string
	cfg   tart.Config
}

func (d *supDriver) record(op, name string) error {
	d.mu.Lock()
	d.calls = append(d.calls, op+" "+name)
	d.mu.Unlock()
	return workspace.ValidName(name)
}

func (d *supDriver) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *supDriver) vm(name, p string) string {
	return filepath.Join(d.root, name, filepath.FromSlash(p))
}

func (d *supDriver) Create(_ context.Context, name string, _ workspace.Spec) error {
	return d.record("create", name)
}

func (d *supDriver) Start(_ context.Context, name string) (workspace.Network, error) {
	return workspace.Network{VM: netip.MustParseAddr("192.0.2.10"), Gateway: netip.MustParseAddr("127.0.0.1")}, d.record("start", name)
}

func (d *supDriver) Put(_ context.Context, name, _, _ string) error { return d.record("put", name) }

func (d *supDriver) Exec(_ context.Context, name string, argv []string, _ io.Reader, stdout, _ io.Writer) error {
	if err := d.record("exec", name); err != nil {
		return err
	}
	if len(argv) < 2 || argv[0] != workspace.AgentCommand || argv[1] != "run" {
		return fmt.Errorf("argv %v", argv)
	}
	task := strings.TrimPrefix(name, workspace.Prefix)
	o := runbundle.Outcome{Status: runner.StatusDone, Result: &v1alpha1.Result{
		Task: task, Status: v1alpha1.ResultDone, Outcome: "pr-opened", Summary: "pushed the intro"}, Transcript: runbundle.TranscriptFile}
	out := d.vm(name, workspace.VMOutDir)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, `{"type":"assistant","text":"done"}`+"\n")
	if err := zw.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, runbundle.TranscriptFile), gz.Bytes(), 0o600); err != nil {
		return err
	}
	if err := runbundle.WriteOutcome(out, o); err != nil {
		return err
	}
	return runbundle.NewEncoder(stdout, time.Now).Encode(runbundle.OutcomeEvent(o))
}

func (d *supDriver) Get(_ context.Context, name, src, dst string, _ ...string) error {
	if err := d.record("get", name); err != nil {
		return err
	}
	from := d.vm(name, src)
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	es, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range es {
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (d *supDriver) Stop(_ context.Context, name string) error    { return d.record("stop", name) }
func (d *supDriver) Destroy(_ context.Context, name string) error { return d.record("destroy", name) }
func (d *supDriver) List(context.Context) ([]string, error)       { return d.listed, nil }

// supervisedTest is a supervise run's setup: a config with codex, its login in the state dir,
// and deps whose host check passes and whose driver is d.
type supervisedTest struct {
	dir, cfg, state string
	driver          *supDriver
	deps            superviseDeps
}

func newSupervisedTest(t *testing.T, dispatcher string) *supervisedTest {
	t.Helper()
	s := &supervisedTest{dir: t.TempDir(), driver: &supDriver{root: t.TempDir()}}
	s.cfg = writeConfig(t, s.dir, dispatcher, "")
	s.state = filepath.Join(s.dir, "state")
	if err := login.ForStateDir(s.state).SaveCodex(login.Codex{AccessToken: "codex-login-access-0123", AccountID: "acct_1", UserID: "user_1", PlanType: "pro"}); err != nil {
		t.Fatal(err)
	}
	hd, _ := hostTestDeps(t)
	hd.Env.Home = s.dir
	// the host's state dir, which host check would have made: the supervise lock is in it.
	if err := os.MkdirAll(hd.Env.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	s.deps = superviseDeps{
		Worker: workerDeps{Config: homeIn(s.dir), Owner: owner.TokenFunc(func(context.Context) (string, error) { return ownerSecret, nil })},
		Host:   hd.Env,
		HostCheck: func(context.Context, *host.Env) ([]host.Status, error) {
			return []host.Status{{Step: "platform", State: host.StateOK}, {Step: "pf", State: host.StateSkipped, Detail: "opted out"}}, nil
		},
		NewDriver: func(c tart.Config) workspace.Driver {
			s.driver.mu.Lock()
			s.driver.cfg = c
			s.driver.mu.Unlock()
			return s.driver
		},
		Listen: func(ctx context.Context, a netip.AddrPort) (net.Listener, error) {
			if a != netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), workspace.GatewayPort) {
				return nil, fmt.Errorf("listen on %s", a)
			}
			var lc net.ListenConfig
			return lc.Listen(ctx, "tcp", "127.0.0.1:0")
		},
	}
	return s
}

func (s *supervisedTest) args(extra ...string) []string {
	return append([]string{"--config", s.cfg, "--state-dir", s.state}, extra...)
}

func TestSuperviseUsage(t *testing.T) {
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"now"}, 2, "unexpected arguments"},
		{[]string{"--driver", "kata"}, 2, `--driver "kata": only tart is supported`},
		{[]string{"--log-level", "loud"}, 2, "--log-level"},
		{[]string{"--nope"}, 2, "flag provided but not defined"},
		{[]string{"-h"}, 0, "-warm-base"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var errOut bytes.Buffer
			if code := runSupervise(context.Background(), tt.args, &errOut, superviseDeps{}); code != tt.code || !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("runSupervise = %d, want %d with %q:\n%s", code, tt.code, tt.want, errOut.String())
			}
		})
	}
}

// supervise doesn't start without the harnesses' logins, or on a host that host check
// doesn't pass; neither builds a driver.
func TestSuperviseRefusals(t *testing.T) {
	t.Run("host check", func(t *testing.T) {
		s := newSupervisedTest(t, "http://127.0.0.1:1")
		s.deps.HostCheck = nil // the real steps, on the fake Mac: Softnet is missing
		var errOut bytes.Buffer
		if code := runSupervise(context.Background(), s.args(), &errOut, s.deps); code != 1 {
			t.Fatalf("code %d:\n%s", code, errOut.String())
		}
		for _, w := range []string{"softnet   missing", "run `factory-agent host init --driver tart`"} {
			if !strings.Contains(errOut.String(), w) {
				t.Errorf("stderr lacks %q:\n%s", w, errOut.String())
			}
		}
		if strings.Contains(errOut.String(), "tart      ok") {
			t.Errorf("ok steps are listed:\n%s", errOut.String())
		}
	})
	t.Run("no login", func(t *testing.T) {
		s := newSupervisedTest(t, "http://127.0.0.1:1")
		if err := os.Remove(filepath.Join(s.state, login.Dir, login.CodexFile)); err != nil {
			t.Fatal(err)
		}
		var errOut bytes.Buffer
		if code := runSupervise(context.Background(), s.args(), &errOut, s.deps); code != 1 ||
			!strings.Contains(errOut.String(), "run `factory-agent login codex`") || !strings.Contains(errOut.String(), "no harness passed its probe") {
			t.Fatalf("code %d:\n%s", code, errOut.String())
		}
	})
	t.Run("another supervise", func(t *testing.T) {
		s := newSupervisedTest(t, "http://127.0.0.1:1")
		s.driver.listed = []string{"ws-tsk_live"}
		e := s.deps.Host
		release, err := host.LockSupervise(&e)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		// bounded: an unlocked supervise would go on to serve.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var errOut bytes.Buffer
		if code := runSupervise(ctx, s.args(), &errOut, s.deps); code != 1 ||
			!strings.Contains(errOut.String(), "another factory-agent supervise is running") {
			t.Fatalf("code %d:\n%s", code, errOut.String())
		}
		if calls := s.driver.Calls(); len(calls) != 0 || s.driver.cfg.Bin != "" {
			t.Errorf("a locked supervise built a driver or called it: %v", calls)
		}
	})
	t.Run("list fails", func(t *testing.T) {
		if err := destroyOrphans(context.Background(), failingList{}, discardLog()); err == nil || !strings.Contains(err.Error(), "leftover") {
			t.Errorf("destroyOrphans = %v", err)
		}
	})
}

type failingList struct{ workspace.Driver }

func (failingList) List(context.Context) ([]string, error) { return nil, errors.New("tart list: boom") }

// The orphan cleanup destroys every listed workspace and nothing else.
func TestDestroyOrphans(t *testing.T) {
	d := &supDriver{listed: []string{"ws-tsk_a", "ws-1", "ws-gw", "factory-agent-warm", "ws-tsk_b", "ws-tsk_../x"}}
	if err := destroyOrphans(context.Background(), d, discardLog()); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Calls(), []string{"destroy ws-tsk_a", "destroy ws-tsk_b"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

// leaseDispatcher offers one lease, records its completion, and passes everything else (the
// prompt, the transcript) to harnesstest's dispatcher.
type leaseDispatcher struct {
	lease apiclient.Lease
	next  http.Handler

	mu       sync.Mutex
	offered  bool
	complete []apiclient.CompleteRequest
	lending  []v1alpha1.Lending
}

func (l *leaseDispatcher) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	body, _ := io.ReadAll(r.Body)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodPost && p == "/agents":
		var req apiclient.RegisterAgentRequest
		_ = json.Unmarshal(body, &req)
		if req.Lending != nil {
			l.mu.Lock()
			l.lending = append(l.lending, *req.Lending)
			l.mu.Unlock()
		}
		reply(http.StatusCreated, apiclient.AgentRegistration{AgentId: "agt_gus-cmd-test", AgentToken: "agt_gus-cmd-test.secret", ExpiresAt: time.Now().Add(time.Hour)})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/lease"):
		l.mu.Lock()
		first := !l.offered
		l.offered = true
		l.mu.Unlock()
		if first {
			reply(http.StatusOK, l.lease)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/heartbeat"):
		reply(http.StatusOK, apiclient.HeartbeatResponse{ExpiresAt: time.Now().Add(2 * time.Minute)})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/complete"):
		var c apiclient.CompleteRequest
		_ = json.Unmarshal(body, &c)
		l.mu.Lock()
		l.complete = append(l.complete, c)
		l.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/agents/"):
		var u apiclient.AgentUpdate
		_ = json.Unmarshal(body, &u)
		if u.Lending != nil {
			l.mu.Lock()
			l.lending = append(l.lending, *u.Lending)
			l.mu.Unlock()
		}
		reply(http.StatusOK, v1alpha1.Agent{})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/agents/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		r.Body = io.NopCloser(bytes.NewReader(body))
		l.next.ServeHTTP(w, r)
	}
}

func (l *leaseDispatcher) lendings() []v1alpha1.Lending {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lending)
}

func (l *leaseDispatcher) completed() []apiclient.CompleteRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.complete)
}

// One lease through supervise: registration, the orphan cleanup, the task in a workspace on
// the fake driver behind the gateway, the result completed and the transcript uploaded, then
// a SIGHUP reload and a drain.
func TestSuperviseOneLease(t *testing.T) {
	e := harnesstest.NewE2E(t, "tsk_sup1", "Write the intro.\n")
	lease := e.Job(v1alpha1.HarnessResolved{Name: "codex", Model: "gpt-5-codex"}, []string{"pr-opened"}).Lease
	lease.Lease.Agent, lease.Lease.ExpiresAt, lease.Lease.HeartbeatEvery = "agt_gus-cmd-test", time.Now().Add(2*time.Minute), "30s"
	next, err := url.Parse(e.DispatcherURL())
	if err != nil {
		t.Fatal(err)
	}
	disp := &leaseDispatcher{lease: lease, next: httputil.NewSingleHostReverseProxy(next)}
	ts := httptest.NewServer(http.HandlerFunc(disp.serve))
	t.Cleanup(ts.Close)

	s := newSupervisedTest(t, ts.URL)
	s.driver.listed = []string{"ws-tsk_old", "ws-1"}
	sigs := make(chan os.Signal, 2)
	s.deps.Worker.Signals = sigs
	errOut := &syncBuffer{}
	code := make(chan int, 1)
	go func() {
		code <- runSupervise(context.Background(), s.args("--warm-base", "my-warm", "--log-level", "debug"), errOut, s.deps)
	}()
	waitUntil(t, "the completion", func() bool { return len(disp.completed()) == 1 })
	// SIGHUP: the reloaded lending keeps maxConcurrent at 1.
	if err := os.WriteFile(s.cfg, bytes.Replace(mustRead(t, s.cfg), []byte("queues: [poc]"), []byte("queues: [poc]\n  maxConcurrent: 3"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGHUP
	waitUntil(t, "the lending PATCH", func() bool { return len(disp.lendings()) == 2 })
	sigs <- syscall.SIGTERM
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("supervise exited %d\n%s", c, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("supervise didn't drain\n%s", errOut.String())
	}

	c := disp.completed()[0]
	if c.Result.Status != v1alpha1.ResultDone || c.Result.Outcome != "pr-opened" || c.Result.Task != "tsk_sup1" || c.TranscriptRef == nil || *c.TranscriptRef == "" {
		t.Errorf("complete %+v", c)
	}
	ws := "ws-tsk_sup1"
	if got, want := s.driver.Calls(), []string{"destroy ws-tsk_old", "create " + ws, "start " + ws, "put " + ws, "exec " + ws, "get " + ws, "destroy " + ws}; !slices.Equal(got, want) {
		t.Errorf("driver calls %v, want %v", got, want)
	}
	if s.driver.cfg.WarmBase != "my-warm" || s.driver.cfg.Bin == "" {
		t.Errorf("driver config %+v", s.driver.cfg)
	}
	if l := disp.lendings(); len(l) != 2 || l[0].MaxConcurrent != 1 || l[1].MaxConcurrent != 1 || len(l[1].Queues) != 1 {
		t.Errorf("registered and reloaded lending %+v, want maxConcurrent 1", l)
	}
	if types := e.TranscriptTypes(); !slices.Equal(types, []string{"assistant"}) {
		t.Errorf("transcript types %q", types)
	}
	for _, sec := range []string{joinSecret, ownerSecret, "agt_gus-cmd-test.secret", harnesstest.GitToken, "codex-login-access-0123"} {
		if strings.Contains(errOut.String(), sec) {
			t.Errorf("a secret is in the logs: %q", sec)
		}
	}
	if _, err := os.Stat(filepath.Join(s.state, "runs", "tsk_sup1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run directory is left: %v", err)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

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

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/agent/host"
	"github.com/ContainerSolutions/factory/internal/agent/login"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/agent/worker"
	"github.com/ContainerSolutions/factory/internal/agent/workspace"
	"github.com/ContainerSolutions/factory/internal/agent/workspace/kata"
	"github.com/ContainerSolutions/factory/internal/agent/workspace/tart"
	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
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
// The nth Start gives the workspace the gateway 127.0.0.n, as a kata slot has its own.
type supDriver struct {
	root   string
	listed []string
	// together makes each Exec wait until that many have started (concurrency).
	together int

	mu      sync.Mutex
	calls   []string
	cfg     tart.Config
	kataCfg kata.Config
	spec    workspace.Spec
	started int
	execs   int
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

func (d *supDriver) Create(_ context.Context, name string, spec workspace.Spec) error {
	d.mu.Lock()
	d.spec = spec
	d.mu.Unlock()
	return d.record("create", name)
}

func (d *supDriver) Start(_ context.Context, name string) (workspace.Network, error) {
	d.mu.Lock()
	d.started++
	n := byte(d.started)
	d.mu.Unlock()
	return workspace.Network{VM: netip.AddrFrom4([4]byte{192, 0, 2, 10 + n}), Gateway: netip.AddrFrom4([4]byte{127, 0, 0, n})}, d.record("start", name)
}

// waitTogether returns once d.together Execs have started, or fails after 10 s.
func (d *supDriver) waitTogether() error {
	d.mu.Lock()
	d.execs++
	d.mu.Unlock()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d.mu.Lock()
		n := d.execs
		d.mu.Unlock()
		if n >= d.together {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d workspaces ran at once", n, d.together)
		}
	}
}

func (d *supDriver) Put(_ context.Context, name, _, _ string) error { return d.record("put", name) }

func (d *supDriver) Exec(_ context.Context, name string, argv []string, _ io.Reader, stdout, _ io.Writer) error {
	if err := d.record("exec", name); err != nil {
		return err
	}
	if len(argv) < 2 || argv[0] != workspace.AgentCommand || argv[1] != "run" {
		return fmt.Errorf("argv %v", argv)
	}
	if err := d.waitTogether(); err != nil {
		return err
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
		Worker: workerDeps{Config: homeIn(s.dir)},
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
		NewKataDriver: func(c kata.Config) workspace.Driver {
			s.driver.mu.Lock()
			s.driver.kataCfg = c
			s.driver.mu.Unlock()
			return s.driver
		},
		Listen: func(ctx context.Context, a netip.AddrPort) (net.Listener, error) {
			if a != netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), workspace.DefaultGatewayPort) {
				return nil, fmt.Errorf("listen on %s", a)
			}
			var lc net.ListenConfig
			return lc.Listen(ctx, "tcp", "127.0.0.1:0")
		},
	}
	return s
}

// newKataSupervisedTest is newSupervisedTest on a fake Linux host with 61 GiB of memory, the
// config's lending and workspace blocks ending in extra; host check passes unless the caller
// clears HostCheck.
func newKataSupervisedTest(t *testing.T, dispatcher, extra string) *supervisedTest {
	t.Helper()
	s := newSupervisedTest(t, dispatcher)
	s.cfg = writeConfig(t, s.dir, dispatcher, extra)
	e := kataHostDeps(t).Env
	e.Home = s.dir
	if err := os.MkdirAll(e.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	s.deps.Host = e
	s.deps.HostCheck = func(context.Context, *host.Env) ([]host.Status, error) {
		return []host.Status{{Step: "platform", State: host.StateOK}, {Step: host.KataWarmStep, State: host.StateOK}}, nil
	}
	return s
}

// kataRecords writes host init's katabase.json and katawarm.json into the host's state dir.
func (s *supervisedTest) kataRecords(t *testing.T) {
	t.Helper()
	for p, b := range map[string]string{
		s.deps.Host.KataBaseRecordPath(): `{"image": "factory-agent-base:dev", "contextSHA256": "ab", "builtAt": "2026-10-26T12:00:00Z"}`,
		s.deps.Host.KataWarmRecordPath(): `{"image": "factory-agent-base:dev"}`,
	} {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
		{[]string{"--driver", "vz"}, 2, `--driver "vz" is not tart or kata`},
		{[]string{"--driver", "kata", "--warm-base", "my-warm"}, 2, "--warm-base is the tart driver's, not kata's"},
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
	t.Run("kata host check", func(t *testing.T) {
		s := newKataSupervisedTest(t, "http://127.0.0.1:1", "")
		s.deps.HostCheck = nil // the real kata steps, on the fake Linux host: nothing installed
		var errOut bytes.Buffer
		if code := runSupervise(context.Background(), s.args("--driver", "kata"), &errOut, s.deps); code != 1 {
			t.Fatalf("code %d:\n%s", code, errOut.String())
		}
		for _, w := range []string{"nerdctl   missing", "katawarm  missing", "run `factory-agent host init --driver kata`"} {
			if !strings.Contains(errOut.String(), w) {
				t.Errorf("stderr lacks %q:\n%s", w, errOut.String())
			}
		}
		if strings.Contains(errOut.String(), "platform  ok") || len(s.driver.Calls()) != 0 {
			t.Errorf("ok steps are listed, or the driver was called:\n%s", errOut.String())
		}
	})
	t.Run("warm base with the config's kata", func(t *testing.T) {
		s := newKataSupervisedTest(t, "http://127.0.0.1:1", "workspace:\n  driver: kata\n")
		var errOut bytes.Buffer
		if code := runSupervise(context.Background(), s.args("--warm-base", "my-warm"), &errOut, s.deps); code != 1 ||
			!strings.Contains(errOut.String(), "--warm-base is the tart driver's, not kata's (workspace.driver is kata)") {
			t.Fatalf("code %d:\n%s", code, errOut.String())
		}
	})
	t.Run("no image record", func(t *testing.T) {
		s := newKataSupervisedTest(t, "http://127.0.0.1:1", "")
		var errOut bytes.Buffer
		if code := runSupervise(context.Background(), s.args(), &errOut, s.deps); code != 1 ||
			!strings.Contains(errOut.String(), "names no image: run `factory-agent host init --driver kata`") {
			t.Fatalf("code %d:\n%s", code, errOut.String())
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

// The orphan cleanup destroys every listed workspace and nothing else: not host init's warm
// build workspace either.
func TestDestroyOrphans(t *testing.T) {
	d := &supDriver{listed: []string{"ws-tsk_a", "ws-1", "ws-gw", "factory-agent-warm", "ws-tsk_b", "ws-tsk_../x", host.KataWarmBuild}}
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
	leases []apiclient.Lease
	next   http.Handler

	mu       sync.Mutex
	offered  int
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
		if r.Header.Get(worker.HeaderEnrollmentToken) != enrollSecret && r.Header.Get(worker.HeaderAgentRefreshToken) != refreshSecret {
			reply(http.StatusUnauthorized, apiclient.Problem{Title: "Unauthorized", Status: http.StatusUnauthorized})
			return
		}
		rt := refreshSecret
		reply(http.StatusCreated, apiclient.AgentRegistration{AgentId: "agt_gus-cmd-test", AgentToken: "agt_gus-cmd-test.secret", ExpiresAt: time.Now().Add(time.Hour), RefreshToken: &rt})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/lease"):
		l.mu.Lock()
		var next *apiclient.Lease
		if l.offered < len(l.leases) {
			next = &l.leases[l.offered]
			l.offered++
		}
		l.mu.Unlock()
		if next != nil {
			reply(http.StatusOK, *next)
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
	disp := &leaseDispatcher{leases: []apiclient.Lease{lease}, next: httputil.NewSingleHostReverseProxy(next)}
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
	if sp := s.driver.spec; sp != (workspace.Spec{CPU: 6, MemoryMiB: 8 << 10}) {
		t.Errorf("workspace spec %+v, want workspace.size's default", sp)
	}
	if l := disp.lendings(); len(l) != 2 || l[0].MaxConcurrent != 1 || l[1].MaxConcurrent != 1 || len(l[1].Queues) != 1 {
		t.Errorf("registered and reloaded lending %+v, want maxConcurrent 1", l)
	}
	if types := e.TranscriptTypes(); !slices.Equal(types, []string{"assistant"}) {
		t.Errorf("transcript types %q", types)
	}
	for _, sec := range []string{enrollSecret, refreshSecret, "agt_gus-cmd-test.secret", harnesstest.GitToken, "codex-login-access-0123"} {
		if strings.Contains(errOut.String(), sec) {
			t.Errorf("a secret is in the logs: %q", sec)
		}
	}
	if _, err := os.Stat(filepath.Join(s.state, "runs", "tsk_sup1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run directory is left: %v", err)
	}
}

// supervise hands the workspace runner the config's workspace.gatewayPort, default 3128: the
// gateway listens on the port the host's firewall opens.
func TestSuperviseGatewayPort(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		want  netip.AddrPort
	}{
		{"default", "", netip.MustParseAddrPort("127.0.0.1:3128")},
		{"configured", "workspace: {driver: tart, gatewayPort: 4000}\n", netip.MustParseAddrPort("127.0.0.1:4000")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := harnesstest.NewE2E(t, "tsk_port1", "Write the intro.\n")
			lease := e.Job(v1alpha1.HarnessResolved{Name: "codex", Model: "gpt-5-codex"}, []string{"pr-opened"}).Lease
			lease.Lease.Agent, lease.Lease.ExpiresAt, lease.Lease.HeartbeatEvery = "agt_gus-cmd-test", time.Now().Add(2*time.Minute), "30s"
			next, err := url.Parse(e.DispatcherURL())
			if err != nil {
				t.Fatal(err)
			}
			disp := &leaseDispatcher{leases: []apiclient.Lease{lease}, next: httputil.NewSingleHostReverseProxy(next)}
			ts := httptest.NewServer(http.HandlerFunc(disp.serve))
			t.Cleanup(ts.Close)

			s := newSupervisedTest(t, ts.URL)
			s.cfg = writeConfig(t, s.dir, ts.URL, tt.extra)
			var lmu sync.Mutex
			var gateways []netip.AddrPort
			s.deps.Listen = func(ctx context.Context, a netip.AddrPort) (net.Listener, error) {
				lmu.Lock()
				gateways = append(gateways, a)
				lmu.Unlock()
				var lc net.ListenConfig
				return lc.Listen(ctx, "tcp", "127.0.0.1:0")
			}
			sigs := make(chan os.Signal, 1)
			s.deps.Worker.Signals = sigs
			errOut := &syncBuffer{}
			code := make(chan int, 1)
			go func() { code <- runSupervise(context.Background(), s.args(), errOut, s.deps) }()
			waitUntil(t, "the completion", func() bool { return len(disp.completed()) == 1 })
			sigs <- syscall.SIGTERM
			select {
			case c := <-code:
				if c != 0 {
					t.Fatalf("supervise exited %d\n%s", c, errOut.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("supervise didn't drain\n%s", errOut.String())
			}
			if c := disp.completed()[0]; c.Result.Status != v1alpha1.ResultDone {
				t.Errorf("complete %+v", c)
			}
			lmu.Lock()
			defer lmu.Unlock()
			if want := []netip.AddrPort{tt.want}; !slices.Equal(gateways, want) {
				t.Errorf("gateways on %v, want %v", gateways, want)
			}
		})
	}
}

// The kata driver gets katabase.json's image and workspace.size's disk, and the warm disk only
// when host check's katawarm step is ok and katawarm.json exists; its workspaces are
// workspace.size.
func TestSuperviseKataDriver(t *testing.T) {
	ok, missing := host.Status{Step: host.KataWarmStep, State: host.StateOK}, host.Status{Step: host.KataWarmStep, State: host.StateMissing}
	tests := []struct {
		name   string
		status host.Status
		record bool
		warm   bool
	}{
		{"warm", ok, true, true},
		{"no record", ok, false, false},
		{"step not ok", missing, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newKataSupervisedTest(t, "http://127.0.0.1:1", "")
			ts.kataRecords(t)
			if !tt.record {
				if err := os.Remove(ts.deps.Host.KataWarmRecordPath()); err != nil {
					t.Fatal(err)
				}
			}
			e := ts.deps.Host
			e.Workspace = config.Workspace{Driver: config.DriverKata, Size: config.WorkspaceSize{CPUs: 8, Memory: "12Gi", Disk: "100Gi"}}.WithDefaults("linux")
			s := &supervisor{d: ts.deps, e: &e, stderr: io.Discard, log: discardLog()}
			_, spec, err := s.newDriver(context.Background(), []host.Status{tt.status})
			if err != nil {
				t.Fatal(err)
			}
			if c := ts.driver.kataCfg; c.Image != "factory-agent-base:dev" || c.DiskGiB != 100 || c.Warm != tt.warm {
				t.Errorf("kata config %+v, want warm %v", c, tt.warm)
			}
			if spec != (workspace.Spec{CPU: 8, MemoryMiB: 12 << 10}) || s.workspaceGiB != 12 || s.memTotal>>30 != 61 {
				t.Errorf("spec %+v, workspace %d GiB, memory %d GiB", spec, s.workspaceGiB, s.memTotal>>30)
			}
		})
	}
}

// tart's workspaces are workspace.size too, not the driver's defaults: host init built the base
// VM that size, and tart's Create sets the Spec on each clone.
func TestSuperviseTartDriver(t *testing.T) {
	tests := []struct {
		name string
		size config.WorkspaceSize
		want workspace.Spec
	}{
		{"default", config.WorkspaceSize{}, workspace.Spec{CPU: 6, MemoryMiB: 8 << 10}},
		{"sized", config.WorkspaceSize{CPUs: 4, Memory: "16Gi"}, workspace.Spec{CPU: 4, MemoryMiB: 16 << 10}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newSupervisedTest(t, "http://127.0.0.1:1")
			e := ts.deps.Host
			e.Workspace = config.Workspace{Driver: config.DriverTart, Size: tt.size}.WithDefaults("darwin")
			s := &supervisor{d: ts.deps, e: &e, warmBase: "my-warm", stderr: io.Discard, log: discardLog()}
			_, spec, err := s.newDriver(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if c := ts.driver.cfg; c.WarmBase != "my-warm" || ts.driver.kataCfg.Image != "" {
				t.Errorf("tart config %+v, kata config %+v", c, ts.driver.kataCfg)
			}
			if spec != tt.want {
				t.Errorf("spec %+v, want %+v", spec, tt.want)
			}
		})
	}
}

// The kata cap: lending.maxConcurrent, at most the 63 slots and the workspaces that fit in the
// physical memory less 4 GiB, and at least one.
func TestKataConcurrency(t *testing.T) {
	const gib = uint64(1) << 30
	tests := []struct {
		want   int
		mem    uint64
		wsGiB  int
		n      int
		capped string
	}{
		{3, 62 * gib, 8, 3, ""},                  // the workstation: 7 fit
		{10, 62 * gib, 8, 7, "memory: 62 GiB"},   // (62-4)/8
		{7, 60 * gib, 8, 7, ""},                  // exactly 7
		{8, 60*gib - 1, 8, 6, "fits 6"},          // a byte short of 7
		{100, 1024 * gib, 2, 63, "63 workspace"}, // the slots
		{100, 100 * gib, 2, 48, "memory"},        // memory below the slots
		{2, 6 * gib, 8, 1, "memory"},             // less than one fits: still one
		{2, 3 * gib, 8, 1, "memory"},             // less than the headroom
		{5, 62 * gib, 0, 5, ""},                  // no size: no memory cap
	}
	for _, tt := range tests {
		n, by := kataConcurrency(tt.want, tt.mem, tt.wsGiB)
		if n != tt.n || (tt.capped == "") != (by == "") || !strings.Contains(by, tt.capped) {
			t.Errorf("kataConcurrency(%d, %d GiB, %d GiB) = %d, %q; want %d, %q", tt.want, tt.mem/gib, tt.wsGiB, n, by, tt.n, tt.capped)
		}
	}
}

// Two leases at once through supervise --driver kata: each in its own workspace behind its own
// gateway (the real one, on loopback), both completed; the workspaces are sized from the config,
// the cleanup leaves host init's warm build alone, and lending is capped on SIGHUP by the memory
// it re-reads, keeping the last good value when the read fails.
func TestSuperviseKataTwoLeases(t *testing.T) {
	e := harnesstest.NewE2E(t, "tsk_kata1", "Write the intro.\n")
	var leases []apiclient.Lease
	for _, id := range []string{"tsk_kata1", "tsk_kata2"} {
		e.TaskID = id
		l := e.Job(v1alpha1.HarnessResolved{Name: "codex", Model: "gpt-5-codex"}, []string{"pr-opened"}).Lease
		run := "run_" + id
		l.Lease.Id, l.Lease.Run = "lse_"+id, &run
		l.Lease.Agent, l.Lease.ExpiresAt, l.Lease.HeartbeatEvery = "agt_gus-cmd-test", time.Now().Add(2*time.Minute), "30s"
		leases = append(leases, l)
	}
	next, err := url.Parse(e.DispatcherURL())
	if err != nil {
		t.Fatal(err)
	}
	disp := &leaseDispatcher{leases: leases, next: httputil.NewSingleHostReverseProxy(next)}
	ts := httptest.NewServer(http.HandlerFunc(disp.serve))
	t.Cleanup(ts.Close)

	s := newKataSupervisedTest(t, ts.URL, "  maxConcurrent: 2\nworkspace:\n  driver: kata\n  size: {cpus: 4, memory: 16Gi, disk: 80Gi}\n")
	s.kataRecords(t)
	s.driver.listed = []string{"ws-tsk_old", host.KataWarmBuild}
	s.driver.together = 2
	var lmu sync.Mutex
	var gateways []netip.AddrPort
	s.deps.Listen = func(ctx context.Context, a netip.AddrPort) (net.Listener, error) {
		lmu.Lock()
		gateways = append(gateways, a)
		lmu.Unlock()
		var lc net.ListenConfig
		return lc.Listen(ctx, "tcp", "127.0.0.1:0")
	}
	sigs := make(chan os.Signal, 2)
	s.deps.Worker.Signals = sigs
	errOut := &syncBuffer{}
	code := make(chan int, 1)
	go func() { code <- runSupervise(context.Background(), s.args(), errOut, s.deps) }()
	waitUntil(t, "both completions", func() bool { return len(disp.completed()) == 2 })
	// SIGHUP: the host is down to 40 GiB (from 61), and 40 less 4 fits 2 workspaces of 16 GiB.
	if err := os.WriteFile(s.cfg, bytes.Replace(mustRead(t, s.cfg), []byte("maxConcurrent: 2"), []byte("maxConcurrent: 5"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	meminfo := filepath.Join(s.deps.Host.Root, "proc", "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal:       41943040 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGHUP
	waitUntil(t, "the lending PATCH", func() bool { return len(disp.lendings()) == 2 })
	// SIGHUP with the memory unreadable: the cap keeps 40 GiB's.
	if err := os.Remove(meminfo); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGHUP
	waitUntil(t, "the second lending PATCH", func() bool { return len(disp.lendings()) == 3 })
	sigs <- syscall.SIGTERM
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("supervise exited %d\n%s", c, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("supervise didn't drain\n%s", errOut.String())
	}

	var tasks []string
	for _, c := range disp.completed() {
		if c.Result.Status != v1alpha1.ResultDone || c.TranscriptRef == nil {
			t.Errorf("complete %+v", c)
		}
		tasks = append(tasks, c.Result.Task)
	}
	if slices.Sort(tasks); !slices.Equal(tasks, []string{"tsk_kata1", "tsk_kata2"}) {
		t.Errorf("completed %v", tasks)
	}
	calls := s.driver.Calls()
	if calls[0] != "destroy ws-tsk_old" || slices.Contains(calls, "destroy "+host.KataWarmBuild) {
		t.Errorf("the cleanup's calls %v", calls)
	}
	for _, ws := range []string{"ws-tsk_kata1", "ws-tsk_kata2"} {
		var got []string
		for _, c := range calls {
			if op, name, _ := strings.Cut(c, " "); name == ws {
				got = append(got, op)
			}
		}
		if want := []string{"create", "start", "put", "exec", "get", "destroy"}; !slices.Equal(got, want) {
			t.Errorf("%s: calls %v, want %v", ws, got, want)
		}
	}
	if c := s.driver.kataCfg; c.Image != "factory-agent-base:dev" || c.DiskGiB != 80 || !c.Warm {
		t.Errorf("kata config %+v", c)
	}
	if sp := s.driver.spec; sp != (workspace.Spec{CPU: 4, MemoryMiB: 16 << 10}) {
		t.Errorf("workspace spec %+v", sp)
	}
	lmu.Lock()
	slices.SortFunc(gateways, func(a, b netip.AddrPort) int { return a.Compare(b) })
	if want := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:3128"), netip.MustParseAddrPort("127.0.0.2:3128")}; !slices.Equal(gateways, want) {
		t.Errorf("gateways on %v, want %v", gateways, want)
	}
	lmu.Unlock()
	if l := disp.lendings(); len(l) != 3 || l[0].MaxConcurrent != 2 || l[1].MaxConcurrent != 2 || l[2].MaxConcurrent != 2 {
		t.Errorf("registered and reloaded lending %+v, want maxConcurrent 2 each time (the start's 61 GiB would give 3)", l)
	}
	for _, want := range []string{"lending.maxConcurrent capped", "physical memory changed", "physical memory not re-read"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("no %q in the logs:\n%s", want, errOut.String())
		}
	}
}

// On SIGHUP the kata driver re-reads the physical memory and logs it, and the cap, when they
// change; a failed read keeps the last good value; tart doesn't read it.
func TestReloadMemory(t *testing.T) {
	const gib = uint64(1) << 30
	tests := []struct {
		name    string
		driver  string
		meminfo string // "" removes proc/meminfo
		want    uint64
		log     string
	}{
		{"changed", config.DriverKata, "MemTotal:       41943040 kB\n", 40 * gib, "memoryGiB=40 wasGiB=61 maxWorkspaces=2 wasMaxWorkspaces=3"},
		{"unchanged", config.DriverKata, "MemTotal:       64395636 kB\n", 64395636 << 10, ""},
		{"unreadable", config.DriverKata, "", 64395636 << 10, "physical memory not re-read; the lending cap keeps the last\" memoryGiB=61"},
		{"garbled", config.DriverKata, "MemTotal: lots\n", 64395636 << 10, "unexpected MemTotal line"},
		{"tart", config.DriverTart, "", 64395636 << 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := kataHostDeps(t).Env
			e.Workspace.Driver = tt.driver
			meminfo := filepath.Join(e.Root, "proc", "meminfo")
			var err error
			if tt.meminfo == "" {
				err = os.Remove(meminfo)
			} else {
				err = os.WriteFile(meminfo, []byte(tt.meminfo), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			s := &supervisor{e: &e, log: slog.New(slog.NewTextHandler(&buf, nil)), memTotal: 64395636 << 10, workspaceGiB: 16}
			s.reloadMemory()
			if s.memTotal != tt.want {
				t.Errorf("memory %d, want %d", s.memTotal, tt.want)
			}
			if got := buf.String(); (tt.log == "") != (got == "") || !strings.Contains(got, tt.log) {
				t.Errorf("log %q, want %q", got, tt.log)
			}
		})
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

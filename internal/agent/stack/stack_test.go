package stack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

var _ pipeline.Stack = (*ForPipeline)(nil)

// fakeRunner records commands instead of running them; fail makes the commands whose line
// contains it fail; block (when set) is waited on by make.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	fail  string
	block chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, c Command, out io.Writer) error {
	line := c.String()
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	_, _ = io.WriteString(out, "ran "+c.Name+"\n")
	if f.block != nil && c.Name == "make" && strings.HasPrefix(c.Args[0], "demo-up-") {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.fail != "" && strings.Contains(line, f.fail) {
		return errors.New("exit status 2")
	}
	return nil
}

func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func testState(project string) State {
	return State{
		Project: project, Profile: ProfileShowcase, LocalDev: "/home/owner/local-dev",
		Versions: StackVersions{API: "1.4.0", UI: "2.13.0", Agent: "0.9.2"},
	}
}

func newTestManager(t *testing.T, r Runner) *Manager {
	t.Helper()
	m, err := New(Options{StateDir: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpDown(t *testing.T) {
	r := &fakeRunner{}
	m := newTestManager(t, r)
	s := testState("factory-tsk-1")
	if err := m.Up(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got, err := m.Current()
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("Current = %+v, %v", got, err)
	}
	if st, err := os.Stat(m.Lock()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("lock %v %v", st, err)
	}
	// The same project again: brought up again with the new versions.
	s.Versions.API = "1.4.1"
	if err := m.Up(context.Background(), s); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got, _ := m.Current(); got.Versions.API != "1.4.1" {
		t.Errorf("re-up state %+v", got)
	}
	// Another project is refused.
	if err := m.Up(context.Background(), testState("factory-tsk-2")); !IsBusy(err) || !strings.Contains(err.Error(), "factory-tsk-1") {
		t.Fatalf("second stack: %v", err)
	}
	if err := m.Down(context.Background(), "factory-tsk-2"); err == nil {
		t.Error("Down of a project that isn't up passed")
	}
	if err := m.Down(context.Background(), "factory-tsk-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Current(); !errors.Is(err, ErrNoStack) {
		t.Errorf("after down: %v", err)
	}
	if err := m.Down(context.Background(), ""); err != nil {
		t.Errorf("down with nothing up: %v", err)
	}
	want := []string{
		"make demo-up-showcase 'COMPOSE_COMMAND=docker compose' COMPOSE_PROJECT_NAME=factory-tsk-1 CCF_API_IMAGE=ghcr.io/compliance-framework/api:1.4.0 CCF_UI_IMAGE=ghcr.io/compliance-framework/ui:2.13.0 CCF_AGENT_VERSION=0.9.2 KIND_HOST_ALIAS=factory-tsk-1-api-1",
		"make demo-up-showcase 'COMPOSE_COMMAND=docker compose' COMPOSE_PROJECT_NAME=factory-tsk-1 CCF_API_IMAGE=ghcr.io/compliance-framework/api:1.4.1 CCF_UI_IMAGE=ghcr.io/compliance-framework/ui:2.13.0 CCF_AGENT_VERSION=0.9.2 KIND_HOST_ALIAS=factory-tsk-1-api-1",
		"docker compose --project-name factory-tsk-1 down --volumes --remove-orphans --timeout 10",
		"make showcase-kind-destroy 'COMPOSE_COMMAND=docker compose'",
	}
	if got := r.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands\n got %q\nwant %q", got, want)
	}
}

func TestUpFailureTakesTheStackDown(t *testing.T) {
	r := &fakeRunner{fail: "demo-up-free"}
	m := newTestManager(t, r)
	s := testState("factory-tsk-1")
	s.Profile = ProfileFree
	if err := m.Up(context.Background(), s); err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("Up = %v", err)
	}
	if _, err := m.Current(); !errors.Is(err, ErrNoStack) {
		t.Errorf("the lock is kept after a failed up: %v", err)
	}
	got := r.lines()
	if len(got) != 2 || !strings.HasPrefix(got[1], "docker compose --project-name factory-tsk-1 down --volumes") {
		t.Errorf("commands %q", got)
	}
	if strings.Contains(got[0], "KIND_HOST_ALIAS") {
		t.Errorf("free profile sets KIND_HOST_ALIAS: %q", got[0])
	}
}

func TestDownFailureKeepsTheLock(t *testing.T) {
	r := &fakeRunner{}
	m := newTestManager(t, r)
	if err := m.Up(context.Background(), testState("factory-tsk-1")); err != nil {
		t.Fatal(err)
	}
	r.fail = "compose"
	if err := m.Down(context.Background(), ""); err == nil {
		t.Fatal("Down passed")
	}
	if _, err := m.Current(); err != nil {
		t.Errorf("the lock was released although down failed: %v", err)
	}
	r.fail = ""
	if err := m.Down(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestLockContention(t *testing.T) {
	// Several processes' worth of Ups for different tasks at once: exactly one gets the
	// stack, the others are refused while it is up, whatever the interleaving.
	r := &fakeRunner{block: make(chan struct{})}
	dir := t.TempDir()
	const n = 8
	var (
		wg      sync.WaitGroup
		ok      atomic.Int32
		busy    atomic.Int32
		started = make(chan struct{}, n)
		errs    = make(chan error, n)
	)
	for i := range n {
		// One Manager each, as separate processes would have.
		m, err := New(Options{StateDir: dir, Runner: r})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			err := m.Up(context.Background(), testState("factory-tsk-"+string(rune('a'+i))))
			switch {
			case err == nil:
				ok.Add(1)
			case IsBusy(err):
				busy.Add(1)
			default:
				errs <- err
			}
		}()
	}
	for range n {
		<-started
	}
	// Let every loser be refused before the winner's make returns.
	deadline := time.Now().Add(10 * time.Second)
	for busy.Load() < n-1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(r.block)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if ok.Load() != 1 || busy.Load() != n-1 {
		t.Fatalf("%d up, %d refused", ok.Load(), busy.Load())
	}
	ups := 0
	for _, l := range r.lines() {
		if strings.HasPrefix(l, "make demo-up-") {
			ups++
		}
	}
	if ups != 1 {
		t.Errorf("make ran %d times", ups)
	}
}

func TestUnreadableLockIsBusy(t *testing.T) {
	m := newTestManager(t, &fakeRunner{})
	if err := os.WriteFile(m.Lock(), []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(context.Background(), testState("factory-tsk-1")); !IsBusy(err) {
		t.Fatalf("Up = %v", err)
	}
}

func TestPlanRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*State){
		"project":    func(s *State) { s.Project = "Factory_X" },
		"profile":    func(s *State) { s.Profile = "all" },
		"local-dev":  func(s *State) { s.LocalDev = "local-dev" },
		"unresolved": func(s *State) { s.Versions.UI = "" },
		"version":    func(s *State) { s.Versions.Agent = "$(shell id)" },
	} {
		s := testState("factory-tsk-1")
		mutate(&s)
		if _, err := NewPlan(s); err == nil {
			t.Errorf("%s: NewPlan passed", name)
		}
	}
}

func TestProjectName(t *testing.T) {
	for in, want := range map[string]string{"tsk_01JA.x": "factory-tsk-01ja-x", "abc-1": "factory-abc-1"} {
		if got := ProjectName(in); got != want || !projectRE.MatchString(got) {
			t.Errorf("ProjectName(%q) = %q", in, got)
		}
	}
}

// stackServer is the stack's API and UI: not ready until ready is set.
func stackServer(t *testing.T, ready *atomic.Bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case apiHealthPath, "/":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWait(t *testing.T) {
	var ready atomic.Bool
	srv := stackServer(t, &ready)
	clk := clock.NewFake(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	m, err := New(Options{StateDir: t.TempDir(), Runner: &fakeRunner{}, Clock: clk, UIURL: srv.URL, APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Wait(context.Background(), time.Minute) }()
	clk.BlockUntil(2) // the deadline and the first poll interval
	ready.Store(true)
	clk.Advance(DefaultPollInterval)
	if err := <-done; err != nil {
		t.Fatalf("Wait = %v", err)
	}

	ready.Store(false)
	go func() { done <- m.Wait(context.Background(), time.Minute) }()
	clk.BlockUntil(2)
	clk.Advance(time.Minute)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Wait = %v", err)
	}
}

func TestForPipeline(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	srv := stackServer(t, &ready)
	r := &fakeRunner{}
	m, err := New(Options{StateDir: t.TempDir(), Runner: r, UIURL: srv.URL, APIURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	p := &ForPipeline{Manager: m, LocalDev: "/home/owner/local-dev", GitHub: newFakeGitHub(), Clock: clk}
	task := v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{
		Inputs: map[string]any{"verifyAgainst": map[string]any{"train": "compliance-framework/workflows#123"}},
	}}
	env, err := p.Up(context.Background(), harnessWorkspace(), task)
	if err != nil {
		t.Fatal(err)
	}
	if env[EnvURL] != srv.URL || env[EnvProject] != "factory-tsk-1" || env[EnvDir] != m.StateDir() ||
		env[EnvVersions] != `{"api":"1.4.0","ui":"2.13.0","agent":"0.9.2","plugins":{"plugin-github-repositories":"0.9.0"}}` {
		t.Errorf("env %v", env)
	}
	s, err := m.Current()
	if err != nil || s.Task != "tsk_1" || s.Source != "train compliance-framework/workflows#123" || !s.StartedAt.Equal(clk.Now()) {
		t.Errorf("state %+v %v", s, err)
	}
	if err := p.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Down(context.Background()); err != nil {
		t.Errorf("second Down: %v", err)
	}
	if _, err := m.Current(); !errors.Is(err, ErrNoStack) {
		t.Errorf("after Down: %v", err)
	}

	// A stack that never gets ready is taken down and the lock released.
	ready.Store(false)
	p.WaitTimeout = time.Millisecond
	if _, err := p.Up(context.Background(), harnessWorkspace(), task); err == nil {
		t.Fatal("Up passed with a stack that isn't ready")
	}
	if _, err := m.Current(); !errors.Is(err, ErrNoStack) {
		t.Errorf("after a failed wait: %v", err)
	}
	// No versions in the inputs.
	task.Spec.Inputs = nil
	if _, err := p.Up(context.Background(), harnessWorkspace(), task); err == nil {
		t.Error("Up passed without versions")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"PATH=/bin", "COMPOSE_PROJECT_NAME=ccf", "HOME=/h"}, []string{"COMPOSE_PROJECT_NAME=factory-x"})
	want := []string{"PATH=/bin", "HOME=/h", "COMPOSE_PROJECT_NAME=factory-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %q", got)
	}
}

func TestExecRunner(t *testing.T) {
	// A harmless program stands in for make: the environment and directory reach it.
	sh, err := execLookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	var out strings.Builder
	r := ExecRunner{Environ: func() []string { return []string{"PATH=" + filepath.Dir(sh), "X=old"} }}
	if err := r.Run(context.Background(), Command{Dir: dir, Name: sh, Args: []string{"-c", "echo $X; pwd"}, Env: []string{"X=new"}}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || lines[0] != "new" {
		t.Errorf("output %q", out.String())
	}
	if err := r.Run(context.Background(), Command{Dir: dir, Name: sh, Args: []string{"-c", "exit 3"}}, io.Discard); err == nil {
		t.Error("a failing command passed")
	}
}

func harnessWorkspace() harness.Workspace { return harness.Workspace{} }

var execLookPath = exec.LookPath

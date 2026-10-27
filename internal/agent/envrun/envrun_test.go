package envrun

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
)

const (
	taskID = "tsk_env1"
	// ghToken is shaped like a GitHub token: the log and the errors never hold it.
	ghToken = "ghs_envrunShapedTokenNeverLogged0123456789"
)

var t0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// rig is one Runner on a temp workspace and state dir, recording its steps.
type rig struct {
	t     *testing.T
	ws    harness.Workspace
	state string
	kind  string
	clk   *clock.Fake
	r     *Runner

	mu    sync.Mutex
	steps []string
	errs  map[string]error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, state: t.TempDir(), kind: t.TempDir(), clk: clock.NewFake(t0), errs: map[string]error{},
		ws: harness.Workspace{Root: root, Out: out, Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root}}}
	r, err := New(Options{StateDir: g.state, KindImages: g.kind, Clock: g.clk, OnStep: g.onStep})
	if err != nil {
		t.Fatal(err)
	}
	g.r = r
	return g
}

func (g *rig) onStep(name string, _ time.Duration, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := "ok"
	if err != nil {
		state = "error"
		g.errs[name] = err
	}
	g.steps = append(g.steps, name+":"+state)
}

func (g *rig) stepList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.steps)
}

func (g *rig) log() string {
	b, err := os.ReadFile(filepath.Join(g.ws.Out, runbundle.EnvironmentLog))
	if err != nil {
		g.t.Fatalf("the environment log: %v", err)
	}
	return string(b)
}

func (g *rig) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(g.ws.Root, name))
	return string(b)
}

func task(e *v1alpha1.Environment, versions map[string]string) v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: taskID}, Spec: v1alpha1.TaskRunSpec{Environment: e, Versions: versions}}
}

// server answers every request with code (a redirect to /elsewhere for a 3xx).
func server(t *testing.T, code int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code >= 300 && code < 400 {
			http.Redirect(w, r, "/elsewhere", code)
			return
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

// The whole sequence: setup and up run in the workspace root under bash -eo pipefail with the
// task's id, versions and kind images; ready accepts a 3xx; Up returns the exposes and versions;
// Down runs down; every step is reported; the output is in the log.
func TestUpDown(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	if err := os.WriteFile(filepath.Join(g.kind, "api.tar"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := server(t, http.StatusFound)
	e := &v1alpha1.Environment{
		Setup: "echo setup ran > setup.out\n",
		Up: `echo "$FACTORY_TASK_ID $FACTORY_VERSION_API_SERVER $FACTORY_VERSION_UI $FACTORY_KIND_IMAGES" > up.out
echo "up says hi"
echo "on stderr" >&2
`,
		Down:    "echo down ran > down.out\n",
		Ready:   &v1alpha1.ReadyCheck{HTTP: []string{ready.URL + "/health"}},
		Exposes: map[string]string{"FACTORY_STACK_URL": "http://localhost:8000"},
	}
	vars, err := g.r.Up(context.Background(), g.ws, task(e, map[string]string{"api-server": "v1.2.3", "ui": "v0.4.0"}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"FACTORY_STACK_URL": "http://localhost:8000", "FACTORY_VERSION_API_SERVER": "v1.2.3", "FACTORY_VERSION_UI": "v0.4.0"}
	if !maps.Equal(vars, want) {
		t.Errorf("Up returned %v, want %v", vars, want)
	}
	if got := g.file("setup.out"); got != "setup ran\n" {
		t.Errorf("setup.out %q", got)
	}
	if got, want := g.file("up.out"), taskID+" v1.2.3 v0.4.0 "+g.kind+"\n"; got != want {
		t.Errorf("up.out %q, want %q", got, want)
	}
	for _, name := range []string{"setup", "up", "down"} {
		st, err := os.Stat(filepath.Join(g.state, "env", name+".sh"))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("the %s script: %v %v", name, st, err)
		}
	}
	if err := g.r.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.file("down.out"); got != "down ran\n" {
		t.Errorf("down.out %q", got)
	}
	if err := g.r.Down(context.Background()); err != nil || g.file("down.out") != "down ran\n" {
		t.Errorf("a second Down: %v", err)
	}
	if got, want := g.stepList(), []string{"environment.setup:ok", "environment.up:ok", "environment.ready:ok", "environment.down:ok"}; !slices.Equal(got, want) {
		t.Errorf("steps %v, want %v", got, want)
	}
	log := g.log()
	for _, s := range []string{"# environment.setup\n", "# environment.up\n", "up says hi\n", "on stderr\n", "# environment.down\n"} {
		if !strings.Contains(log, s) {
			t.Errorf("the log has no %q:\n%s", s, log)
		}
	}
}

// Each failure of Up: the step and its error, and what ran or didn't.
func TestUpFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		env    *v1alpha1.Environment
		steps  []string
		errHas []string
		check  func(t *testing.T, g *rig, err error)
	}{
		{name: "-e stops at the failing command", env: &v1alpha1.Environment{Up: "echo before\nfalse\necho after > after.out\n"},
			steps: []string{"environment.up:error"}, errHas: []string{"up: exit status 1", "its output ends:\n", "before"},
			check: func(t *testing.T, g *rig, _ error) {
				if g.file("after.out") != "" {
					t.Error("the script went on after a failing command")
				}
			}},
		{name: "pipefail fails a pipeline whose first stage fails", env: &v1alpha1.Environment{Up: "false | cat\necho after > after.out\n"},
			steps: []string{"environment.up:error"}, errHas: []string{"up: exit status 1"}},
		{name: "a failing setup skips up", env: &v1alpha1.Environment{Setup: "exit 3\n", Up: "echo up > up.out\n"},
			steps: []string{"environment.setup:error"}, errHas: []string{"setup: exit status 3"},
			check: func(t *testing.T, g *rig, _ error) {
				if g.file("up.out") != "" {
					t.Error("up ran after a failed setup")
				}
			}},
		{name: "the output is redacted", env: &v1alpha1.Environment{Up: "echo \"cloning with " + ghToken + "\"\nexit 1\n"},
			steps: []string{"environment.up:error"}, errHas: []string{"cloning with " + pipeline.Redacted},
			check: func(t *testing.T, g *rig, err error) {
				if log := g.log(); strings.Contains(log, ghToken) || !strings.Contains(log, pipeline.Redacted) {
					t.Errorf("the log isn't redacted:\n%s", log)
				}
			}},
		{name: "the error carries the last 8 KiB", env: &v1alpha1.Environment{Up: "echo first-line\nfor i in $(seq 1 2000); do echo \"line $i of the output\"; done\necho last-line\nexit 1\n"},
			steps: []string{"environment.up:error"}, errHas: []string{"last-line"},
			check: func(t *testing.T, g *rig, err error) {
				_, tail, _ := strings.Cut(err.Error(), "its output ends:\n")
				if len(tail) > MaxTail || len(tail) < MaxTail-64 || strings.Contains(tail, "first-line") {
					t.Errorf("the tail is %d bytes (first-line in it: %v)", len(tail), strings.Contains(tail, "first-line"))
				}
				if !strings.Contains(g.log(), "first-line") {
					t.Error("the log lost the first line")
				}
			}},
		{name: "an exposes name the runtime sets", env: &v1alpha1.Environment{Up: "echo up > up.out\n", Exposes: map[string]string{"FACTORY_TOKEN": "x"}},
			errHas: []string{"the task:", "FACTORY_TOKEN"},
			check: func(t *testing.T, g *rig, _ error) {
				if g.file("up.out") != "" {
					t.Error("up ran for an invalid task")
				}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRig(t)
			vars, err := g.r.Up(context.Background(), g.ws, task(tt.env, nil))
			if err == nil || vars != nil {
				t.Fatalf("Up: %v %v", vars, err)
			}
			for _, s := range tt.errHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("the error %q has no %q", err, s)
				}
			}
			if strings.Contains(err.Error(), ghToken) {
				t.Errorf("the error holds the token: %q", err)
			}
			if got := g.stepList(); !slices.Equal(got, tt.steps) {
				t.Errorf("steps %v, want %v", got, tt.steps)
			}
			if tt.check != nil {
				tt.check(t, g, err)
			}
		})
	}
}

// down runs after a failed up (which may have started containers), and its failure is its
// step's error.
func TestDownAfterAFailedUp(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	e := &v1alpha1.Environment{Up: "exit 1\n", Down: "echo down ran > down.out\nexit 2\n"}
	if _, err := g.r.Up(context.Background(), g.ws, task(e, nil)); err == nil {
		t.Fatal("Up succeeded")
	}
	err := g.r.Down(context.Background())
	if err == nil || !strings.Contains(err.Error(), "down: exit status 2") || g.file("down.out") != "down ran\n" {
		t.Errorf("Down: %v, down.out %q", err, g.file("down.out"))
	}
	if got, want := g.stepList(), []string{"environment.up:error", "environment.down:error"}; !slices.Equal(got, want) {
		t.Errorf("steps %v, want %v", got, want)
	}
}

// Down is bounded by DownTimeout.
func TestDownTimeout(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	g.r.o.DownTimeout = 200 * time.Millisecond
	if _, err := g.r.Up(context.Background(), g.ws, task(&v1alpha1.Environment{Down: "sleep 30\n"}, nil)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := g.r.Down(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Errorf("Down: %v after %s", err, time.Since(start))
	}
}

// ready fails when a URL doesn't answer 2xx or 3xx within the timeout, on the injected clock.
func TestReadyTimeout(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	ok, down := server(t, http.StatusNoContent), server(t, http.StatusServiceUnavailable)
	e := &v1alpha1.Environment{Up: "true\n", Ready: &v1alpha1.ReadyCheck{HTTP: []string{ok.URL, down.URL + "/health"}, Timeout: "5m"}}
	done := make(chan error, 1)
	go func() {
		_, err := g.r.Up(context.Background(), g.ws, task(e, nil))
		done <- err
	}()
	g.clk.BlockUntil(2) // the deadline and the first tick
	g.clk.Advance(5 * time.Minute)
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ready didn't time out")
	}
	if err == nil || !strings.Contains(err.Error(), down.URL+"/health isn't ready after 5m0s (last: 503 Service Unavailable)") {
		t.Errorf("Up: %v", err)
	}
	if got, want := g.stepList(), []string{"environment.up:ok", "environment.ready:error"}; !slices.Equal(got, want) {
		t.Errorf("steps %v, want %v", got, want)
	}
}

// The ready checks never go through a proxy: they are the VM's own services.
func TestReadyNoProxy(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	tr, ok := g.r.client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("the ready client's transport %T uses a proxy", g.r.client.Transport)
	}
}

// Cancelling Up kills the script's whole process group.
func TestCancelKillsTheGroup(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	pidFile := filepath.Join(g.ws.Root, "child.pid")
	e := &v1alpha1.Environment{Up: "sleep 60 &\necho $! > child.pid\nwait\n"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.r.Up(ctx, g.ws, task(e, nil))
		done <- err
	}()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if pid == 0 {
		t.Fatal("the script didn't start its child")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Up: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Up didn't return after the cancel")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the script's child %d outlived the cancel", pid)
		}
	}
}

// A command up leaves running (a server started with &) keeps running and writing to the log
// after up returns: the log's pipe stays open for it.
func TestUpLeavesABackgroundCommand(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	g.r.grace = 50 * time.Millisecond
	e := &v1alpha1.Environment{Up: "(sleep 1; echo late output; echo alive > bg.out) &\necho up done\n"}
	start := time.Now()
	if _, err := g.r.Up(context.Background(), g.ws, task(e, nil)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Errorf("Up waited %s for its background command", d)
	}
	for deadline := time.Now().Add(20 * time.Second); g.file("bg.out") == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if g.file("bg.out") != "alive\n" {
		t.Fatal("the background command died")
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(g.log(), "late output\n") && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if log := g.log(); !strings.Contains(log, "up done\n") || !strings.Contains(log, "late output\n") {
		t.Errorf("the log:\n%s", log)
	}
	if err := g.r.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The log is capped (MaxLog, lowered here), ended by one [truncated] line.
func TestLogCap(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	g.r.maxLog = 64 << 10
	e := &v1alpha1.Environment{Up: "for i in $(seq 1 10000); do echo 'a line of output'; done\n"}
	if _, err := g.r.Up(context.Background(), g.ws, task(e, nil)); err != nil {
		t.Fatal(err)
	}
	if err := g.r.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := g.log()
	if len(log) > int(g.r.maxLog)+len("[truncated]\n") || len(log) < int(g.r.maxLog)-64 || !strings.HasSuffix(log, "a line of output\n[truncated]\n") {
		t.Errorf("the log is %d bytes, ending %q", len(log), log[max(0, len(log)-40):])
	}
}

// FACTORY_VERSION_<COMPONENT>: upper case, "-" as "_".
func TestVersionVar(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"ui": "FACTORY_VERSION_UI", "api-server": "FACTORY_VERSION_API_SERVER", "plugin-2": "FACTORY_VERSION_PLUGIN_2"} {
		if got := VersionVar(in); got != want {
			t.Errorf("VersionVar(%q) = %q, want %q", in, got, want)
		}
	}
}

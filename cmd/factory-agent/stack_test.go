package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/stack"
	"github.com/compliance-framework/factory/internal/clock"
)

var updateGolden = flag.Bool("update", false, "rewrite the goldens under testdata/")

// recordRunner records the commands instead of running them: no make, no docker.
type recordRunner struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordRunner) Run(_ context.Context, c stack.Command, _ io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, c.String())
	return nil
}

// trainGitHub serves the sample train's tracking issue and the latest releases.
func trainGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "stack-train-123.md"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch r.URL.Path {
		case "/repos/compliance-framework/workflows/issues/123":
			v = map[string]any{"number": 123, "body": string(body)}
		case "/repos/compliance-framework/api/releases/latest":
			v = map[string]string{"tag_name": "v1.3.2"}
		case "/repos/compliance-framework/ui/releases/latest":
			v = map[string]string{"tag_name": "v2.13.0"}
		case "/repos/compliance-framework/agent/releases/latest":
			v = map[string]string{"tag_name": "v0.9.1"}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stackTestDeps(t *testing.T, r stack.Runner) stackDeps {
	return stackDeps{
		Runner: r, GitHub: stack.GitHub{BaseURL: trainGitHub(t).URL},
		Clock:  clock.NewFake(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)),
		Getenv: func(string) string { return "" },
	}
}

// TestStackUpDryRun is local-stack.md's "Done when": --dry-run prints the exact make
// invocation and compose environment for a sample train (golden).
func TestStackUpDryRun(t *testing.T) {
	r := &recordRunner{}
	var out, errOut bytes.Buffer
	args := []string{"up", "--dry-run", "--train", "compliance-framework/workflows#123", "--task", "tsk_01JATRAIN",
		"--local-dev", "/home/owner/local-dev", "--state-dir", t.TempDir()}
	if code := runStack(context.Background(), args, &out, &errOut, stackTestDeps(t, r)); code != 0 {
		t.Fatalf("stack up --dry-run = %d\n%s", code, errOut.String())
	}
	golden := filepath.Join("testdata", "stack-up-dry-run.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("dry run\n got:\n%s\nwant:\n%s", out.String(), want)
	}
	if len(r.lines) != 0 {
		t.Errorf("a dry run ran %q", r.lines)
	}
}

func TestStackLifecycle(t *testing.T) {
	r := &recordRunner{}
	ready := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ready.Close()
	state := t.TempDir()
	d := stackTestDeps(t, r) // outside a run: the owner's shell
	ctx := context.Background()
	versions := filepath.Join(t.TempDir(), "versions.json")
	if err := os.WriteFile(versions, []byte(`{"api":"v0.22.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runStack(ctx, []string{"up", "--versions", versions, "--task", "tsk_2", "--profile", "free", "--local-dev", "/srv/local-dev", "--state-dir", state}, &out, &errOut, d)
	if code != 0 {
		t.Fatalf("up = %d\n%s", code, errOut.String())
	}
	for _, want := range []string{"FACTORY_STACK_PROJECT=factory-tsk-2\n", `FACTORY_STACK_VERSIONS='{"api":"0.22.0","ui":"2.13.0","agent":"0.9.1"}'`, "FACTORY_STACK_DIR=" + state} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("up printed\n%s\nlacking %q", out.String(), want)
		}
	}
	// Another task's stack is refused with exit 3.
	errOut.Reset()
	if code := runStack(ctx, []string{"up", "--task", "tsk_3", "--local-dev", "/srv/local-dev", "--state-dir", state}, io.Discard, &errOut, d); code != exitStackBusy ||
		!strings.Contains(errOut.String(), "factory-tsk-2") {
		t.Errorf("second up = %d %q", code, errOut.String())
	}
	// Inside the run (the pipeline's environment): env and wait find the worker's stack
	// through $FACTORY_STACK_DIR; up and down are refused.
	inRunEnv := map[string]string{stack.EnvDir: state, "FACTORY_TASK_ID": "tsk_2", "FACTORY_HEADLESS": "1"}
	d2 := d
	d2.Getenv = func(k string) string { return inRunEnv[k] }
	out.Reset()
	if code := runStack(ctx, []string{"env"}, &out, io.Discard, d2); code != 0 || !strings.Contains(out.String(), "FACTORY_STACK_URL=http://localhost:8000\n") {
		t.Errorf("env in the run = %d %q", code, out.String())
	}
	d2.HTTPClient = &http.Client{Transport: redirectTo(ready.URL)}
	if code := runStack(ctx, []string{"wait", "--timeout", "1m"}, io.Discard, io.Discard, d2); code != 0 {
		t.Errorf("wait in the run = %d", code)
	}
	for _, env := range []map[string]string{inRunEnv, {"FACTORY_HEADLESS": "1"}, {"FACTORY_TASK_ID": "tsk_2"}} {
		d3 := d
		d3.Getenv = func(k string) string { return env[k] }
		for _, args := range [][]string{
			{"up", "--task", "tsk_2", "--local-dev", "/srv/local-dev", "--state-dir", state},
			{"up", "--dry-run", "--local-dev", "/srv/local-dev", "--state-dir", state},
			{"down", "--state-dir", state},
		} {
			errOut.Reset()
			if code := runStack(ctx, args, io.Discard, &errOut, d3); code != 2 || !strings.Contains(errOut.String(), "the worker manages the run's stack") {
				t.Errorf("%v in a run (%v) = %d %q", args, env, code, errOut.String())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(state, stack.LockName)); err != nil {
		t.Errorf("a refused down touched the lock: %v", err)
	}
	// Back in the owner's shell, down works.
	d.Getenv = func(k string) string {
		if k == stack.EnvDir {
			return state
		}
		return ""
	}
	if code := runStack(ctx, []string{"down"}, io.Discard, io.Discard, d); code != 0 {
		t.Errorf("down = %d", code)
	}
	errOut.Reset()
	if code := runStack(ctx, []string{"env"}, io.Discard, &errOut, d2); code != 1 || !strings.Contains(errOut.String(), "no stack is up") {
		t.Errorf("env after down = %d %q", code, errOut.String())
	}
	if code := runStack(ctx, []string{"down"}, io.Discard, io.Discard, d); code != 0 {
		t.Errorf("down with nothing up = %d", code)
	}
	want := []string{
		"make demo-up-free 'COMPOSE_COMMAND=docker compose' COMPOSE_PROJECT_NAME=factory-tsk-2 CCF_API_IMAGE=ghcr.io/compliance-framework/api:0.22.0 CCF_UI_IMAGE=ghcr.io/compliance-framework/ui:2.13.0 CCF_AGENT_VERSION=0.9.1",
		"docker compose --project-name factory-tsk-2 down --volumes --remove-orphans --timeout 10",
	}
	if strings.Join(r.lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands\n got %q\nwant %q", r.lines, want)
	}
}

func TestStackUsage(t *testing.T) {
	d := stackTestDeps(t, &recordRunner{})
	for _, tt := range []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"nope"}, 2},
		{[]string{"help"}, 0},
		{[]string{"up", "--train", "a/b#1", "--release", "api@v1", "--local-dev", "/x", "--state-dir", t.TempDir()}, 2},
		{[]string{"up", "extra"}, 2},
		{[]string{"wait", "--state-dir", t.TempDir()}, 1}, // no stack up
		{[]string{"up", "--dry-run", "--profile", "all", "--local-dev", "/x", "--state-dir", t.TempDir()}, 1},
	} {
		if code := runStack(context.Background(), tt.args, io.Discard, io.Discard, d); code != tt.code {
			t.Errorf("stack %v = %d, want %d", tt.args, code, tt.code)
		}
	}
}

// redirectTo sends every request to base (the stack's fixed localhost URLs in tests).
type redirectTo string

func (b redirectTo) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(string(b), "http://")
	return http.DefaultTransport.RoundTrip(r2)
}

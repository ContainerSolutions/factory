package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/stack"
)

// parseEnvLines reads `stack env`'s KEY=VALUE lines back through a POSIX shell (the way
// skills use them: eval "$(factory-agent stack env)").
func parseEnvLines(t *testing.T, lines string) map[string]string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `set -a; eval "$1"; exec env`, "sh", lines)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("eval %q: %v", lines, err)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && strings.HasPrefix(k, "FACTORY_STACK_") {
			out[k] = v
		}
	}
	return out
}

// TestAgentShimEnv: inside a run, `factory-agent stack env` prints the run's stack (the
// variables the pipeline put in the harness's environment) exactly as the owner's `stack
// env` prints the same stack, and nothing, with exit 0, when the run has none.
func TestAgentShimEnv(t *testing.T) {
	ctx := context.Background()
	// The owner's `stack up` (recorded, no make): what it prints is a stack's environment.
	state := t.TempDir()
	var upOut, owner bytes.Buffer
	if code := runStack(ctx, []string{"up", "--task", "tsk_2", "--profile", "free", "--local-dev", "/srv/local-dev", "--state-dir", state},
		&upOut, io.Discard, stackTestDeps(t, &recordRunner{})); code != 0 {
		t.Fatalf("up = %d", code)
	}
	ownerGetenv := func(k string) string {
		if k == stack.EnvDir {
			return state
		}
		return ""
	}
	d := stackTestDeps(t, &recordRunner{})
	d.Getenv = ownerGetenv
	if code := runStack(ctx, []string{"env"}, &owner, io.Discard, d); code != 0 {
		t.Fatalf("owner's env = %d", code)
	}
	runEnv := parseEnvLines(t, owner.String())
	if len(runEnv) != 5 || runEnv[stack.EnvVersions] != `{"api":"1.3.2","ui":"2.13.0","agent":"0.9.1"}` {
		t.Fatalf("the owner's env %q parses to %v", owner.String(), runEnv)
	}
	runEnv[pipeline.EnvHeadless], runEnv[pipeline.EnvTaskID] = "1", "tsk_2"
	shim := agentShimDeps{Getenv: func(k string) string { return runEnv[k] }}

	var out, errOut bytes.Buffer
	if code := runAgentShim(ctx, []string{"stack", "env"}, &out, &errOut, shim); code != 0 || out.String() != owner.String() {
		t.Errorf("env in the run = %d\n%s\nwant the owner's\n%s\n%s", code, out.String(), owner.String(), errOut.String())
	}
	// Through `factory-agent fx`, which the workspace's bin/factory-agent runs.
	out.Reset()
	if code := runFx(ctx, []string{pipeline.AgentShimOp, "stack", "env"}, strings.NewReader(""), &out, io.Discard, shim.Getenv); code != 0 || out.String() != owner.String() {
		t.Errorf("fx agent-shim stack env = %d %q", code, out.String())
	}

	// No stack in the run: nothing on stdout, exit 0 (the lock, a manual stack's here, is
	// not the run's).
	none := map[string]string{pipeline.EnvHeadless: "1", stack.EnvDir: state}
	for _, env := range []map[string]string{none, {stack.EnvURL: "http://localhost:8000"}} {
		out.Reset()
		errOut.Reset()
		code := runAgentShim(ctx, []string{"stack", "env"}, &out, &errOut, agentShimDeps{Getenv: func(k string) string { return env[k] }})
		if code != 0 || out.Len() != 0 || !strings.Contains(errOut.String(), "this run has no stack") {
			t.Errorf("env without a stack (%v) = %d %q %q", env, code, out.String(), errOut.String())
		}
	}
}

// TestAgentShimWait: `factory-agent stack wait` polls the run's stack's API health and UI.
func TestAgentShimWait(t *testing.T) {
	ctx := context.Background()
	var ready atomic.Bool
	var paths seenPaths
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.add(r.URL.Path)
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{pipeline.EnvHeadless: "1", stack.EnvURL: srv.URL, stack.EnvAPI: srv.URL + "/"}
	d := agentShimDeps{Getenv: func(k string) string { return env[k] }}

	var errOut bytes.Buffer
	if code := runAgentShim(ctx, []string{"stack", "wait", "--timeout", "50ms"}, io.Discard, &errOut, d); code != 1 ||
		!strings.Contains(errOut.String(), "wasn't ready within 50ms") || !strings.Contains(errOut.String(), "503") {
		t.Errorf("wait on a stack never ready = %d %q", code, errOut.String())
	}
	ready.Store(true)
	errOut.Reset()
	if code := runAgentShim(ctx, []string{"stack", "wait"}, io.Discard, &errOut, d); code != 0 || errOut.String() != "the stack is ready\n" {
		t.Errorf("wait on a ready stack = %d %q", code, errOut.String())
	}
	if got := paths.get(); !strings.Contains(got, "/api/health") || !strings.Contains(got, " / ") {
		t.Errorf("wait polled %q, want the API's health and the UI", got)
	}
	errOut.Reset()
	none := func(k string) string {
		if k == pipeline.EnvHeadless {
			return "1"
		}
		return ""
	}
	if code := runAgentShim(ctx, []string{"stack", "wait"}, io.Discard, &errOut, agentShimDeps{Getenv: none}); code != 1 ||
		!strings.Contains(errOut.String(), "no stack is up") {
		t.Errorf("wait without a stack = %d %q", code, errOut.String())
	}
}

// TestAgentShimRefuses: the run's factory-agent has stack env and wait only.
func TestAgentShimRefuses(t *testing.T) {
	d := agentShimDeps{Getenv: func(string) string { return "" }}
	for _, tt := range []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "usage: factory-agent stack env|wait"},
		{[]string{"help"}, 0, ""},
		{[]string{"stack"}, 2, "usage:"},
		{[]string{"stack", "--help"}, 0, ""},
		{[]string{"stack", "wait", "-h"}, 0, ""},
		{[]string{"stack", "up", "--train", "a/b#1"}, 2, "the worker manages the run's stack"},
		{[]string{"stack", "down"}, 2, "the worker manages the run's stack"},
		{[]string{"stack", "nope"}, 2, "refused inside a run"},
		{[]string{"stack", "env", "extra"}, 2, "unexpected arguments"},
		{[]string{"stack", "env", "--state-dir", "/x"}, 2, "not defined"},
		{[]string{"stack", "wait", "--timeout", "soon"}, 2, "invalid value"},
		{[]string{"worker"}, 2, "not available inside a run"},
		{[]string{"fx", "pr", "open"}, 2, "not available inside a run"},
		{[]string{"mcp"}, 2, "not available inside a run"},
		{[]string{"version"}, 2, "not available inside a run"},
	} {
		var out, errOut bytes.Buffer
		code := runAgentShim(context.Background(), tt.args, &out, &errOut, d)
		if code != tt.code || !strings.Contains(errOut.String(), tt.msg) {
			t.Errorf("factory-agent %v = %d %q, want %d %q", tt.args, code, errOut.String(), tt.code, tt.msg)
		}
		if tt.code != 0 && out.Len() != 0 {
			t.Errorf("factory-agent %v printed %q on stdout", tt.args, out.String())
		}
	}
}

// seenPaths records the paths a test server saw.
type seenPaths struct {
	mu    sync.Mutex
	paths []string
}

func (p *seenPaths) add(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, s)
}

// get is the paths seen, space-separated and space-padded.
func (p *seenPaths) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return " " + strings.Join(p.paths, " ") + " "
}

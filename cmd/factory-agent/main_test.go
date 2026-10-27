package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/probe"
)

const (
	enrollSecret  = "enr_enroll-secret-cmd-0123456789"
	refreshSecret = "arf_refresh-secret-cmd-0123456789"
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
		!strings.Contains(out.String(), "supervise") || !strings.Contains(out.String(), "probe") {
		t.Errorf("help = %d %q", code, out.String())
	}
	var errOut bytes.Buffer
	if code := run(context.Background(), []string{"nope"}, io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), `unknown command "nope"`) {
		t.Errorf("unknown = %d %q", code, errOut.String())
	}
}

// writeConfig writes an agent config (0600) and its enrollment token (0600) under dir.
func writeConfig(t *testing.T, dir, dispatcher string, extra string) string {
	t.Helper()
	tok := filepath.Join(dir, "enrollment-token")
	if err := os.WriteFile(tok, []byte(enrollSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "agent.yaml")
	cfg := "dispatcher: " + dispatcher + "\npool: local\nenrollmentTokenFile: " + tok + "\nhost: cmd-test\n" +
		"labels: [fast-disk, gpu]\n" +
		"harnesses:\n  codex:\n    models: [gpt-5-codex]\n    efforts: [low, high]\n    default: gpt-5-codex\n" +
		"lending:\n  queues: [poc]\n" + extra
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type fakeTooling struct{ missing string }

func (f fakeTooling) Check(context.Context) []probe.Tool {
	var out []probe.Tool
	for _, n := range probe.BaseTools() {
		t := probe.Tool{Name: n, OK: n != f.missing}
		if !t.OK {
			t.Problem = "not found on PATH"
		}
		out = append(out, t)
	}
	return out
}

func probeOpts(missing string) probe.Options {
	return probe.Options{Tooling: fakeTooling{missing: missing}, Harnesses: map[string]probe.Prober{}}
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
		"labels: fast-disk, gpu\n", "git: ok",
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
	if len(rep.Capabilities.Harnesses) != 1 || rep.Capabilities.Harnesses[0].Name != "codex" || len(rep.Dropped) != 0 {
		t.Errorf("report %+v", rep)
	}

	errOut.Reset()
	if code := runProbe(context.Background(), []string{"--config", p}, io.Discard, &errOut, probeOpts("git"), homeIn(dir)); code != 1 ||
		!strings.Contains(errOut.String(), "base tool git") {
		t.Errorf("probe without git = %d %q", code, errOut.String())
	}
	// Without its own probers, probe offers a harness by its login in the state dir, as
	// supervise does: there is none here.
	out.Reset()
	errOut.Reset()
	if code := runProbe(context.Background(), []string{"--config", p, "--state-dir", t.TempDir()}, &out, &errOut,
		probe.Options{Tooling: fakeTooling{}}, homeIn(dir)); code != 1 ||
		!strings.Contains(out.String(), "run `factory-agent login codex`") || !strings.Contains(errOut.String(), "no harness passed its probe") {
		t.Errorf("probe without logins = %d %q %q", code, out.String(), errOut.String())
	}
	if code := runProbe(context.Background(), []string{"--config", filepath.Join(dir, "missing.yaml")}, io.Discard, io.Discard, probeOpts(""), homeIn(dir)); code != 1 {
		t.Errorf("probe with no config = %d", code)
	}
	if code := runProbe(context.Background(), []string{"extra"}, io.Discard, io.Discard, probeOpts(""), homeIn(dir)); code != 2 {
		t.Errorf("probe extra = %d", code)
	}
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

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

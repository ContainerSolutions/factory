package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// noisyRunner is a make that writes lines lines of output (one with a credential early on,
// the last naming the failing step) and fails; the commands that take the stack down write
// their own line and pass.
type noisyRunner struct{ lines int }

const (
	noisySecret   = "GITHUB_TOKEN=ghp_0123456789abcdefghijABCDEFGHIJ012345"
	noisyLastLine = "make: *** [github-check-creds] Error 1 (token ghp_0123456789abcdefghijABCDEFGHIJ012345)"
)

func (n noisyRunner) Run(_ context.Context, c Command, out io.Writer) error {
	if c.Name != "make" || !strings.HasPrefix(c.Args[0], "demo-up-") {
		_, _ = io.WriteString(out, "ran "+c.Name+"\n")
		return nil
	}
	var b strings.Builder
	for i := range n.lines {
		switch i {
		case 1:
			b.WriteString(noisySecret + "\n")
		case n.lines - 1:
			b.WriteString(noisyLastLine + "\n")
		default:
			fmt.Fprintf(&b, "step %d\n", i)
		}
	}
	_, _ = io.WriteString(out, b.String())
	return errors.New("make demo-up-showcase: exit status 2")
}

func failingPipeline(t *testing.T, r Runner) (*ForPipeline, *bytes.Buffer, *clock.Fake) {
	t.Helper()
	m, err := New(Options{StateDir: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	clk := clock.NewFake(time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC))
	p := &ForPipeline{
		Manager: m, LocalDev: "/home/owner/local-dev", GitHub: newFakeGitHub(), Clock: clk,
		Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	return p, &logs, clk
}

func stackTask() v1alpha1.Task {
	return v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{
		Inputs: map[string]any{"verifyAgainst": map[string]any{"train": "compliance-framework/workflows#123"}},
	}}
}

// warnRecords are the "stack up failed" records in a JSON log.
func warnRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		if rec["msg"] == "stack up failed" {
			out = append(out, rec)
		}
	}
	return out
}

func TestForPipelineUpFailureLeavesTheOutput(t *testing.T) {
	p, logs, _ := failingPipeline(t, noisyRunner{lines: 1000})
	_, err := p.Up(context.Background(), harnessWorkspace(), stackTask())
	if err == nil {
		t.Fatal("Up passed")
	}
	wantPath := filepath.Join(p.Manager.StateDir(), LogsDir, "stack-factory-tsk-1-20261020T093000Z.log")
	if want := "exit status 2 (make output: " + wantPath + ", on the agent)"; !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error %q\nwant suffix %q", err, want)
	}
	if strings.Contains(err.Error(), "step ") || strings.Contains(err.Error(), "ghp_") {
		t.Errorf("the error holds make's output: %q", err)
	}

	// The log: 0600 in a 0700 directory, the last maxOutputLines lines after a header.
	st, err := os.Stat(wantPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("log %v %v", st, err)
	}
	if dst, err := os.Stat(filepath.Dir(wantPath)); err != nil || dst.Mode().Perm() != 0o700 {
		t.Errorf("logs dir %v %v", dst, err)
	}
	b, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "# stack up factory-tsk-1 (make demo-up-showcase) failed at 2026-10-20T09:30:00Z: ") {
		t.Errorf("header %q", lines[0])
	}
	body := lines[1:]
	if len(body) != maxOutputLines {
		t.Errorf("%d lines kept, want %d", len(body), maxOutputLines)
	}
	// make's last line comes before the down commands' output.
	if i := slices.Index(body, noisyLastLine); i < 0 || !slices.Contains(body[i:], "ran docker") {
		t.Errorf("log tail %q", body[max(len(body)-4, 0):])
	}
	if slices.Contains(body, "step 2") {
		t.Error("the log holds more than the tail")
	}

	// The WARN: make's last line only, redacted, and the path.
	recs := warnRecords(t, logs)
	if len(recs) != 1 {
		t.Fatalf("%d warn records: %s", len(recs), logs)
	}
	rec := recs[0]
	if rec["level"] != "WARN" || rec["project"] != "factory-tsk-1" || rec["target"] != "demo-up-showcase" ||
		rec["log"] != wantPath || !strings.Contains(fmt.Sprint(rec["err"]), "exit status 2") {
		t.Errorf("warn record %v", rec)
	}
	if want := "make: *** [github-check-creds] Error 1 (token [REDACTED])"; rec["last"] != want {
		t.Errorf("last = %q, want %q", rec["last"], want)
	}
	if s := logs.String(); strings.Contains(s, "ghp_") || strings.Contains(s, "step ") {
		t.Errorf("the worker's log holds make's output: %s", s)
	}
}

func TestForPipelineWaitFailureLeavesTheOutput(t *testing.T) {
	var ready atomic.Bool
	srv := stackServer(t, &ready)
	m, err := New(Options{StateDir: t.TempDir(), Runner: &fakeRunner{}, UIURL: srv.URL, APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	p := &ForPipeline{
		Manager: m, LocalDev: "/home/owner/local-dev", GitHub: newFakeGitHub(), WaitTimeout: time.Millisecond,
		Clock:  clock.NewFake(time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)),
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	_, err = p.Up(context.Background(), harnessWorkspace(), stackTask())
	wantPath := filepath.Join(m.StateDir(), LogsDir, "stack-factory-tsk-1-20261020T093000Z.log")
	if err == nil || !strings.Contains(err.Error(), "wasn't ready") || !strings.HasSuffix(err.Error(), "(make output: "+wantPath+", on the agent)") {
		t.Fatalf("Up = %v", err)
	}
	b, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ran make\nran docker\n") {
		t.Errorf("log %q", b)
	}
	// make's last line, before the stack was taken down.
	if recs := warnRecords(t, &logs); len(recs) != 1 || recs[0]["last"] != "ran make" {
		t.Errorf("warn records %v", recs)
	}
}

func TestFailureLogPruning(t *testing.T) {
	p, _, _ := failingPipeline(t, noisyRunner{lines: 3})
	dir := filepath.Join(p.Manager.StateDir(), LogsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 25 older logs (old-00 the oldest) and a file that isn't one.
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 25 {
		path := filepath.Join(dir, fmt.Sprintf("stack-factory-old-%02d.log", i))
		if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := old.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Two failures in the same second: two logs.
	for range 2 {
		if _, err := p.Up(context.Background(), harnessWorkspace(), stackTask()); err == nil {
			t.Fatal("Up passed")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var want []string
	for i := 7; i < 25; i++ {
		want = append(want, fmt.Sprintf("stack-factory-old-%02d.log", i))
	}
	want = append(want, "notes.txt", "stack-factory-tsk-1-20261020T093000Z-2.log", "stack-factory-tsk-1-20261020T093000Z.log")
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("logs\n got %q\nwant %q", names, want)
	}
	if dst, err := os.Stat(dir); err != nil || dst.Mode().Perm() != 0o700 {
		t.Errorf("logs dir %v %v", dst, err)
	}
}

func TestRedactLine(t *testing.T) {
	for in, want := range map[string]string{
		"make: *** [github-check-creds] Error 1":                    "make: *** [github-check-creds] Error 1",
		"GITHUB_TOKEN=ghp_0123456789abcdefghij":                     "GITHUB_TOKEN=[REDACTED]",
		"using gho_ABCDEFGH12345678 and ghs_abcdefgh12345678 ok":    "using [REDACTED] and [REDACTED] ok",
		"pat github_pat_11ABCDEFG0_abcdefghijklmnop end":            "pat [REDACTED] end",
		"OPENAI_API_KEY='sk-proj-abcDEF123_456-xyz'":                "OPENAI_API_KEY='[REDACTED]'",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig failed": "Authorization: Bearer [REDACTED] failed",
		"authorization: bearer\tabc123":                             "authorization: bearer [REDACTED]",
		"task-runner and ghp_short stay":                            "task-runner and ghp_short stay",
	} {
		if got := redactLine(in); got != want {
			t.Errorf("redactLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		"one\ntwo\n\n  \n":              "two",
		"pulling 10%\rpulling 100%\n":   "pulling 100%",
		"only":                          "only",
		strings.Repeat("x", 600) + "\n": strings.Repeat("x", maxLastLine) + "…",
	} {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\n", 2, "a\nb\n"},
		{"a\nb\n", 5, "a\nb\n"},
		{"a\nb", 1, "b"},
	} {
		if got := lastLines(c.in, c.n); got != c.want {
			t.Errorf("lastLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

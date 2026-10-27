package workspace

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/login"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// TestMain answers `fx git-credential`: PrepareBundle's skills fetch runs the test binary as
// the credential helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fx" && os.Args[2] == "git-credential" {
		if err := gitcred.Helper(context.Background(), os.Args[3:], os.Stdin, os.Stdout, os.Getenv(gitcred.EnvSocket)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(5)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const (
	wsTaskID = "tsk_ws1"
	// claudeToken and codexToken are the owner's logins: never in the bundle or the transcript.
	claudeToken = "claude-oauth-login-never-in-a-vm-0123"
	codexToken  = "codex-access-login-never-in-a-vm-0123"
)

// The VM's addresses: the gateway listens on loopback, and the VM's address is one the agent
// host doesn't have (TEST-NET-1), so the test's own connections are refused peers.
var (
	vmAddr = netip.MustParseAddr("192.0.2.10")
	gwAddr = netip.MustParseAddr("127.0.0.1")
)

// vmScript is what the fake VM does when factory-agent run is exec'd: vm is its file system
// root, out its /work/out, events its stdout.
type vmScript func(ctx context.Context, r *rig, out string, events *runbundle.Encoder) error

// fakeDriver is an in-process workspace.Driver: each workspace is a directory under root,
// Exec runs the rig's script, and every call is recorded.
type fakeDriver struct {
	r    *rig
	root string

	mu    sync.Mutex
	calls []string
	argv  []string
}

func (d *fakeDriver) record(op, name string) error {
	d.mu.Lock()
	d.calls = append(d.calls, op)
	d.mu.Unlock()
	return ValidName(name)
}

func (d *fakeDriver) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *fakeDriver) vm(name, p string) string {
	return filepath.Join(d.root, name, filepath.FromSlash(p))
}

func (d *fakeDriver) Create(_ context.Context, name string, spec Spec) error {
	if err := d.record("create", name); err != nil {
		return err
	}
	if spec != (Spec{CPU: DefaultCPU, MemoryMiB: DefaultMemoryMiB}) {
		return fmt.Errorf("spec %+v", spec)
	}
	return os.MkdirAll(d.vm(name, "/work"), 0o700)
}

func (d *fakeDriver) Start(_ context.Context, name string) (Network, error) {
	return Network{VM: vmAddr, Gateway: gwAddr}, d.record("start", name)
}

func (d *fakeDriver) Put(_ context.Context, name, src, dst string) error {
	if err := d.record("put", name); err != nil {
		return err
	}
	return copyTree(src, d.vm(name, dst))
}

func (d *fakeDriver) Exec(ctx context.Context, name string, argv []string, _ io.Reader, stdout, _ io.Writer) error {
	if err := d.record("exec", name); err != nil {
		return err
	}
	d.mu.Lock()
	d.argv = slices.Clone(argv)
	d.mu.Unlock()
	out := d.vm(name, VMOutDir)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	return d.r.script(ctx, d.r, out, runbundle.NewEncoder(stdout, time.Now))
}

// fakeMaxGet caps what the fake's Get copies, as tart's MaxGet does.
const fakeMaxGet = 1 << 20

// Get copies src, or only its named files (a missing one skipped), and fails over fakeMaxGet.
func (d *fakeDriver) Get(_ context.Context, name, src, dst string, files ...string) error {
	if err := d.record("get", name); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return errors.New("get: dst exists")
	}
	from := d.vm(name, src)
	if len(files) == 0 {
		if err := copyTree(from, dst); err != nil {
			return err
		}
	} else {
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		for _, f := range files {
			if _, err := os.Lstat(filepath.Join(from, f)); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err := copyTree(filepath.Join(from, f), filepath.Join(dst, f)); err != nil {
				return err
			}
		}
	}
	var size int64
	_ = filepath.WalkDir(dst, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	if size > fakeMaxGet {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("%s: getting %s: more than %d bytes", name, src, fakeMaxGet)
	}
	return nil
}

func (d *fakeDriver) Stop(_ context.Context, name string) error    { return d.record("stop", name) }
func (d *fakeDriver) Destroy(_ context.Context, name string) error { return d.record("destroy", name) }
func (d *fakeDriver) List(context.Context) ([]string, error)       { return nil, nil }

// copyTree copies src to dst, symlinks as symlinks (Get's hostile cases).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			return os.MkdirAll(to, 0o700)
		case e.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o600)
	})
}

// sink keeps what the runner relays.
type sink struct {
	mu       sync.Mutex
	progress []v1alpha1.ProgressEvent
	usage    runner.UsageDelta
}

func (s *sink) Progress(e v1alpha1.ProgressEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, e)
}

func (s *sink) Usage(u runner.UsageDelta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.InputTokens += u.InputTokens
	s.usage.OutputTokens += u.OutputTokens
}

// rig is one workspace run: harnesstest's dispatcher and git server (the bundle's sources and
// the transcript's upload), the host pipeline, the fake driver and the real gateway on
// loopback.
type rig struct {
	t      *testing.T
	e      *harnesstest.E2E
	job    *runner.Job
	state  string
	driver *fakeDriver
	sink   *sink
	script vmScript
	opts   RunnerOptions

	mu     sync.Mutex
	gwAddr string // the gateway's real listener
}

func newRig(t *testing.T, h v1alpha1.HarnessResolved, script vmScript) *rig {
	t.Helper()
	e := harnesstest.NewE2E(t, wsTaskID, "Write the intro.\n")
	r := &rig{t: t, e: e, job: e.Job(h, []string{"pr-opened"}), state: t.TempDir(), sink: &sink{}, script: script}
	r.driver = &fakeDriver{r: r, root: t.TempDir()}
	r.opts = RunnerOptions{
		Driver: r.driver, Pipeline: e.Pipeline(&fake.Adapter{}), StateDir: r.state,
		Listen: func(ctx context.Context, a netip.AddrPort) (net.Listener, error) {
			port := r.opts.GatewayPort
			if port == 0 {
				port = DefaultGatewayPort
			}
			if a != netip.AddrPortFrom(gwAddr, uint16(port)) { //nolint:gosec // a test's port
				return nil, fmt.Errorf("listen on %s", a)
			}
			var lc net.ListenConfig
			ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
			if err == nil {
				r.mu.Lock()
				r.gwAddr = ln.Addr().String()
				r.mu.Unlock()
			}
			return ln, err
		},
	}
	return r
}

func (r *rig) run(ctx context.Context) runner.Outcome {
	r.t.Helper()
	rn, err := NewRunner(r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	out, err := rn.Run(ctx, r.job, r.sink)
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return out
}

// knock connects to the gateway from loopback: a refused peer, so an egress record.
func (r *rig) knock() {
	r.mu.Lock()
	a := r.gwAddr
	r.mu.Unlock()
	c, err := net.Dial("tcp", a)
	if err != nil {
		r.t.Errorf("dial the gateway: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, c) // closed at accept
	_ = c.Close()
}

// The outcome files a script writes.
func writeOutcome(t *testing.T, out string, o runbundle.Outcome) {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, runbundle.OutcomeFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTranscript(t *testing.T, out string, lines ...string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = io.WriteString(zw, strings.Join(lines, "\n")+"\n")
	_ = zw.Close()
	if err := os.WriteFile(filepath.Join(out, runbundle.TranscriptFile), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func doneResult() *v1alpha1.Result {
	return &v1alpha1.Result{Task: wsTaskID, Status: v1alpha1.ResultDone, Outcome: "pr-opened", Summary: "pushed the intro"}
}

// finishWith is a script that sends a progress and a usage event, knocks on the gateway, writes
// o (with a transcript when lines are given) and sends the outcome event.
func finishWith(o runbundle.Outcome, lines ...string) vmScript {
	return func(_ context.Context, r *rig, out string, enc *runbundle.Encoder) error {
		t := r.t
		in, outTok := int64(10), int64(5)
		for _, e := range []runbundle.Event{
			{Type: runbundle.EventStart, Start: &runbundle.Start{Version: runbundle.Version, RunID: "run_1", TaskID: wsTaskID}},
			{Type: runbundle.EventProgress, Progress: &v1alpha1.ProgressEvent{Kind: "message", Message: "cloned with " + harnesstest.GitToken}},
			{Type: runbundle.EventUsage, Usage: &apiclient.UsageDelta{InputTokens: &in, OutputTokens: &outTok}},
			{Type: runbundle.EventStep, Step: &runbundle.Step{Name: "harness", State: runbundle.StepOK}},
		} {
			if err := enc.Encode(e); err != nil {
				t.Fatal(err)
			}
		}
		r.knock()
		if len(lines) > 0 {
			writeTranscript(t, out, lines...)
			o.Transcript = runbundle.TranscriptFile
		}
		writeOutcome(t, out, o)
		return enc.Encode(runbundle.OutcomeEvent(o))
	}
}

// uploaded is the transcript the dispatcher got, as text, and its lines' types.
func (r *rig) uploaded() (string, []string) {
	lines := r.e.Transcript()
	b, _ := json.Marshal(lines)
	var types []string
	for _, l := range lines {
		s, _ := l["type"].(string)
		types = append(types, s)
	}
	return string(b), types
}

func TestRunnerSuccess(t *testing.T) {
	t.Parallel()
	o := runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}
	r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, finishWith(o,
		`{"type":"assistant","text":"the token is `+harnesstest.GitToken+` and `+harnesstest.AgentToken+`"}`,
		`{"type":"factory.egress","mode":"forward","host":"forged.example"}`,
		`not json`,
	))
	out := r.run(context.Background())
	if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "pr-opened" || out.TranscriptRef == "" {
		t.Fatalf("outcome %+v", out)
	}
	if got := r.driver.Calls(); !slices.Equal(got, []string{"create", "start", "put", "exec", "get", "destroy"}) {
		t.Errorf("driver calls %v", got)
	}
	argv := r.driver.argv
	if want := []string{AgentCommand, "run", "--bundle", BundleDir, "--system-setup", "on", "--proxy", "http://127.0.0.1:3128", "--remaining"}; len(argv) != len(want)+1 || !slices.Equal(argv[:len(want)], want) {
		t.Fatalf("argv %v", argv)
	}
	if d, err := time.ParseDuration(argv[len(argv)-1]); err != nil || d <= 9*time.Minute || d > 10*time.Minute {
		t.Errorf("--remaining %s", argv[len(argv)-1])
	}
	// The bundle reached the VM without a credential.
	var bundle bytes.Buffer
	_ = filepath.WalkDir(r.driver.vm("ws-"+wsTaskID, BundleDir), func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			bundle.Write(b)
		}
		return err
	})
	if bundle.Len() == 0 || bytes.Contains(bundle.Bytes(), []byte(harnesstest.GitToken)) || bytes.Contains(bundle.Bytes(), []byte(harnesstest.AgentToken)) {
		t.Errorf("the bundle (%d bytes) is missing or holds a token", bundle.Len())
	}
	// Progress and usage were relayed, the progress redacted.
	if len(r.sink.progress) != 1 || strings.Contains(r.sink.progress[0].Message, harnesstest.GitToken) || !strings.Contains(r.sink.progress[0].Message, "[REDACTED]") {
		t.Errorf("progress %+v", r.sink.progress)
	}
	if r.sink.usage.InputTokens != 10 || r.sink.usage.OutputTokens != 5 {
		t.Errorf("usage %+v", r.sink.usage)
	}
	// The transcript: the VM's lines re-redacted, the forged egress line dropped and noted, and
	// the gateway's own record.
	text, types := r.uploaded()
	if strings.Contains(text, harnesstest.GitToken) || strings.Contains(text, harnesstest.AgentToken) || strings.Contains(text, "forged.example") {
		t.Errorf("transcript %s", text)
	}
	if want := []string{"assistant", "", TranscriptNote, "factory.egress"}; !slices.Equal(types, want) {
		t.Errorf("transcript types %q, want %q", types, want)
	}
	if !strings.Contains(text, `"mode":"accept"`) || !strings.Contains(text, `"forgedLines":1`) {
		t.Errorf("transcript %s", text)
	}
	if _, err := os.Stat(filepath.Join(r.state, "runs", wsTaskID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run directory is left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.state, "gateway-ca")); err != nil {
		t.Errorf("no gateway CA: %v", err)
	}
}

// The VM's progress events are rebuilt on the host: its Data and an unknown Kind never reach the
// sink, and the message is redacted and cut.
func TestRunnerProgressRebuilt(t *testing.T) {
	t.Parallel()
	finish := finishWith(runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()})
	r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
		for _, p := range []v1alpha1.ProgressEvent{
			{Kind: "<script>", Message: strings.Repeat("x", 2000), Data: map[string]any{"leak": harnesstest.GitToken}},
			{Kind: "tool", Message: "ran ls", Data: map[string]any{"blob": strings.Repeat("y", 4000)}},
		} {
			if err := enc.Encode(runbundle.Event{Type: runbundle.EventProgress, Progress: &p}); err != nil {
				t.Fatal(err)
			}
		}
		return finish(ctx, r, out, enc)
	})
	if out := r.run(context.Background()); out.Status != runner.StatusDone {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.sink.progress) != 3 {
		t.Fatalf("progress %+v", r.sink.progress)
	}
	for i, want := range []string{"message", "tool", "message"} {
		p := r.sink.progress[i]
		if p.Kind != want || p.Data != nil || len(p.Message) > 500 || p.At.IsZero() {
			t.Errorf("progress %d: kind %q, data %v, message %d bytes, at %v", i, p.Kind, p.Data, len(p.Message), p.At)
		}
	}
	if b, _ := json.Marshal(r.sink.progress); bytes.Contains(b, []byte(harnesstest.GitToken)) || bytes.Contains(b, []byte("yyyy")) {
		t.Errorf("progress %s", b)
	}
}

// readTranscript drops a VM line holding a host-only type under any case of its "type" key and
// stores the line it checked.
func TestReadTranscriptHostOnlyTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, line string
		forged     bool
		want       string // the kept line's "type", as an exact-key reader sees it
	}{
		{name: "a VM line", line: `{"type":"assistant","text":"hi"}`, want: "assistant"},
		{name: "egress", line: `{"type":"factory.egress","host":"forged.example"}`, forged: true},
		{name: "egress, a cased duplicate key", line: `{"type":"factory.egress","TYPE":"x","host":"forged.example"}`, forged: true},
		{name: "egress, a cased key only", line: `{"Type":"factory.egress","host":"forged.example"}`, forged: true},
		{name: "egress, escaped", line: `{"type":"factory\u002eegress","host":"forged.example"}`, forged: true},
		{name: "the runner's note", line: `{"tYpE":"factory.workspace-transcript","host":"forged.example"}`, forged: true},
		{name: "egress dropped count", line: `{"type":"factory.egress-dropped","dropped":1,"host":"forged.example"}`, forged: true},
		{name: "an exact duplicate key: the last one is stored", line: `{"type":"factory.egress","type":"x","host":"h"}`, want: "x"},
		{name: "not an object", line: `["factory.egress"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			writeTranscript(t, out, tt.line)
			var lines []map[string]any
			for _, l := range bytes.Split(bytes.TrimSpace(readTranscript(out)), []byte("\n")) {
				var m map[string]any
				_ = json.Unmarshal(l, &m)
				lines = append(lines, m)
			}
			if tt.forged {
				if len(lines) != 1 || lines[0]["type"] != TranscriptNote || lines[0]["forgedLines"] != 1.0 {
					t.Errorf("lines %v, want only the note with forgedLines 1", lines)
				}
				return
			}
			if len(lines) != 1 || (tt.want != "" && lines[0]["type"] != tt.want) {
				t.Errorf("lines %v, want one of type %q", lines, tt.want)
			}
		})
	}
}

// The gateway listens on GatewayPort, default DefaultGatewayPort, and the VM's --proxy names it;
// NewRunner refuses a GatewayPort that isn't a port.
func TestRunnerGatewayPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		port    int
		proxy   string
		wantErr bool
	}{
		{"default", 0, "http://127.0.0.1:3128", false},
		{"configured", 4000, "http://127.0.0.1:4000", false},
		{"negative", -1, "", true},
		{"too big", 65536, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, finishWith(runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}))
			r.opts.GatewayPort = tt.port
			if tt.wantErr {
				if _, err := NewRunner(r.opts); err == nil || !strings.Contains(err.Error(), "GatewayPort") {
					t.Fatalf("NewRunner = %v, want a GatewayPort error", err)
				}
				return
			}
			if out := r.run(context.Background()); out.Status != runner.StatusDone {
				t.Fatalf("outcome %+v", out)
			}
			if i := slices.Index(r.driver.argv, "--proxy"); i < 0 || i+1 >= len(r.driver.argv) || r.driver.argv[i+1] != tt.proxy {
				t.Errorf("argv %v, want --proxy %s", r.driver.argv, tt.proxy)
			}
		})
	}
}

func TestRunnerKeepWorkspace(t *testing.T) {
	t.Parallel()
	r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, finishWith(runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}))
	r.opts.KeepWorkspace = true
	if out := r.run(context.Background()); out.Status != runner.StatusDone {
		t.Fatalf("outcome %+v", out)
	}
	if got := r.driver.Calls(); got[len(got)-1] != "stop" || slices.Contains(got, "destroy") {
		t.Errorf("driver calls %v", got)
	}
	if _, err := os.Stat(filepath.Join(r.state, "runs", wsTaskID, "out", runbundle.OutcomeFile)); err != nil {
		t.Errorf("the run directory isn't kept: %v", err)
	}
}

// The VM's outcomes, as the runner re-checks them.
func TestRunnerOutcomes(t *testing.T) {
	t.Parallel()
	failedRes := &v1alpha1.Result{Task: wsTaskID, Status: v1alpha1.ResultFailed, Summary: "harness-error: claude exited 1"}
	far := time.Now().Add(100 * time.Hour)
	other := doneResult()
	other.Task = "tsk_other"
	shipped := doneResult()
	shipped.Outcome = "shipped"
	tests := []struct {
		name   string
		script vmScript
		check  func(t *testing.T, out runner.Outcome)
	}{
		{"failed", finishWith(runbundle.Outcome{Status: runner.StatusFailed, Reason: runner.ReasonHarnessError, Result: failedRes}),
			func(t *testing.T, out runner.Outcome) {
				if out.Status != runner.StatusFailed || out.Reason != runner.ReasonHarnessError || out.Result == nil || out.Result.Summary != failedRes.Summary {
					t.Errorf("outcome %+v", out)
				}
			}},
		{"throttled, clamped", finishWith(runbundle.Outcome{Status: runner.StatusThrottled, ThrottledUntil: &far}),
			func(t *testing.T, out runner.Outcome) {
				if out.Status != runner.StatusThrottled || out.ThrottledUntil == nil || out.ThrottledUntil.After(time.Now().Add(runbundle.MaxThrottle)) {
					t.Errorf("outcome %+v", out)
				}
			}},
		{"exit 4 without an outcome", func(_ context.Context, _ *rig, _ string, _ *runbundle.Encoder) error {
			return &ExitError{Code: 4}
		}, wantFailed(runner.ReasonHarnessError, "exit status 4")},
		{"a status that isn't one", writeRaw(`{"status":"merged"}`), wantFailed(runner.ReasonHarnessError, "outcome.status")},
		{"done without a result", writeRaw(`{"status":"done"}`), wantFailed(runner.ReasonHarnessError, "has no result")},
		{"another task's result", finishWith(runbundle.Outcome{Status: runner.StatusDone, Result: other}), wantFailed(runner.ReasonInvalidResult, "another task's")},
		{"a status that differs", finishWith(runbundle.Outcome{Status: runner.StatusNoop, Result: doneResult()}), wantFailed(runner.ReasonInvalidResult, "differs")},
		{"an outcome off the task's list", finishWith(runbundle.Outcome{Status: runner.StatusDone, Result: shipped}), wantFailed(runner.ReasonInvalidResult, "is not one of")},
		{"outcome.json a symlink", func(_ context.Context, r *rig, out string, _ *runbundle.Encoder) error {
			target := filepath.Join(r.t.TempDir(), "elsewhere.json")
			b, _ := json.Marshal(runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()})
			if err := os.WriteFile(target, b, 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(out, runbundle.OutcomeFile))
		}, wantFailed(runner.ReasonHarnessError, "not a regular file")},
		{"a big scratch beside the outcome", func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
			// docs-gif's recordings, frames and tapes in $FACTORY_OUT: over the collect's cap.
			if err := os.MkdirAll(filepath.Join(out, "gif", "frames"), 0o700); err != nil {
				return err
			}
			big := bytes.Repeat([]byte("x"), fakeMaxGet)
			for _, f := range []string{"gif/demo.mp4", "gif/frames/0001.png", "result.json"} {
				if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(f)), big, 0o600); err != nil {
					return err
				}
			}
			return finishWith(runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}, `{"type":"assistant","text":"recorded"}`)(ctx, r, out, enc)
		}, func(t *testing.T, out runner.Outcome) {
			if out.Status != runner.StatusDone || out.Result == nil || out.Result.Outcome != "pr-opened" || out.TranscriptRef == "" {
				t.Errorf("outcome %+v (summary %q)", out, summaryOf(out))
			}
		}},
		{"a transcript that isn't gzip", func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
			o := runbundle.Outcome{Status: runner.StatusDone, Result: doneResult(), Transcript: runbundle.TranscriptFile}
			if err := os.WriteFile(filepath.Join(out, runbundle.TranscriptFile), []byte("plain "+harnesstest.GitToken), 0o600); err != nil {
				return err
			}
			writeOutcome(r.t, out, o)
			return nil
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, tt.script)
			out := r.run(context.Background())
			if tt.check != nil {
				tt.check(t, out)
			} else { // the transcript that isn't gzip: a note instead of its bytes
				text, types := r.uploaded()
				if out.Status != runner.StatusDone || out.TranscriptRef == "" || !slices.Equal(types, []string{TranscriptNote}) ||
					!strings.Contains(text, "not gzip") || strings.Contains(text, harnesstest.GitToken) {
					t.Errorf("outcome %+v, transcript %s", out, text)
				}
			}
			if out.Result != nil && strings.Contains(out.Result.Summary, harnesstest.GitToken) {
				t.Errorf("summary %q", out.Result.Summary)
			}
			if got := r.driver.Calls(); got[len(got)-1] != "destroy" {
				t.Errorf("driver calls %v", got)
			}
		})
	}
}

func writeRaw(outcome string) vmScript {
	return func(_ context.Context, _ *rig, out string, _ *runbundle.Encoder) error {
		return os.WriteFile(filepath.Join(out, runbundle.OutcomeFile), []byte(outcome), 0o600)
	}
}

func wantFailed(reason, summary string) func(*testing.T, runner.Outcome) {
	return func(t *testing.T, out runner.Outcome) {
		t.Helper()
		if out.Status != runner.StatusFailed || out.Reason != reason || out.Result == nil || !strings.Contains(out.Result.Summary, summary) {
			t.Errorf("outcome %+v (summary %q), want failed{%s} with %q", out, summaryOf(out), reason, summary)
		}
	}
}

func summaryOf(o runner.Outcome) string {
	if o.Result == nil {
		return ""
	}
	return o.Result.Summary
}

// A cancel stops the run through Exec, destroys the workspace and reports nothing more.
func TestRunnerCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, func(ctx context.Context, _ *rig, out string, enc *runbundle.Encoder) error {
		cancel()
		<-ctx.Done()
		writeOutcome(t, out, runbundle.Outcome{Status: runner.StatusFailed, Reason: runner.ReasonCancelled})
		return ctx.Err()
	})
	out := r.run(ctx)
	if out.Status != runner.StatusFailed || out.Reason != runner.ReasonCancelled || out.Result != nil || out.TranscriptRef != "" {
		t.Errorf("outcome %+v", out)
	}
	if got := r.driver.Calls(); !slices.Equal(got, []string{"create", "start", "put", "exec", "destroy"}) {
		t.Errorf("driver calls %v", got)
	}
}

// The harness's login comes from the store: none fails the run before a VM exists; codex's
// account reaches the bundle.
func TestRunnerLogins(t *testing.T) {
	t.Parallel()
	t.Run("no claude login", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, v1alpha1.HarnessResolved{Name: "claude-code", Model: "m"}, nil)
		out := r.run(context.Background())
		wantFailed(runner.ReasonHarnessError, "login claude")(t, out)
		if got := r.driver.Calls(); len(got) != 0 {
			t.Errorf("driver calls %v", got)
		}
	})
	t.Run("no codex login", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, v1alpha1.HarnessResolved{Name: "codex", Model: "m"}, nil)
		wantFailed(runner.ReasonHarnessError, "login codex")(t, r.run(context.Background()))
	})
	t.Run("claude", func(t *testing.T) {
		t.Parallel()
		o := runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}
		r := newRig(t, v1alpha1.HarnessResolved{Name: "claude-code", Model: "m"}, finishWith(o, `{"type":"x","echo":"`+claudeToken+`"}`))
		if err := login.ForStateDir(r.state).SaveClaude(claudeToken + "\n"); err != nil {
			t.Fatal(err)
		}
		if out := r.run(context.Background()); out.Status != runner.StatusDone {
			t.Fatalf("outcome %+v", out)
		}
		if text, _ := r.uploaded(); strings.Contains(text, claudeToken) {
			t.Errorf("the transcript holds the claude login: %s", text)
		}
	})
	t.Run("codex", func(t *testing.T) {
		t.Parallel()
		var manifest runbundle.Manifest
		o := runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}
		done := finishWith(o)
		r := newRig(t, v1alpha1.HarnessResolved{Name: "codex", Model: "m"}, func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
			b, err := os.ReadFile(r.driver.vm("ws-"+wsTaskID, BundleDir+"/"+runbundle.ManifestFile))
			if err == nil {
				err = json.Unmarshal(b, &manifest)
			}
			if err != nil {
				return err
			}
			return done(ctx, r, out, enc)
		})
		if err := login.ForStateDir(r.state).SaveCodex(login.Codex{AccessToken: codexToken, AccountID: "acct_1", UserID: "user_1", PlanType: "pro"}); err != nil {
			t.Fatal(err)
		}
		if out := r.run(context.Background()); out.Status != runner.StatusDone {
			t.Fatalf("outcome %+v", out)
		}
		if manifest.Codex == nil || *manifest.Codex != (runbundle.CodexAccount{AccountID: "acct_1", UserID: "user_1", PlanType: "pro"}) {
			t.Errorf("the bundle's codex account %+v", manifest.Codex)
		}
	})
}

// The supervisor's own clock: no time left refuses the run; past the wall clock plus Overrun
// the run is stopped (failed{limit}); a run that doesn't exit DownBudget after its outcome
// event is stopped and its outcome collected anyway.
func TestRunnerDeadlines(t *testing.T) {
	t.Parallel()
	block := func(ctx context.Context, _ *rig, _ string, _ *runbundle.Encoder) error {
		<-ctx.Done()
		return ctx.Err()
	}
	t.Run("no time left", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, block)
		r.job.StartedAt = time.Now().Add(-11 * time.Minute)
		wantFailed(runner.ReasonLimit, "no wall clock left")(t, r.run(context.Background()))
		if got := r.driver.Calls(); slices.Contains(got, "exec") || got[len(got)-1] != "destroy" {
			t.Errorf("driver calls %v", got)
		}
	})
	t.Run("overrun", func(t *testing.T) {
		t.Parallel()
		clk := clock.NewFake(time.Now())
		r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
			clk.Advance(10*time.Minute + DefaultOverrun) // the VM never ends on its own
			return block(ctx, r, out, enc)
		})
		r.job.StartedAt = clk.Now()
		r.opts.Clock = clk
		wantFailed(runner.ReasonLimit, "past the task's wall clock")(t, r.run(context.Background()))
	})
	t.Run("down budget", func(t *testing.T) {
		t.Parallel()
		o := runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}
		r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
			writeOutcome(r.t, out, o)
			if err := enc.Encode(runbundle.OutcomeEvent(o)); err != nil {
				return err
			}
			<-ctx.Done() // a hung down
			return ctx.Err()
		})
		r.opts.DownBudget = 50 * time.Millisecond
		if out := r.run(context.Background()); out.Status != runner.StatusDone || out.Result == nil {
			t.Errorf("outcome %+v", out)
		}
	})
}

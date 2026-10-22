package vmrun

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/effector"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/internal/fakegateway"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// TestMain doubles as the workspace's `factory` CLI: the pipeline's bin/factory shim runs
// `<test binary> fx <op>`, as it runs `factory-agent fx <op>` in a VM.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fx" && effector.IsOp(os.Args[2:]) {
		e, err := effector.FromEnv(os.Getenv, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "factory:", err)
			os.Exit(effector.ExitUsage)
		}
		os.Exit(effector.Main(context.Background(), e, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

const (
	vmTaskID   = "tsk_vm1"
	vmPrompt   = "Write the intro.\n"
	vmBranch   = "factory/" + vmTaskID
	goodResult = `{"status": "done", "outcome": "pr-opened", "summary": "pushed the intro"}`
	badResult  = `{"status": "done", "outcome": "shipped", "summary": "x"}`
)

// shapedToken is shaped like an API key: what vmrun reports of an error never holds it.
const shapedToken = "sk-vmrunErrorKeyShapeNeverReported0123"

// t0 is the fake clock's start: every event's time.
var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// noRunner fails the test on any command: --system-setup off runs none.
type noRunner struct{ t *testing.T }

func (r noRunner) Run(_ context.Context, _, _ io.Writer, name string, args ...string) error {
	r.t.Errorf("system setup ran %s %v", name, args)
	return fmt.Errorf("%s: not in a test", name)
}

// vmRig is one VM workspace run, in process: the dispatcher and the git server of
// harnesstest.E2E (acme/docs, private and rw) reached only through the fakegateway, which holds
// every credential; the bundle PrepareBundle wrote on the "host"; the fake harness; and
// --system-setup off against a temp system root.
type vmRig struct {
	t                   *testing.T
	e                   *harnesstest.E2E
	gw                  *fakegateway.Gateway
	bundle, work, state string
	clk                 *clock.Fake
	adapter             *fake.Adapter
	sigs                chan os.Signal
	stdout, stderr      syncBuffer
	extraArgs           []string
	pushedSHA           string
	system              System
	adapters            func(*runbundle.Bundle, adapterOptions) (map[string]harness.Adapter, error)
}

// newVMRig builds the rig; mutate changes the job before its bundle is written.
func newVMRig(t *testing.T, mutate ...func(*runner.Job)) *vmRig {
	t.Helper()
	e := harnesstest.NewE2E(t, vmTaskID, vmPrompt)
	gw := fakegateway.Start(t, fakegateway.Config{RunToken: fakegateway.RunToken, Dispatcher: e.DispatcherURL(), TaskID: vmTaskID,
		GitUpstream: e.GitURL(), Repos: map[string]fakegateway.Repo{"acme/docs": {Token: harnesstest.GitToken, RW: true}}})
	job := e.Job(v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, []string{"pr-opened"})
	job.Lease.Task.Spec.Effects = &v1alpha1.EffectsPolicy{Allow: []v1alpha1.EffectOp{v1alpha1.OpPushBranch, v1alpha1.OpComment}}
	for _, m := range mutate {
		m(job)
	}
	base := t.TempDir()
	r := &vmRig{
		t: t, e: e, gw: gw, bundle: e.BundleJob(job, pipeline.BundleExtras{CA: gw.CA().CertPEM()}),
		work: filepath.Join(base, "work"), state: filepath.Join(base, "state"),
		clk: clock.NewFake(t0), adapter: &fake.Adapter{}, sigs: make(chan os.Signal, 2),
	}
	home := filepath.Join(base, "home")
	r.system = System{Root: filepath.Join(base, "root"), Runner: noRunner{t},
		User: func() (*user.User, error) {
			return &user.User{Uid: "1000", Gid: "1000", Username: "admin", HomeDir: home}, nil
		}}
	r.adapters = func(*runbundle.Bundle, adapterOptions) (map[string]harness.Adapter, error) {
		return map[string]harness.Adapter{fake.Name: r.adapter}, nil
	}
	return r
}

// args are the supervisor's command line.
func (r *vmRig) args() []string {
	return append([]string{"--proxy", r.gw.URL(), "--bundle", r.bundle, "--work", r.work, "--state-dir", r.state,
		"--system-setup", "off", "--log-format", "text", "--log-level", "debug"}, r.extraArgs...)
}

func (r *vmRig) main() int {
	r.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		r.t.Fatal(err)
	}
	code := mainWith(context.Background(), r.args(), &r.stdout, &r.stderr, deps{
		system: r.system, adapters: r.adapters, agentBinary: exe, clock: r.clk, signals: r.sigs,
	})
	if r.t.Failed() {
		r.t.Logf("stderr:\n%s", r.stderr.String())
	}
	return code
}

// workIt is the harness's work: a commit on the task branch, `factory push` and `factory
// comment`, each through the gateway with no credential of its own.
func (r *vmRig) workIt(ctx context.Context, ws harness.Workspace, _ string) error {
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = ws.Repo, ws.Env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("checkout", "--quiet", "-b", vmBranch); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ws.Repo, "intro.md"), []byte("# Intro\n"), 0o600); err != nil {
		return err
	}
	if _, err := git("add", "-A"); err != nil {
		return err
	}
	if _, err := git("commit", "--quiet", "--no-gpg-sign", "-m", "intro"); err != nil {
		return err
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	r.pushedSHA = sha
	for _, argv := range [][]string{{"push"}, {"comment", "--body", "Pushed the intro."}} {
		cmd := exec.CommandContext(ctx, filepath.Join(ws.Bin, "factory"), argv...)
		cmd.Dir, cmd.Env = ws.Repo, ws.Env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("factory %v: %w\n%s", argv, err, out)
		}
	}
	return nil
}

// events decodes stdout.
func (r *vmRig) events() []runbundle.Event {
	r.t.Helper()
	d := runbundle.NewDecoder(strings.NewReader(r.stdout.String()))
	var out []runbundle.Event
	for {
		e, err := d.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			r.t.Fatalf("the event stream: %v", err)
		}
		out = append(out, e)
	}
}

// outcome is outcome.json as the VM wrote it (unclamped).
func (r *vmRig) outcome() runbundle.Outcome {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.work, runbundle.OutDir, runbundle.OutcomeFile))
	if err != nil {
		r.t.Fatalf("outcome.json: %v", err)
	}
	var o runbundle.Outcome
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

func stepNames(evs []runbundle.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Step != nil {
			out = append(out, e.Step.Name+":"+e.Step.State)
		}
	}
	return out
}

func eventTypes(evs []runbundle.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// The VM workspace runs end to end: the harness pushes and comments through the gateway, the
// outcome and the transcript land in out/, the events are pinned, and no credential reaches the
// harness's environment, the workspace, the state dir or the bundle.
func TestRunEndToEnd(t *testing.T) {
	t.Parallel()
	r := newVMRig(t)
	r.adapter.Script = fake.Script{Turn: fake.Turn{
		Do:         r.workIt,
		Events:     []harness.ProgressEvent{{Kind: harness.KindTurn, Summary: "writing the intro"}},
		Usage:      []harness.UsageDelta{{InputTokens: 120, OutputTokens: 30}},
		Result:     []byte(goodResult),
		Transcript: []string{`{"type":"assistant","text":"done"}`},
	}}
	if code := r.main(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	o := r.outcome()
	if o.Status != runner.StatusDone || o.Result == nil || o.Result.Outcome != "pr-opened" || o.Transcript != runbundle.TranscriptFile {
		t.Fatalf("outcome %+v %+v", o, o.Result)
	}
	if host, err := runbundle.ReadOutcome(filepath.Join(r.work, runbundle.OutDir), t0); err != nil || host.Status != runner.StatusDone {
		t.Errorf("the host's ReadOutcome: %+v %v", host, err)
	}
	if lines := gunzipLines(t, filepath.Join(r.work, runbundle.OutDir, runbundle.TranscriptFile)); !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, `"text":"done"`)
	}) {
		t.Errorf("the transcript: %q", lines)
	}

	// The push landed and both effects reached the dispatcher with the gateway's run token.
	if got := r.e.RemoteHead("acme/docs", vmBranch); got == "" || got != r.pushedSHA {
		t.Errorf("remote %s = %q, want %q", vmBranch, got, r.pushedSHA)
	}
	effects := r.e.Effects()
	var ops []string
	for _, ef := range effects {
		ops = append(ops, ef.Op)
		if ef.Auth != "Bearer "+fakegateway.RunToken {
			t.Errorf("%s reached the dispatcher with %q", ef.Op, ef.Auth)
		}
	}
	if !slices.Equal(ops, []string{"push-branch", "comment"}) {
		t.Errorf("effects %v", ops)
	}
	r.gw.AssertNoClientCredential(t)

	// No credential anywhere in the VM.
	ws := r.adapter.Prepared()
	if len(ws) != 1 {
		t.Fatalf("prepared %d workspaces", len(ws))
	}
	env := map[string]string{}
	for _, kv := range ws[0].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
		if hit := credential(v); hit != "" {
			t.Errorf("the harness's %s holds %s", k, hit)
		}
	}
	if _, ok := env[pipeline.EnvToken]; ok {
		t.Errorf("the harness has %s", pipeline.EnvToken)
	}
	if env[pipeline.EnvGHToken] != harness.Placeholder || env[pipeline.EnvRuntime] != pipeline.RuntimeWorkspace || env[pipeline.EnvAPI] != runbundle.DefaultDispatcherURL {
		t.Errorf("the harness's environment: %v", env)
	}
	for _, dir := range []string{r.work, r.state, r.bundle} {
		scanNoCredential(t, dir)
	}

	// The event stream, pinned.
	evs := r.events()
	if got := eventTypes(evs); len(got) < 2 || got[0] != runbundle.EventStart || got[len(got)-1] != runbundle.EventOutcome {
		t.Errorf("event types %v", got)
	}
	golden(t, filepath.Join("testdata", "run", "events.golden"), r.stdout.String())
}

// gunzipLines is a gzipped file's lines.
func gunzipLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// credentialShapes are what no file or variable of the VM may hold: GitHub's, the run token's
// and API keys' shapes.
var credentialShapes = regexp.MustCompile(`gh[opsur]_[A-Za-z0-9]{20,}|rtk_[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{20,}`)

// credential names the real token or the shape s holds, or "".
func credential(s string) string {
	for name, tok := range map[string]string{"the git token": harnesstest.GitToken, "the agent token": harnesstest.AgentToken, "the run token": fakegateway.RunToken} {
		if strings.Contains(s, tok) {
			return name
		}
	}
	if credentialShapes.MatchString(s) {
		return "a token shape"
	}
	return ""
}

// scanNoCredential walks dir's regular files (a gzip decompressed) for credentials.
func scanNoCredential(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".gz") {
			if zr, zerr := gzip.NewReader(bytes.NewReader(b)); zerr == nil {
				if raw, rerr := io.ReadAll(zr); rerr == nil {
					b = raw
				}
			}
		}
		if hit := credential(string(b)); hit != "" {
			t.Errorf("%s holds %s", p, hit)
		}
		return nil
	})
	if err != nil {
		t.Errorf("walking %s: %v", dir, err)
	}
}

// The other ends of a run: each writes its outcome (exit 0) with the expected status and
// reason, and its events end with it.
func TestRunOutcomes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		args       []string
		job        func(*runner.Job)
		script     func(r *vmRig) fake.Script
		status     string
		reason     string
		transcript bool
		steps      []string // a subsequence of the step events
		logs       []string // in the log, in any order
		check      func(t *testing.T, r *vmRig, o runbundle.Outcome)
	}{
		{name: "repair", script: func(*vmRig) fake.Script {
			return fake.Script{Turn: fake.Turn{Result: []byte(badResult)}, Repairs: []fake.Turn{{Result: []byte(goodResult)}}}
		}, status: runner.StatusDone, transcript: true, steps: []string{"harness:ok", "repair:ok", "finish:ok"}},
		{name: "invalid twice", script: func(*vmRig) fake.Script {
			return fake.Script{Turn: fake.Turn{Result: []byte(badResult)}, Repairs: []fake.Turn{{Result: []byte(badResult)}}}
		}, status: runner.StatusFailed, reason: runner.ReasonInvalidResult, transcript: true, steps: []string{"harness:ok", "repair:error", "finish:ok"}},
		{name: "throttled", script: func(*vmRig) fake.Script {
			return fake.Script{Turn: fake.Turn{Events: []harness.ProgressEvent{{Summary: "a"}}, RateLimitAfter: 1, RateLimitUntil: t0.Add(24 * time.Hour)}}
		}, status: runner.StatusThrottled, transcript: true, check: func(t *testing.T, r *vmRig, o runbundle.Outcome) {
			// The VM reports its own throttle; the host clamps it to MaxThrottle.
			if o.ThrottledUntil == nil || !o.ThrottledUntil.Equal(t0.Add(24*time.Hour)) {
				t.Errorf("the VM's throttle %v", o.ThrottledUntil)
			}
			host, err := runbundle.ReadOutcome(filepath.Join(r.work, runbundle.OutDir), t0)
			if err != nil || host.ThrottledUntil == nil || !host.ThrottledUntil.Equal(t0.Add(runbundle.MaxThrottle)) {
				t.Errorf("the host's throttle %+v %v", host, err)
			}
		}},
		{name: "--remaining 1s", args: []string{"--remaining", "1s"}, script: func(r *vmRig) fake.Script {
			// The task's maxWallClock is 10m: only the remaining second is left.
			return fake.Script{Turn: fake.Turn{Block: true, Do: func(context.Context, harness.Workspace, string) error {
				r.clk.Advance(2 * time.Second)
				return nil
			}}}
		}, status: runner.StatusFailed, reason: runner.ReasonLimit, transcript: true, steps: []string{"harness:error", "finish:ok"}},
		{name: "SIGTERM", script: func(r *vmRig) fake.Script {
			return fake.Script{Turn: fake.Turn{Block: true, Do: func(context.Context, harness.Workspace, string) error {
				r.sigs <- syscall.SIGTERM
				return nil
			}}}
		}, status: runner.StatusFailed, reason: runner.ReasonCancelled, steps: []string{"harness:error", "finish:ok"},
			logs: []string{"cancelling the run"}},
		{name: "SIGTERM twice", script: func(r *vmRig) fake.Script {
			return fake.Script{Turn: fake.Turn{Block: true, Do: func(ctx context.Context, _ harness.Workspace, _ string) error {
				r.sigs <- syscall.SIGTERM
				<-ctx.Done() // the first cancelled the run
				r.sigs <- syscall.SIGINT
				// The run ends once Do returns: wait until the second signal is handled.
				for deadline := time.Now().Add(10 * time.Second); !strings.Contains(r.stderr.String(), "a second signal") && time.Now().Before(deadline); {
					time.Sleep(5 * time.Millisecond)
				}
				return nil
			}}}
		}, status: runner.StatusFailed, reason: runner.ReasonCancelled,
			logs: []string{"cancelling the run", "a second signal"}},
		{name: "an invalid maxWallClock", job: func(job *runner.Job) {
			job.Lease.Task.Spec.Limits.MaxWallClock = "soon"
		}, script: func(*vmRig) fake.Script { return fake.Script{} }, status: runner.StatusFailed, reason: runner.ReasonHarnessError,
			check: func(t *testing.T, r *vmRig, o runbundle.Outcome) {
				// Run refuses it before any step: vmrun reports the outcome itself.
				if o.Result == nil || !strings.Contains(o.Result.Summary, "limits.maxWallClock") || len(r.adapter.Prepared()) != 0 || len(stepNames(r.events())) != 0 {
					t.Errorf("result %+v, prepared %d, steps %v", o.Result, len(r.adapter.Prepared()), stepNames(r.events()))
				}
			}},
		{name: "system setup fails", script: func(r *vmRig) fake.Script {
			if err := os.WriteFile(r.state, []byte("x"), 0o600); err != nil {
				r.t.Fatal(err)
			}
			return fake.Script{}
		}, status: runner.StatusFailed, reason: setupFailReason, check: func(t *testing.T, r *vmRig, o runbundle.Outcome) {
			if o.Result == nil || !strings.Contains(o.Result.Summary, "the state dir") || len(r.adapter.Prepared()) != 0 {
				t.Errorf("result %+v, prepared %d", o.Result, len(r.adapter.Prepared()))
			}
			if got := eventTypes(r.events()); !slices.Equal(got, []string{runbundle.EventStart, runbundle.EventOutcome}) {
				t.Errorf("events %v", got)
			}
		}},
		{name: "no adapter", script: func(r *vmRig) fake.Script {
			r.adapters = func(*runbundle.Bundle, adapterOptions) (map[string]harness.Adapter, error) {
				return nil, fmt.Errorf("no adapter for harness %q (key %s)", fake.Name, shapedToken)
			}
			return fake.Script{}
		}, status: runner.StatusFailed, reason: runner.ReasonHarnessError, check: func(t *testing.T, r *vmRig, o runbundle.Outcome) {
			// The failed Result's summary is the error, redacted, as the pipeline's are.
			want := "harness-error: adapters: no adapter for harness \"fake\" (key " + pipeline.Redacted + ")"
			if o.Result == nil || o.Result.Status != v1alpha1.ResultFailed || o.Result.Task != vmTaskID || o.Result.Summary != want {
				t.Errorf("result %+v, want the summary %q", o.Result, want)
			}
			if strings.Contains(r.stderr.String(), shapedToken) || !strings.Contains(r.stderr.String(), pipeline.Redacted) {
				t.Errorf("the log isn't redacted:\n%s", r.stderr.String())
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mutate []func(*runner.Job)
			if tt.job != nil {
				mutate = append(mutate, tt.job)
			}
			r := newVMRig(t, mutate...)
			r.extraArgs = tt.args
			r.adapter.Script = tt.script(r)
			if code := r.main(); code != ExitOK {
				t.Fatalf("exit %d", code)
			}
			o := r.outcome()
			if o.Status != tt.status || o.Reason != tt.reason || (o.Transcript != "") != tt.transcript {
				t.Fatalf("outcome %+v %+v", o, o.Result)
			}
			_, err := os.Stat(filepath.Join(r.work, runbundle.OutDir, runbundle.TranscriptFile))
			if (err == nil) != tt.transcript {
				t.Errorf("the transcript file: %v", err)
			}
			evs := r.events()
			last := evs[len(evs)-1]
			if last.Outcome == nil || last.Outcome.Status != tt.status || last.Outcome.Reason != tt.reason || last.Outcome.Result != nil {
				t.Errorf("the last event %+v", last)
			}
			if steps := stepNames(evs); !isSubsequence(tt.steps, steps) {
				t.Errorf("steps %v, want %v among them", steps, tt.steps)
			}
			for _, l := range tt.logs {
				if !strings.Contains(r.stderr.String(), l) {
					t.Errorf("the log has no %q:\n%s", l, r.stderr.String())
				}
			}
			if tt.check != nil {
				tt.check(t, r, o)
			}
			for _, dir := range []string{r.work, r.bundle} {
				scanNoCredential(t, dir)
			}
		})
	}
}

func isSubsequence(want, got []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// A bundle that doesn't read is exit 3, with no event and no outcome.
func TestRunBadBundle(t *testing.T) {
	t.Parallel()
	r := newVMRig(t)
	p := filepath.Join(r.bundle, runbundle.ManifestFile)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"version": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.main(); code != ExitBundle {
		t.Fatalf("exit %d, want %d", code, ExitBundle)
	}
	if r.stdout.String() != "" || !strings.Contains(r.stderr.String(), "the run bundle is invalid") {
		t.Errorf("stdout %q, stderr %q", r.stdout.String(), r.stderr.String())
	}
	if _, err := os.Lstat(r.work); err == nil {
		t.Error("the work dir was made")
	}
}

// An outcome that can't be written is exit 4; the outcome event is still sent.
func TestRunOutcomeNotWritten(t *testing.T) {
	t.Parallel()
	r := newVMRig(t)
	r.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(goodResult), Do: func(_ context.Context, ws harness.Workspace, _ string) error {
		return os.Mkdir(filepath.Join(ws.Out, runbundle.OutcomeFile), 0o700) // the rename can't replace a directory
	}}}
	if code := r.main(); code != ExitOutcome {
		t.Fatalf("exit %d, want %d", code, ExitOutcome)
	}
	evs := r.events()
	if last := evs[len(evs)-1]; last.Outcome == nil || last.Outcome.Status != runner.StatusDone {
		t.Errorf("the last event %+v", last)
	}
}

// Usage errors are exit 2, before the bundle is read.
func TestRunUsage(t *testing.T) {
	t.Parallel()
	ok := []string{"--proxy", "http://192.168.64.1:3128", "--bundle", "/run/factory", "--work", "/work", "--state-dir", "/var/lib/factory-agent/run"}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no proxy", []string{"--bundle", "/b"}, "--proxy is required"},
		{"a proxy with a path", append(slices.Clone(ok), "--proxy", "http://gw:3128/x"), "only a host and a port"},
		{"an https proxy", append(slices.Clone(ok), "--proxy", "https://gw:3128"), "the scheme must be http"},
		{"a relative work dir", append(slices.Clone(ok), "--work", "work"), `--work "work" must be absolute`},
		{"the state dir in the work dir", append(slices.Clone(ok), "--state-dir", "/work/state"), "must not overlap"},
		{"the work dir in the state dir", append(slices.Clone(ok), "--work", "/var/lib/factory-agent/run/w"), "must not overlap"},
		{"the bundle in the work dir", append(slices.Clone(ok), "--bundle", "/work/b"), "--bundle must not be in --work"},
		{"a negative remaining", append(slices.Clone(ok), "--remaining", "-1s"), "must not be negative"},
		{"a zero down timeout", append(slices.Clone(ok), "--down-timeout", "0s"), "must be positive"},
		{"an unknown setup mode", append(slices.Clone(ok), "--system-setup", "maybe"), "--system-setup"},
		{"the removed auto mode", append(slices.Clone(ok), "--system-setup", "auto"), "there is no automatic mode"},
		{"an unknown harness binary", append(slices.Clone(ok), "--harness-bin", "gemini=/bin/g"), "the harness must be one of"},
		{"a bad log format", append(slices.Clone(ok), "--log-format", "xml"), "--log-format"},
		{"an argument", append(slices.Clone(ok), "extra"), "unexpected arguments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := Main(context.Background(), tt.args, &stdout, &stderr); code != ExitUsage {
				t.Fatalf("exit %d, want %d (%s)", code, ExitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) || stdout.Len() != 0 {
				t.Errorf("stderr %q, want %q; stdout %q", stderr.String(), tt.want, stdout.String())
			}
		})
	}
	// --help lists the flags but the hidden --harness-bin.
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"--help"}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stderr.String(), "--remaining") || strings.Contains(stderr.String(), "harness-bin") {
		t.Errorf("--help: %d %q", code, stderr.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the run's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

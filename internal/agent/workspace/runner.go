package workspace

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/gateway"
	"github.com/compliance-framework/factory/internal/agent/gateway/ca"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/login"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/clock"
)

// The workspace runner's fixed places (supervise.md, "The workspace runner").
const (
	// BundleDir is where Put lays the run bundle in the VM.
	BundleDir = "/run/factory"
	// VMOutDir is factory-agent run's out/ in the VM (--work /work).
	VMOutDir = "/work/out"
	// GatewayPort is the gateway's port on the VM's gateway address: the pf anchor's.
	GatewayPort = 3128
	// AgentCommand is factory-agent in the VM (the base image puts it on PATH).
	AgentCommand = "factory-agent"
)

// Defaults of RunnerOptions.
const (
	// DefaultOverrun is how long past the task's wall clock the runner lets the VM run before
	// it cancels it: the VM's own deadline (--remaining) fires first and writes the outcome.
	DefaultOverrun = 2 * time.Minute
	// DefaultDownBudget is how long the runner waits for factory-agent run to exit after its
	// outcome event (its environment's down, at most --down-timeout).
	DefaultDownBudget = pipeline.StackDownTimeout + 30*time.Second
	// releaseTimeout bounds Destroy or Stop, and uploadTimeout the transcript upload.
	releaseTimeout = 5 * time.Minute
	uploadTimeout  = 30 * time.Second
)

// The caps on what the runner reads from the VM, besides runbundle's.
const (
	maxTranscriptRaw  = 256 << 20 // the VM's transcript, decompressed
	maxTranscriptLine = 1 << 20   // one of its lines; a longer one is dropped
	maxEgressRecords  = 50_000
	maxStderr         = 64 << 10
)

// Bundler is what the runner needs of the host's run pipeline (*pipeline.Pipeline): the run
// bundle before the VM exists, and the transcript upload after.
type Bundler interface {
	PrepareBundle(ctx context.Context, job *runner.Job, dir string, x pipeline.BundleExtras) error
	StoreTranscript(ctx context.Context, job *runner.Job, parts [][]byte, extra ...string) (string, error)
}

// RunnerOptions configure a Runner. Driver, Pipeline and StateDir are required.
type RunnerOptions struct {
	Driver Driver
	// Pipeline is the host's pipeline (WorkRoot, GitAuthHelper).
	Pipeline Bundler
	// StateDir is the agent's state dir: runs/<task>/ (the bundle and the VM's out/),
	// gateway-ca/ and logins/.
	StateDir string
	// Spec sizes every workspace (WithDefaults).
	Spec Spec
	// KeepWorkspace stops each workspace instead of destroying it, and keeps runs/<task>/.
	KeepWorkspace bool
	// Listen opens the gateway's listener on addr; default a TCP listener on it (tests listen
	// elsewhere).
	Listen func(ctx context.Context, addr netip.AddrPort) (net.Listener, error)
	// Overrun and DownBudget: DefaultOverrun and DefaultDownBudget when zero.
	Overrun, DownBudget time.Duration
	Clock               clock.Clock
	Logger              *slog.Logger
}

// Runner runs each leased task in its own workspace VM, behind a gateway that holds every
// credential (supervise.md, "The workspace runner"). It implements runner.Runner.
type Runner struct {
	o      RunnerOptions
	logins login.Store
	clk    clock.Clock
	log    *slog.Logger

	caMu sync.Mutex
	ca   *ca.CA
}

var _ runner.Runner = (*Runner)(nil)

// NewRunner checks o and fills in the defaults.
func NewRunner(o RunnerOptions) (*Runner, error) {
	switch {
	case o.Driver == nil:
		return nil, errors.New("workspace runner: no Driver")
	case o.Pipeline == nil:
		return nil, errors.New("workspace runner: no Pipeline")
	case !filepath.IsAbs(o.StateDir):
		return nil, fmt.Errorf("workspace runner: StateDir %q must be absolute", o.StateDir)
	}
	if o.Listen == nil {
		o.Listen = func(ctx context.Context, a netip.AddrPort) (net.Listener, error) {
			var lc net.ListenConfig
			return lc.Listen(ctx, "tcp", a.String())
		}
	}
	if o.Overrun <= 0 {
		o.Overrun = DefaultOverrun
	}
	if o.DownBudget <= 0 {
		o.DownBudget = DefaultDownBudget
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Runner{o: o, logins: login.ForStateDir(o.StateDir), clk: o.Clock, log: o.Logger}, nil
}

// gatewayCA is the agent host's gateway CA, <statedir>/gateway-ca, loaded or made at first use.
func (r *Runner) gatewayCA(ctx context.Context) (*ca.CA, error) {
	r.caMu.Lock()
	defer r.caMu.Unlock()
	if r.ca == nil {
		c, err := ca.LoadOrGenerate(ctx, filepath.Join(r.o.StateDir, "gateway-ca"), r.clk)
		if err != nil {
			return nil, err
		}
		r.ca = c
	}
	return r.ca, nil
}

// Why the VM's run was stopped from outside (context.Cause).
var (
	errOverrun = errors.New("the workspace ran past the task's wall clock")
	errDown    = errors.New("factory-agent run didn't exit within its down budget after its outcome")
)

// Run implements runner.Runner.
func (r *Runner) Run(ctx context.Context, job *runner.Job, sink runner.Sink) (runner.Outcome, error) {
	id := job.TaskID()
	name, err := Name(id)
	if err != nil {
		return runner.Outcome{}, err
	}
	wall := pipeline.DefaultWallClock
	if l := job.Lease.Task.Spec.Limits; l != nil && l.MaxWallClock != "" {
		d, err := l.MaxWallClock.Parse()
		if err != nil || d <= 0 {
			return runner.Outcome{}, fmt.Errorf("limits.maxWallClock %q: invalid", l.MaxWallClock)
		}
		wall = d
	}
	start := job.StartedAt
	if start.IsZero() {
		start = r.clk.Now()
	}
	w := &wsRun{r: r, job: job, sink: sink, id: id, name: name, deadline: start.Add(wall),
		runDir: filepath.Join(r.o.StateDir, "runs", id), log: r.log.With("task", id, "workspace", name), egress: &egressLog{}}
	out := w.execute(ctx)
	return w.finish(ctx, out), nil
}

// wsRun is one job's state.
type wsRun struct {
	r        *Runner
	job      *runner.Job
	sink     runner.Sink
	id, name string
	deadline time.Time
	runDir   string
	log      *slog.Logger

	extra      []string // the model logins' secrets: redacted too
	egress     *egressLog
	gatewayOn  bool   // the gateway served: its records belong in the transcript
	transcript []byte // the VM's transcript, sanitised; nil when it has none
}

// redact redacts s with the job's secrets, the model logins and the token shapes.
func (w *wsRun) redact(s string) string { return pipeline.Redact(w.job, s, w.extra...) }

// failed is a failed Outcome with the pipeline's failed Result, its summary redacted.
func (w *wsRun) failed(reason string, err error) runner.Outcome {
	w.log.Warn("workspace run failed", "reason", reason, "err", w.redact(err.Error()))
	res := pipeline.FailedResult(w.id, reason, err, w.redact)
	return runner.Outcome{Status: runner.StatusFailed, Reason: reason, Result: &res}
}

func cancelled() runner.Outcome {
	return runner.Outcome{Status: runner.StatusFailed, Reason: runner.ReasonCancelled}
}

// execute is the lease's sequence up to the outcome; the workspace is released on every
// return.
func (w *wsRun) execute(ctx context.Context) runner.Outcome {
	h := w.job.Lease.Task.Spec.Harness
	if h == nil || h.Resolved == nil || h.Resolved.Name == "" {
		return w.failed(runner.ReasonHarnessError, errors.New("the task has no resolved harness"))
	}
	logins, acct, err := w.loadLogins(h.Resolved.Name)
	if err != nil {
		return w.failed(runner.ReasonHarnessError, err)
	}
	gwCA, err := w.r.gatewayCA(ctx)
	if err != nil {
		return w.failed(runner.ReasonHarnessError, fmt.Errorf("the gateway CA: %w", err))
	}
	if err := w.freshRunDir(); err != nil {
		return w.failed(runner.ReasonHarnessError, fmt.Errorf("the run directory: %w", err))
	}
	bundle := filepath.Join(w.runDir, "bundle")
	if err := w.r.o.Pipeline.PrepareBundle(ctx, w.job, bundle, pipeline.BundleExtras{CA: gwCA.CertPEM(), Codex: acct}); err != nil {
		if ctx.Err() != nil {
			return cancelled()
		}
		return w.failed(runner.ReasonHarnessError, err)
	}

	d := w.r.o.Driver
	defer w.release(ctx)
	if err := d.Create(ctx, w.name, w.r.o.Spec.WithDefaults()); err != nil {
		return w.stepFailed(ctx, "create", err)
	}
	netw, err := d.Start(ctx, w.name)
	if err != nil {
		return w.stepFailed(ctx, "start", err)
	}
	gw, proxy, err := w.startGateway(ctx, netw, gwCA, logins)
	if err != nil {
		return w.stepFailed(ctx, "gateway", err)
	}
	defer gw.close()
	if err := d.Put(ctx, w.name, bundle, BundleDir); err != nil {
		return w.stepFailed(ctx, "put", err)
	}
	remaining := w.deadline.Sub(w.r.clk.Now()).Truncate(time.Second)
	if remaining < time.Second {
		return w.failed(runner.ReasonLimit, errors.New("no wall clock left to start the run"))
	}
	execErr, cause := w.exec(ctx, proxy, remaining)
	switch {
	case ctx.Err() != nil:
		return cancelled()
	case errors.Is(cause, errOverrun):
		return w.failed(runner.ReasonLimit, errOverrun)
	}
	return w.collect(ctx, execErr)
}

// stepFailed fails the run at a driver step, or reports the cancel that stopped it.
func (w *wsRun) stepFailed(ctx context.Context, step string, err error) runner.Outcome {
	if ctx.Err() != nil {
		return cancelled()
	}
	return w.failed(runner.ReasonHarnessError, fmt.Errorf("workspace %s: %w", step, err))
}

// modelLogins are the logins the task's harness needs: the gateway's credential sources.
type modelLogins struct {
	anthropic func(context.Context) (gateway.AnthropicAuth, error)
	chatgpt   func(context.Context) (gateway.ChatGPTLogin, error)
}

// loadLogins checks the harness's login is in the store, keeps its secrets for redaction, and
// returns the gateway's sources (read from the store on every request) and, for codex, the
// bundle's account.
func (w *wsRun) loadLogins(name string) (modelLogins, *runbundle.CodexAccount, error) {
	store := w.r.logins
	switch name {
	case config.HarnessClaudeCode:
		tok, err := store.Claude()
		if err != nil {
			return modelLogins{}, nil, fmt.Errorf("the claude login (run `claude setup-token | factory-agent login claude`): %w", err)
		}
		w.extra = append(w.extra, tok)
		return modelLogins{anthropic: func(context.Context) (gateway.AnthropicAuth, error) {
			tok, err := store.Claude()
			return gateway.AnthropicAuth{OAuthToken: tok}, err
		}}, nil, nil
	case config.HarnessCodex:
		c, err := store.Codex()
		if err != nil {
			return modelLogins{}, nil, fmt.Errorf("the codex login (run `factory-agent login codex`): %w", err)
		}
		w.extra = append(w.extra, c.AccessToken, c.RefreshToken)
		return modelLogins{chatgpt: func(context.Context) (gateway.ChatGPTLogin, error) {
			c, err := store.Codex()
			return gateway.ChatGPTLogin{AccessToken: c.AccessToken, AccountID: c.AccountID}, err
		}}, &runbundle.CodexAccount{AccountID: c.AccountID, UserID: c.UserID, PlanType: c.PlanType}, nil
	}
	return modelLogins{}, nil, nil
}

// freshRunDir replaces a leftover runs/<task>/ (a killed supervisor's) with an empty one, 0700.
func (w *wsRun) freshRunDir() error {
	if err := os.RemoveAll(w.runDir); err != nil {
		return err
	}
	runs := filepath.Dir(w.runDir)
	if err := os.MkdirAll(runs, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(runs, 0o700); err != nil {
		return err
	}
	return os.Mkdir(w.runDir, 0o700)
}

// release destroys the workspace (or stops it with KeepWorkspace) and removes runs/<task>/
// (kept with KeepWorkspace), on a context the run's cancel doesn't end.
func (w *wsRun) release(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), releaseTimeout)
	defer cancel()
	if w.r.o.KeepWorkspace {
		if err := w.r.o.Driver.Stop(ctx, w.name); err != nil {
			w.log.Error("stopping the workspace failed", "err", w.redact(err.Error()))
		}
		w.log.Warn("keeping the workspace and its run directory (--keep-workspace)", "dir", w.runDir)
		return
	}
	if err := w.r.o.Driver.Destroy(ctx, w.name); err != nil {
		w.log.Error("destroying the workspace failed", "err", w.redact(err.Error()))
	}
	if err := os.RemoveAll(w.runDir); err != nil {
		w.log.Error("removing the run directory failed", "dir", w.runDir, "err", err)
	}
}

// runningGateway is a gateway being served.
type runningGateway struct {
	g    *gateway.Gateway
	done chan struct{}
}

func (g *runningGateway) close() {
	_ = g.g.Close()
	<-g.done
}

// startGateway serves the workspace's gateway on <Gateway>:GatewayPort, pinned to the VM's
// address, and returns it and the --proxy URL.
func (w *wsRun) startGateway(ctx context.Context, netw Network, gwCA *ca.CA, logins modelLogins) (*runningGateway, string, error) {
	if !netw.VM.IsValid() || !netw.Gateway.IsValid() {
		return nil, "", errors.New("the driver returned no addresses")
	}
	disp, err := url.Parse(w.job.Dispatcher)
	if err != nil {
		return nil, "", fmt.Errorf("the dispatcher URL: %w", err)
	}
	if w.job.AgentToken == nil {
		return nil, "", errors.New("the job has no agent token for the dispatcher route")
	}
	var runID string
	if w.job.Lease.Lease.Run != nil {
		runID = *w.job.Lease.Lease.Run
	}
	g, err := gateway.New(gateway.Config{
		TaskID: w.id, RunID: runID, Clients: []netip.Addr{netw.VM}, CA: gwCA, Git: w.job.Git,
		Dispatcher: disp, DispatcherToken: w.job.AgentToken,
		Anthropic: logins.anthropic, ChatGPT: logins.chatgpt,
		Sink: w.egress, Clock: w.r.clk, Logger: w.log,
	})
	if err != nil {
		return nil, "", err
	}
	addr := netip.AddrPortFrom(netw.Gateway, GatewayPort)
	ln, err := w.r.o.Listen(ctx, addr)
	if err != nil {
		_ = g.Close()
		return nil, "", fmt.Errorf("listen on %s: %w", addr, err)
	}
	rg := &runningGateway{g: g, done: make(chan struct{})}
	go func() {
		defer close(rg.done)
		if err := g.Serve(context.WithoutCancel(ctx), ln); err != nil {
			w.log.Error("the gateway stopped", "err", err)
		}
	}()
	w.gatewayOn = true
	return rg, "http://" + addr.String(), nil
}

// exec runs factory-agent run in the VM and relays its events until it exits. It stops the run
// (Exec's SIGTERM) when ctx ends, when the task's wall clock plus Overrun has passed, and when
// the run hasn't exited DownBudget after its outcome event; cause says which of the last two.
func (w *wsRun) exec(ctx context.Context, proxy string, remaining time.Duration) (error, error) {
	argv := []string{AgentCommand, "run", "--bundle", BundleDir, "--system-setup", "on",
		"--proxy", proxy, "--remaining", remaining.String()}
	execCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	hard := w.r.clk.NewTimerAt(w.deadline.Add(w.r.o.Overrun))
	defer hard.Stop()
	go func() {
		select {
		case <-hard.C():
			stop(errOverrun)
		case <-execCtx.Done():
		}
	}()

	pr, pw := io.Pipe()
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		w.relay(execCtx, pr, func() {
			t := w.r.clk.NewTimer(w.r.o.DownBudget)
			go func() {
				defer t.Stop()
				select {
				case <-t.C():
					stop(errDown)
				case <-execCtx.Done():
				}
			}()
		})
	}()
	stderr := &capBuffer{max: maxStderr}
	w.log.Info("factory-agent run starting in the workspace", "remaining", remaining)
	err := w.r.o.Driver.Exec(execCtx, w.name, argv, nil, pw, stderr)
	_ = pw.Close()
	<-relayed
	if err != nil {
		w.log.Debug("factory-agent run's stderr", "stderr", w.redact(stderr.String()))
	}
	return err, context.Cause(execCtx)
}

// relay decodes the VM's events into the sink: progress (redacted, cut, stamped now) and
// usage; steps go to the log at debug (VM text). outcome is called once, at the outcome
// event. It drains r to the end, so Exec never blocks on it.
func (w *wsRun) relay(ctx context.Context, r io.Reader, outcome func()) {
	defer func() { _, _ = io.Copy(io.Discard, r) }()
	dec := runbundle.NewDecoder(r)
	seen := false
	for {
		e, err := dec.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		var verr *runbundle.ValidationError
		if errors.As(err, &verr) {
			w.log.Debug("a workspace event was refused", "err", err)
			continue
		}
		if err != nil {
			w.log.Warn("reading the workspace's events failed", "err", err)
			return
		}
		switch e.Type {
		case runbundle.EventProgress:
			p := *e.Progress
			p.At = w.r.clk.Now().UTC()
			p.Message = truncate(w.redact(p.Message), harness.MaxSummary)
			w.sink.Progress(p)
		case runbundle.EventUsage:
			w.sink.Usage(runbundle.RunnerUsage(*e.Usage))
		case runbundle.EventStep:
			w.log.Debug("workspace step", "step", w.redact(e.Step.Name), "state", e.Step.State, "ms", e.Step.MS, "err", w.redact(e.Step.Error))
		case runbundle.EventOutcome:
			if !seen && ctx.Err() == nil {
				seen = true
				outcome()
			}
		}
	}
}

// collect gets the VM's out/ and turns it into the Outcome, re-checked on the host.
func (w *wsRun) collect(ctx context.Context, execErr error) runner.Outcome {
	out := filepath.Join(w.runDir, "out")
	if err := w.r.o.Driver.Get(ctx, w.name, VMOutDir, out); err != nil {
		if ctx.Err() != nil {
			return cancelled()
		}
		return w.failed(runner.ReasonHarnessError, noOutcome(execErr, fmt.Errorf("collect out/: %w", err)))
	}
	o, err := runbundle.ReadOutcome(out, w.r.clk.Now())
	if err != nil {
		return w.failed(runner.ReasonHarnessError, noOutcome(execErr, err))
	}
	if o.Transcript != "" {
		w.transcript = readTranscript(out)
	}
	return w.check(o)
}

// noOutcome is the error of a run whose outcome is missing or refused, with the exit status.
func noOutcome(execErr, err error) error {
	if execErr != nil {
		return fmt.Errorf("the workspace returned no valid outcome (factory-agent run: %w): %w", execErr, err)
	}
	return fmt.Errorf("the workspace returned no valid outcome: %w", err)
}

// check re-validates the VM's outcome (ReadOutcome checked its shape and clamped its throttle):
// a throttled or cancelled outcome carries no Result; any other needs one for this task, with
// the outcome's status, valid against the schema the host fetched (the bundle's) and the task's
// outcomes.
func (w *wsRun) check(o runbundle.Outcome) runner.Outcome {
	out := o.Runner()
	if out.Status == runner.StatusThrottled || out.Reason == runner.ReasonCancelled {
		out.Result = nil
		return out
	}
	res := out.Result
	switch {
	case res == nil:
		return w.failed(runner.ReasonHarnessError, fmt.Errorf("the workspace's %s outcome has no result", out.Status))
	case res.Task != w.id:
		return w.failed(runner.ReasonInvalidResult, errors.New("the workspace's result is another task's"))
	case string(res.Status) != out.Status:
		return w.failed(runner.ReasonInvalidResult, errors.New("the workspace's result status differs from its outcome's"))
	}
	doc, err := json.Marshal(res)
	if err != nil {
		return w.failed(runner.ReasonInvalidResult, fmt.Errorf("the workspace's result: %w", err))
	}
	b, err := runbundle.Read(filepath.Join(w.runDir, "bundle"))
	if err != nil {
		return w.failed(runner.ReasonHarnessError, fmt.Errorf("the run bundle: %w", err))
	}
	t := w.job.Lease.Task
	var outcomes []string
	if t.Spec.Output != nil {
		outcomes = t.Spec.Output.Outcomes
	}
	if problems := pipeline.ValidateResult(w.id, t.Spec.TaskSpec.Name, b.OutputSchema, outcomes, doc); len(problems) > 0 {
		return w.failed(runner.ReasonInvalidResult, fmt.Errorf("the workspace's result is invalid: %s", strings.Join(problems, "; ")))
	}
	return out
}

// finish uploads the transcript (the VM's, sanitised, then the gateway's egress records), unless
// ctx is done (the worker reports nothing then), and returns out with its reference.
func (w *wsRun) finish(ctx context.Context, out runner.Outcome) runner.Outcome {
	if ctx.Err() != nil {
		return out
	}
	var parts [][]byte
	if w.transcript != nil {
		parts = append(parts, w.transcript)
	}
	if w.gatewayOn {
		if p := w.egress.part(); len(p) > 0 {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return out
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadTimeout)
	defer cancel()
	ref, err := w.r.o.Pipeline.StoreTranscript(uctx, w.job, parts, w.extra...)
	if err != nil {
		w.log.Warn("transcript not uploaded", "err", w.redact(err.Error()))
		return out
	}
	if ref != "" {
		w.log.Info("transcript uploaded")
	}
	out.TranscriptRef = ref
	return out
}

// TranscriptNote is the transcript line the runner writes about the VM's transcript: lines it
// dropped (over the line cap, or a factory.egress line the VM forged) or why it has none.
const TranscriptNote = "factory.workspace-transcript"

// readTranscript reads the VM's out/transcript.jsonl.gz (untrusted): a regular file inside
// out/, gunzipped up to maxTranscriptRaw, without lines over maxTranscriptLine and without
// factory.egress lines (only the gateway writes those). What it dropped, or why it read
// nothing, is a TranscriptNote line at the end. The redaction and the cap are StoreTranscript's.
func readTranscript(outDir string) []byte {
	note := func(m map[string]any) []byte {
		m["type"] = TranscriptNote
		b, _ := json.Marshal(m)
		return append(b, '\n')
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return note(map[string]any{"error": "out/ can't be opened"})
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(runbundle.TranscriptFile)
	if err != nil || !fi.Mode().IsRegular() {
		return note(map[string]any{"error": "the transcript is not a regular file"})
	}
	f, err := root.Open(runbundle.TranscriptFile)
	if err != nil {
		return note(map[string]any{"error": "the transcript can't be opened"})
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return note(map[string]any{"error": "the transcript is not gzip"})
	}
	br := bufio.NewReaderSize(io.LimitReader(zr, maxTranscriptRaw), maxTranscriptLine)
	var buf bytes.Buffer
	long, forged := 0, 0
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			long++
			if err != nil {
				break
			}
			continue
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &head) == nil && head.Type == gateway.RecordType {
				forged++
			} else {
				buf.Write(line)
				if line[len(line)-1] != '\n' {
					buf.WriteByte('\n')
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				m := map[string]any{"error": "the transcript is truncated or corrupt"}
				if long+forged > 0 {
					m["droppedLines"], m["forgedEgress"] = long, forged
				}
				buf.Write(note(m))
				return buf.Bytes()
			}
			break
		}
	}
	if long+forged > 0 {
		buf.Write(note(map[string]any{"droppedLines": long, "forgedEgress": forged}))
	}
	return buf.Bytes()
}

// egressLog is the gateway's Sink: its records as factory.egress transcript lines, in order,
// up to maxEgressRecords.
type egressLog struct {
	mu      sync.Mutex
	lines   [][]byte
	dropped int
}

var _ gateway.Sink = (*egressLog)(nil)

// Egress implements gateway.Sink.
func (l *egressLog) Egress(r gateway.Record) {
	b, err := json.Marshal(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil || len(l.lines) >= maxEgressRecords {
		l.dropped++
		return
	}
	l.lines = append(l.lines, append(b, '\n'))
}

// part is the records as one transcript part, with a count of the dropped ones.
func (l *egressLog) part() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := bytes.Join(l.lines, nil)
	if l.dropped > 0 {
		b = append(b, []byte(`{"type":"factory.egress-dropped","dropped":`+strconv.Itoa(l.dropped)+"}\n")...)
	}
	return b
}

// capBuffer keeps the first max bytes written to it.
type capBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// truncate cuts s to n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

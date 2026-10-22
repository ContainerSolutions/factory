package vmrun

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// factory-agent run's exit codes (workspace-run.md §Exit codes and signals).
const (
	// ExitOK: an outcome was written, whatever its status.
	ExitOK = 0
	// ExitUsage: the command line is wrong.
	ExitUsage = 2
	// ExitBundle: the run bundle is invalid; no event and no outcome.
	ExitBundle = 3
	// ExitOutcome: the outcome couldn't be written; the supervisor has only the events.
	ExitOutcome = 4
)

// The flags' defaults.
const (
	DefaultBundle   = "/run/factory"
	DefaultWork     = "/work"
	DefaultStateDir = "/var/lib/factory-agent/run"
)

// setupFailReason is the reason of a run whose system setup failed. Seam (P1 phase 3, PR 7):
// the environment runner adds runner.ReasonEnvironmentError, which this becomes.
const setupFailReason = runner.ReasonHarnessError

// maxSummary bounds the Result summary of a failure vmrun reports itself (the pipeline's).
const maxSummary = 500

// deps are mainWith's injectable dependencies; the zero value is production (Main).
type deps struct {
	// system is the system setup's template; StateDir and WorkDir come from the flags.
	system System
	// adapters builds the task's adapters; default Adapters.
	adapters func(*runbundle.Bundle, AdapterOptions) (map[string]harness.Adapter, error)
	// agentBinary is the workspace's `factory` CLI; default os.Executable (pipeline.New).
	agentBinary string
	// clock times the run and stamps the events; default clock.Real.
	clock clock.Clock
	// signals delivers SIGTERM and SIGINT; default the process's.
	signals <-chan os.Signal
}

// Main is `factory-agent run` (workspace-run.md §factory-agent run): it reads the run bundle,
// sets the VM up for the gateway, and runs the task's pipeline in the workspace with every
// credential outside the VM. stdout is the event stream (runbundle.Event lines) and stderr
// the log. It returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return mainWith(ctx, args, stdout, stderr, deps{})
}

// options are the parsed flags.
type options struct {
	proxy             *url.URL
	bundle, work      string
	state             string
	remaining         time.Duration
	downTimeout       time.Duration
	setup             SystemSetup
	logLevel, logForm string
	bins              harnessBins
}

func mainWith(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	o, code := parseFlags(args, stderr)
	if o == nil {
		return code
	}
	log, err := newLogger(stderr, o.logLevel, o.logForm)
	if err != nil {
		fmt.Fprintln(stderr, "factory-agent run:", err)
		return ExitUsage
	}
	if d.clock == nil {
		d.clock = clock.Real{}
	}
	if d.adapters == nil {
		d.adapters = Adapters
	}
	b, err := runbundle.Read(o.bundle)
	if err != nil {
		log.Error("the run bundle is invalid", "bundle", o.bundle, "err", err)
		return ExitBundle
	}
	log = log.With("task", b.TaskID(), "run", b.Manifest.RunID)
	ev := &events{enc: runbundle.NewEncoder(stdout, d.clock.Now), log: log}
	ev.start(b.Manifest)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigs := d.signals
	if sigs == nil {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		defer signal.Stop(ch)
		sigs = ch
	}
	stop := watchSignals(sigs, cancel, log)
	defer stop()

	r := &vm{o: o, d: d, b: b, log: log, ev: ev, out: filepath.Join(o.work, runbundle.OutDir)}
	return r.run(runCtx)
}

// vm is one run in the VM.
type vm struct {
	o   *options
	d   deps
	b   *runbundle.Bundle
	log *slog.Logger
	ev  *events
	out string // <work>/out

	transcript *transcriptFile
	reported   bool // the outcome was reported
	code       int
}

// run is the sequence after the bundle (workspace-run.md §Sequence, steps 3 to 7).
func (r *vm) run(ctx context.Context) int {
	sys := r.d.system
	sys.StateDir, sys.WorkDir = r.o.state, r.o.work
	if sys.Logger == nil {
		sys.Logger = r.log
	}
	if sys.Clock == nil {
		sys.Clock = r.d.clock
	}
	env, err := sys.Setup(ctx, r.b, r.o.proxy, r.o.setup)
	if err != nil {
		return r.fail(ctx, setupFailReason, err)
	}
	job := JobFromBundle(r.b, r.d.clock.Now(), r.o.remaining)
	adapters, err := r.d.adapters(r.b, AdapterOptions{Binaries: r.o.bins, Clock: r.d.clock})
	if err != nil {
		return r.fail(ctx, runner.ReasonHarnessError, fmt.Errorf("adapters: %w", err))
	}
	r.transcript = &transcriptFile{FileTranscript: pipeline.FileTranscript{Path: filepath.Join(r.out, runbundle.TranscriptFile)}}
	// Seam (P1 phase 3, PR 7): the environment runner becomes Options.Environment here, its
	// Down bounded by --down-timeout and the signals (downGuard).
	p, err := pipeline.New(pipeline.Options{
		Root: r.o.work, GitAuth: pipeline.GitAuthGateway, Env: env, Adapters: adapters, AgentBinary: r.d.agentBinary,
		Prompts: pipeline.BundlePrompts{B: r.b}, Schemas: pipeline.BundleSchemas{B: r.b}, Skills: pipeline.BundleSkills(r.b),
		Transcripts: r.transcript, Clock: r.d.clock, Logger: r.log,
		OnStep: r.ev.step, Finished: r.onFinished,
	})
	if err != nil {
		return r.fail(ctx, runner.ReasonHarnessError, fmt.Errorf("pipeline: %w", err))
	}
	if _, err := p.Run(ctx, job, r.ev); err != nil {
		// Before any step (an invalid maxWallClock): Finished wasn't called.
		return r.fail(ctx, runner.ReasonHarnessError, err)
	}
	if !r.reported {
		return r.fail(ctx, runner.ReasonHarnessError, errors.New("the pipeline ended without an outcome"))
	}
	return r.code
}

// onFinished is pipeline.Options.Finished: outcome.json, then the outcome event, before the
// pipeline's cleanup. The throttle is the VM's own: the host clamps it (runbundle.ReadOutcome).
func (r *vm) onFinished(o runner.Outcome) {
	transcript := ""
	if r.transcript.stored {
		transcript = runbundle.TranscriptFile
	}
	r.report(runbundle.OutcomeOf(o, transcript))
}

// fail reports a run that ended outside the pipeline: failed with reason and a Result whose
// summary is err (the pipeline's shape), or cancelled when a signal cancelled ctx.
func (r *vm) fail(ctx context.Context, reason string, err error) int {
	if ctx.Err() != nil {
		reason = runner.ReasonCancelled
	}
	r.log.Error("the run failed", "reason", reason, "err", err)
	o := runbundle.Outcome{Status: runner.StatusFailed, Reason: reason}
	if reason != runner.ReasonCancelled {
		o.Result = &v1alpha1.Result{Task: r.b.TaskID(), Status: v1alpha1.ResultFailed, Summary: truncate(reason+": "+err.Error(), maxSummary)}
	}
	r.report(o)
	return r.code
}

// report writes outcome.json and sends the outcome event, once. A failed write is exit 4;
// the event is still sent.
func (r *vm) report(o runbundle.Outcome) {
	if r.reported {
		return
	}
	r.reported = true
	if err := runbundle.WriteOutcome(r.out, o); err != nil {
		r.log.Error("the outcome wasn't written", "err", err)
		r.code = ExitOutcome
	}
	r.ev.outcome(o)
}

// transcriptFile is the pipeline's FileTranscript, recording that the transcript was stored
// (the outcome then names it). The pipeline stores it and calls Finished in one goroutine.
type transcriptFile struct {
	pipeline.FileTranscript
	stored bool
}

// Store implements pipeline.TranscriptStore.
func (t *transcriptFile) Store(ctx context.Context, job *runner.Job, gz []byte) (string, error) {
	ref, err := t.FileTranscript.Store(ctx, job, gz)
	t.stored = err == nil
	return ref, err
}

// watchSignals cancels the run on the first SIGTERM or SIGINT: the pipeline ends
// failed{cancelled} and the outcome is still written. A second one is logged. Seam (P1
// phase 3, PR 7): with an environment, it skips (or cancels) the environment's down. stop
// ends the watch.
func watchSignals(sigs <-chan os.Signal, cancel context.CancelFunc, log *slog.Logger) (stop func()) {
	done := make(chan struct{})
	go func() {
		n := 0
		for {
			select {
			case <-done:
				return
			case s := <-sigs:
				n++
				if n == 1 {
					log.Warn("cancelling the run", "signal", s.String())
					cancel()
					continue
				}
				log.Warn("a second signal: the run is already cancelled", "signal", s.String())
			}
		}
	}()
	return func() { close(done) }
}

func parseFlags(args []string, stderr io.Writer) (*options, int) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := &options{bins: harnessBins{}}
	proxy := fs.String("proxy", "", "the gateway, http://<host>:<port> (required)")
	fs.StringVar(&o.bundle, "bundle", DefaultBundle, "the run bundle")
	fs.StringVar(&o.work, "work", DefaultWork, "the workspace root: empty or absent")
	fs.StringVar(&o.state, "state-dir", DefaultStateDir, "what the runtime writes outside the workspace")
	fs.DurationVar(&o.remaining, "remaining", 0, "what is left of the task's wall clock (0: all of limits.maxWallClock)")
	setup := fs.String("system-setup", string(SetupOff), "on (a VM: the supervisor passes it) or off: the privileged system setup")
	fs.DurationVar(&o.downTimeout, "down-timeout", pipeline.StackDownTimeout, "bounds the environment's down")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&o.logForm, "log-format", "json", "json or text")
	fs.Var(o.bins, "harness-bin", "") // hidden: <harness>=<path>, for tests
	fs.Usage = func() { usage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, ExitOK
		}
		return nil, ExitUsage
	}
	err := o.check(fs, *proxy, *setup)
	if err != nil {
		fmt.Fprintln(stderr, "factory-agent run:", err)
		return nil, ExitUsage
	}
	return o, ExitOK
}

func (o *options) check(fs *flag.FlagSet, proxy, setup string) error {
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %v", fs.Args())
	}
	if proxy == "" {
		return errors.New("--proxy is required")
	}
	var err error
	if o.proxy, err = ParseProxy(proxy); err != nil {
		return err
	}
	if o.setup, err = ParseSystemSetup(setup); err != nil {
		return err
	}
	for name, p := range map[string]string{"--bundle": o.bundle, "--work": o.work, "--state-dir": o.state} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%s %q must be absolute", name, p)
		}
	}
	o.bundle, o.work, o.state = filepath.Clean(o.bundle), filepath.Clean(o.work), filepath.Clean(o.state)
	switch {
	case within(o.work, o.state) || within(o.state, o.work):
		return errors.New("--work and --state-dir must not overlap: the workspace starts empty")
	case within(o.work, o.bundle):
		return errors.New("--bundle must not be in --work")
	case o.remaining < 0:
		return fmt.Errorf("--remaining %s must not be negative", o.remaining)
	case o.downTimeout <= 0:
		return fmt.Errorf("--down-timeout %s must be positive", o.downTimeout)
	}
	return nil
}

// within reports whether p is dir or under it (both clean and absolute).
func within(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// usage prints the flags but the hidden --harness-bin.
func usage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintln(w, "usage: factory-agent run --proxy http://<gateway>:<port> [flags]")
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "harness-bin" {
			return
		}
		fmt.Fprintf(w, "  --%s\n    \t%s (default %q)\n", f.Name, f.Usage, f.DefValue)
	})
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("--log-format %q must be json or text", format)
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

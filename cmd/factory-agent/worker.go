package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/codex"
	"github.com/compliance-framework/factory/internal/agent/owner"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/probe"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/agent/worker"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/version"
)

// workerDeps are runWorker's injectable dependencies; the zero value is production.
type workerDeps struct {
	Probe  probe.Options
	Config config.Options
	// Signals delivers SIGTERM/SIGINT (drain, then abort) and SIGHUP (reload lending).
	// Default: the process's signals.
	Signals <-chan os.Signal
	// Owner replaces the device-flow owner login (tests).
	Owner owner.TokenSource
	// HTTPClient calls the dispatcher and GitHub.
	HTTPClient *http.Client
	// NewRunner builds the Runner. Default: the run pipeline, or the stub with
	// --stub-result.
	NewRunner func(cfg *config.Config, o runnerOptions) (runner.Runner, error)
}

// runnerOptions are the worker flags and dependencies the Runner is built with.
type runnerOptions struct {
	// StubResult (development): answer every task with this Result instead of running it.
	StubResult string
	// KeepWorkspace keeps each run's workspace (debugging).
	KeepWorkspace bool
	Logger        *slog.Logger
	// Home expands ~ (the skills cache).
	Home func() (string, error)
}

// harnessAdapters builds each harness's adapter from its local config, one line per
// harness: AGT-S1-T03 registers codex and AGT-S1-T04 claude-code.
var harnessAdapters = map[string]func(config.Harness) harness.Adapter{
	config.HarnessCodex: func(h config.Harness) harness.Adapter { return codex.New(codex.Options{Config: h}) },
}

// skillsCache is where skills checkouts are cached by commit (run-pipeline.md §Steps 5).
const skillsCache = "~/.factory-agent/cache/ccf-skills"

func workerCommand() command {
	return command{
		name:    "worker",
		summary: "register with the dispatcher and run leased tasks until SIGTERM",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runWorker(ctx, args, stderr, workerDeps{Probe: probeOptions()})
		},
	}
}

// newRunner is the Runner `factory-agent worker` uses: the run pipeline with an adapter for
// every configured harness, or with --stub-result (development) a stub that answers every
// task with that Result.
func newRunner(cfg *config.Config, o runnerOptions) (runner.Runner, error) {
	if o.StubResult != "" {
		b, err := os.ReadFile(o.StubResult)
		if err != nil {
			return nil, fmt.Errorf("--stub-result: %w", err)
		}
		res, err := v1alpha1.DecodeResult(b)
		if err != nil {
			return nil, fmt.Errorf("--stub-result %s: %w", o.StubResult, err)
		}
		return &runner.Stub{Outcome: runner.Outcome{Result: res}}, nil
	}
	adapters := map[string]harness.Adapter{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Harnesses)) {
		mk, ok := harnessAdapters[name]
		if !ok {
			return nil, fmt.Errorf("no harness adapter for %s yet (AGT-S1-T03 codex, AGT-S1-T04 claude-code); pass --stub-result <result.json> to run against a development dispatcher", name)
		}
		adapters[name] = mk(cfg.Harnesses[name])
	}
	home := o.Home
	if home == nil {
		home = os.UserHomeDir
	}
	cache, err := config.ExpandPath(skillsCache, home)
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{
		WorkRoot: cfg.WorkRoot, Adapters: adapters, SkillsCache: cache,
		Logger: o.Logger, KeepWorkspace: o.KeepWorkspace,
	})
}

func runWorker(ctx context.Context, args []string, stderr io.Writer, d workerDeps) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "the agent config file")
	stubResult := fs.String("stub-result", "", "development only: answer every task with this Result JSON instead of running it")
	keepWorkspace := fs.Bool("keep-workspace", false, "debugging: keep each run's workspace instead of removing it")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "factory-agent worker: unexpected arguments %v\n", fs.Args())
		return 2
	}
	log, err := newLogger(stderr, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(stderr, "factory-agent worker:", err)
		return 2
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{}
	}
	if d.NewRunner == nil {
		d.NewRunner = newRunner
	}

	co := d.Config
	co.Warn = func(s string) { log.Warn(s) }
	cfg, err := config.Load(*path, co)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	rep := probe.Run(ctx, cfg, d.Probe)
	for _, dr := range rep.Dropped {
		log.Warn("not advertised: its probe failed", "kind", dr.Kind, "name", dr.Name, "reason", dr.Reason)
	}
	if ok, why := rep.Ready(); !ok {
		log.Error("the worker can't start", "why", strings.Join(why, "; "))
		return 1
	}
	rn, err := d.NewRunner(cfg, runnerOptions{StubResult: *stubResult, KeepWorkspace: *keepWorkspace, Logger: log, Home: d.Config.Home})
	if err != nil {
		log.Error("runner", "err", err)
		return 1
	}
	if *stubResult != "" {
		log.Warn("development mode: every leased task is answered with --stub-result, nothing runs", "file", *stubResult)
	}

	src := d.Owner
	if src == nil {
		plain, err := apiclient.NewClientWithResponses(worker.APIBase(cfg.Dispatcher), apiclient.WithHTTPClient(d.HTTPClient))
		if err != nil {
			log.Error("dispatcher client", "err", err)
			return 1
		}
		src = &owner.DeviceFlow{
			API: plain, GitHubURL: cfg.GitHubURL, HTTPClient: &http.Client{Timeout: 30 * time.Second},
			Prompt: func(uri, code string, expires time.Duration) {
				fmt.Fprintf(stderr, "\nTo register this agent, open %s and enter the code %s (it expires in %s).\n\n", uri, code, expires)
			},
		}
	}
	w, err := worker.New(worker.Options{
		Dispatcher: cfg.Dispatcher, Pool: cfg.Pool, Host: cfg.Host,
		AgentVersion: version.Version, Capabilities: rep.Capabilities, Lending: cfg.Lending,
		JoinToken: func() (string, error) { return config.ReadSecretFile(cfg.JoinTokenFile) },
		Owner:     src, Runner: rn, HTTPClient: d.HTTPClient, Logger: log,
	})
	if err != nil {
		log.Error("worker", "err", err)
		return 1
	}

	sigs := d.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt, syscall.SIGHUP)
		defer signal.Stop(ch)
		sigs = ch
	}
	runCtx, abort := context.WithCancel(ctx)
	defer abort()
	go handleSignals(runCtx, sigs, w, abort, *path, co, log)

	log.Info("factory-agent worker starting", "version", version.Version, "dispatcher", cfg.Dispatcher, "pool", cfg.Pool, "host", cfg.Host)
	if err := w.Run(runCtx); err != nil {
		log.Error("worker stopped", "err", err)
		return 1
	}
	log.Info("worker stopped")
	return 0
}

// handleSignals: the first SIGTERM/SIGINT drains, the second aborts (running tasks are
// abandoned with shutdown); SIGHUP re-reads the config and re-sends lending.
func handleSignals(ctx context.Context, sigs <-chan os.Signal, w *worker.Worker, abort context.CancelFunc,
	path string, co config.Options, log *slog.Logger) {
	draining := false
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-sigs:
			if s == syscall.SIGHUP {
				cfg, err := config.Load(path, co)
				if err != nil {
					log.Error("SIGHUP: config not reloaded", "err", err)
					continue
				}
				if err := w.SetLending(ctx, cfg.Lending); err != nil {
					log.Error("SIGHUP: lending not updated", "err", err)
					continue
				}
				log.Info("SIGHUP: lending reloaded (other config changes need a restart)")
				continue
			}
			if !draining {
				draining = true
				log.Info("draining: finishing running tasks; send the signal again to abort them", "signal", s.String())
				w.Drain()
				continue
			}
			log.Warn("aborting: running tasks are abandoned", "signal", s.String())
			abort()
			return
		}
	}
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("--log-format %q must be text or json", format)
}

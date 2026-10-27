package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/enroll"
	"github.com/ContainerSolutions/factory/internal/agent/owner"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
	"github.com/ContainerSolutions/factory/internal/agent/probe"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/agent/worker"
	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/version"
)

// workerDeps are the worker loop's injectable dependencies (runAgentLoop); the zero value is
// production.
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
}

// runnerOptions are the dependencies the host pipeline is built with.
type runnerOptions struct {
	Logger *slog.Logger
	// Home expands ~ (the skills cache).
	Home func() (string, error)
}

// skillsCache is where skills checkouts are cached by commit (run-pipeline.md §Steps 5).
const skillsCache = "~/.factory-agent/cache/ccf-skills"

// hostPipelineOptions are the host run pipeline's options supervise builds its runner on: the
// work root and the skills cache. The host pipeline only builds run bundles, so it has no
// harness adapters: the harnesses run in the VM (vmrun's adapters).
func hostPipelineOptions(cfg *config.Config, o runnerOptions) (pipeline.Options, error) {
	home := o.Home
	if home == nil {
		home = os.UserHomeDir
	}
	cache, err := config.ExpandPath(skillsCache, home)
	if err != nil {
		return pipeline.Options{}, err
	}
	return pipeline.Options{WorkRoot: cfg.WorkRoot, SkillsCache: cache, Logger: o.Logger}, nil
}

// agentLoop is what runAgentLoop needs of its command (supervise).
type agentLoop struct {
	// name is the command's, in the logs.
	name string
	// path is the config file.
	path string
	// stateDir is supervise's state dir (absolute): the enrolled agent's refresh token.
	stateDir string
	log      *slog.Logger
	// newRunner builds the Runner once the config is loaded and probed.
	newRunner func(ctx context.Context, cfg *config.Config) (runner.Runner, error)
	// lending adjusts the config's lending, at start and on SIGHUP; nil keeps it.
	lending func(v1alpha1.Lending) v1alpha1.Lending
	// reload runs on SIGHUP once the config is re-read, before lending: what lending depends on
	// besides the config (supervise's kata cap re-reads the physical memory); nil does nothing.
	reload func()
}

// runAgentLoop is supervise's worker loop once its flags are parsed: load the config, probe,
// build the Runner, register, and serve leases until a drain or an abort.
func runAgentLoop(ctx context.Context, stderr io.Writer, d workerDeps, l agentLoop) int {
	log := l.log
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{}
	}
	if l.lending == nil {
		l.lending = func(x v1alpha1.Lending) v1alpha1.Lending { return x }
	}
	if l.reload == nil {
		l.reload = func() {}
	}

	co := d.Config
	co.Warn = func(s string) { log.Warn(s) }
	cfg, err := config.Load(l.path, co)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	rep := probe.Run(ctx, cfg, d.Probe)
	for _, dr := range rep.Dropped {
		log.Warn("not advertised: its probe failed", "kind", dr.Kind, "name", dr.Name, "reason", dr.Reason)
	}
	if ok, why := rep.Ready(); !ok {
		log.Error("the "+l.name+" can't start", "why", strings.Join(why, "; "))
		return 1
	}
	rn, err := l.newRunner(ctx, cfg)
	if err != nil {
		log.Error("runner", "err", err)
		return 1
	}

	o := worker.Options{
		Dispatcher: cfg.Dispatcher, Pool: cfg.Pool, Host: cfg.Host,
		AgentVersion: version.Version, Capabilities: rep.Capabilities, Lending: l.lending(cfg.Lending),
		Runner: rn, HTTPClient: d.HTTPClient, Logger: log,
	}
	// Enrolled (P1 §6): a refresh token in the state dir, or an enrollment token to exchange.
	if enr := (enroll.Files{StateDir: l.stateDir, TokenFile: cfg.EnrollmentTokenFile}); cfg.EnrollmentTokenFile != "" || enr.Enrolled() {
		o.Enrollment = enr
	} else if o.Owner = d.Owner; o.Owner == nil {
		plain, err := apiclient.NewClientWithResponses(worker.APIBase(cfg.Dispatcher), apiclient.WithHTTPClient(d.HTTPClient))
		if err != nil {
			log.Error("dispatcher client", "err", err)
			return 1
		}
		o.Owner = &owner.DeviceFlow{
			API: plain, GitHubURL: cfg.GitHubURL, HTTPClient: &http.Client{Timeout: 30 * time.Second},
			Prompt: func(uri, code string, expires time.Duration) {
				fmt.Fprintf(stderr, "\nTo register this agent, open %s and enter the code %s (it expires in %s).\n\n", uri, code, expires)
			},
		}
	}
	if o.Enrollment == nil {
		o.JoinToken = func() (string, error) { return config.ReadSecretFile(cfg.JoinTokenFile) }
	}
	w, err := worker.New(o)
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
	go handleSignals(runCtx, sigs, w, abort, l, co, log)

	log.Info("factory-agent "+l.name+" starting", "version", version.Version, "dispatcher", cfg.Dispatcher, "pool", cfg.Pool, "host", cfg.Host)
	if err := w.Run(runCtx); err != nil {
		log.Error(l.name+" stopped", "err", err)
		return 1
	}
	log.Info(l.name + " stopped")
	return 0
}

// handleSignals: the first SIGTERM/SIGINT drains, the second aborts (running tasks are
// abandoned with shutdown); SIGHUP re-reads the config, runs l.reload and re-sends lending.
func handleSignals(ctx context.Context, sigs <-chan os.Signal, w *worker.Worker, abort context.CancelFunc,
	l agentLoop, co config.Options, log *slog.Logger) {
	draining := false
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-sigs:
			if s == syscall.SIGHUP {
				cfg, err := config.Load(l.path, co)
				if err != nil {
					log.Error("SIGHUP: config not reloaded", "err", err)
					continue
				}
				l.reload()
				if err := w.SetLending(ctx, l.lending(cfg.Lending)); err != nil {
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

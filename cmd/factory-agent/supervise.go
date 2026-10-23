package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/host"
	"github.com/compliance-framework/factory/internal/agent/login"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/probe"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/agent/workspace"
	"github.com/compliance-framework/factory/internal/agent/workspace/tart"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// superviseDeps are runSupervise's injectable dependencies; the zero value is production.
type superviseDeps struct {
	// Worker is the worker loop's (Probe: nil is supervise's own, the login store's).
	Worker workerDeps
	// Host is host check's environment; its zero fields get the production defaults.
	Host host.Env
	// HostCheck replaces host check's steps (tests); default host.Steps and host.Check.
	HostCheck func(ctx context.Context, e *host.Env) ([]host.Status, error)
	// NewDriver builds the workspace driver; default tart.New.
	NewDriver func(tart.Config) workspace.Driver
	// Listen opens each gateway's listener (tests); default TCP on the address.
	Listen func(ctx context.Context, addr netip.AddrPort) (net.Listener, error)
}

func superviseCommand() command {
	return command{
		name:    "supervise",
		summary: "like worker, but run each leased task in its own VM workspace behind a gateway",
		run: func(ctx context.Context, args []string, _, stderr io.Writer) int {
			return runSupervise(ctx, args, stderr, superviseDeps{})
		},
	}
}

// errHostCheck is a host that host check doesn't pass.
var errHostCheck = errors.New("host check failed: run `factory-agent host init --driver tart`")

func runSupervise(ctx context.Context, args []string, stderr io.Writer, d superviseDeps) int {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "the agent config file")
	keepWorkspace := fs.Bool("keep-workspace", false, "debugging: stop each workspace VM and keep it, and its run directory, instead of destroying them")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	driver := fs.String("driver", config.DriverTart, "the workspace driver: tart")
	warmBase := fs.String("warm-base", host.WarmBaseVM, "the VM each workspace is cloned from (host init builds it)")
	stateDir := fs.String("state-dir", "", "the agent's state dir: logins, runs and the gateway CA (default ~/Library/Application Support/factory-agent)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "factory-agent supervise: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if *driver != config.DriverTart {
		fmt.Fprintf(stderr, "factory-agent supervise: --driver %q: only tart is supported\n", *driver)
		return 2
	}
	log, err := newLogger(stderr, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(stderr, "factory-agent supervise:", err)
		return 2
	}
	e := d.Host
	if err := e.Defaults(); err != nil {
		log.Error("host environment", "err", err)
		return 1
	}
	if *stateDir == "" {
		*stateDir = e.StateDir()
	}
	if *stateDir, err = filepath.Abs(*stateDir); err != nil {
		log.Error("--state-dir", "err", err)
		return 1
	}
	if d.Worker.Probe.Harnesses == nil {
		d.Worker.Probe = loginProbe(login.ForStateDir(*stateDir))
	}
	s := &supervisor{d: d, e: &e, stderr: stderr, log: log, stateDir: *stateDir, warmBase: *warmBase, keep: *keepWorkspace, unlock: func() {}}
	defer func() { s.unlock() }()
	return runAgentLoop(ctx, stderr, d.Worker, agentLoop{
		name: "supervisor", path: *path, log: log, newRunner: s.newRunner,
		// concurrency 1 (supervise.md): one workspace VM at a time.
		lending: func(l v1alpha1.Lending) v1alpha1.Lending { l.MaxConcurrent = 1; return l },
	})
}

// loginProbe offers each configured harness whose model login is in the store: the harnesses
// run in the VM, so the host has no binary to probe, and a task without its login would fail.
func loginProbe(store login.Store) probe.Options {
	logins := map[string]func() error{
		config.HarnessClaudeCode: func() error { _, err := store.Claude(); return err },
		config.HarnessCodex:      func() error { _, err := store.Codex(); return err },
	}
	o := probe.Options{Harnesses: map[string]probe.Prober{}}
	for name, has := range logins {
		o.Harnesses[name] = loginProber{name: name, has: has}
	}
	return o
}

type loginProber struct {
	name string
	has  func() error
}

func (p loginProber) Probe(ctx context.Context, h config.Harness) (v1alpha1.HarnessOffer, error) {
	if err := p.has(); err != nil {
		which := strings.TrimSuffix(p.name, "-code")
		return v1alpha1.HarnessOffer{}, fmt.Errorf("no %s login (run `factory-agent login %s`): %w", which, which, err)
	}
	return probe.Configured{Name: p.name}.Probe(ctx, h)
}

// supervisor builds supervise's Runner.
type supervisor struct {
	d        superviseDeps
	e        *host.Env
	stderr   io.Writer
	log      *slog.Logger
	stateDir string
	warmBase string
	keep     bool
	// unlock releases the supervise lock newRunner takes, if it did.
	unlock func()
}

// newRunner gates on host check, takes the supervise lock, destroys the workspaces a killed
// supervisor left, and builds the workspace runner on the host pipeline.
func (s *supervisor) newRunner(ctx context.Context, cfg *config.Config) (runner.Runner, error) {
	w := config.Workspace{}
	if cfg.Workspace != nil {
		w = *cfg.Workspace
	}
	w.Driver = config.DriverTart
	s.e.Workspace = w.WithDefaults(s.e.GOOS)
	check := s.d.HostCheck
	if check == nil {
		check = func(ctx context.Context, e *host.Env) ([]host.Status, error) {
			steps, err := host.Steps(e)
			if err != nil {
				return nil, err
			}
			return host.Check(ctx, steps), nil
		}
	}
	sts, err := check(ctx, s.e)
	if err != nil {
		return nil, fmt.Errorf("host check: %w", err)
	}
	if !host.AllOK(sts) {
		var bad []host.Status
		for _, st := range sts {
			if st.State != host.StateOK && st.State != host.StateSkipped {
				bad = append(bad, st)
			}
		}
		_ = printStatuses(s.stderr, bad, false)
		return nil, errHostCheck
	}
	warnSkipped(s.stderr, sts)
	// One supervisor per user (supervise.md): the orphan cleanup would destroy another's VM.
	// The lock is in the host's state dir, which host check made sure exists.
	unlock, err := host.LockSupervise(s.e)
	if err != nil {
		return nil, err
	}
	s.unlock = unlock

	newDriver := s.d.NewDriver
	if newDriver == nil {
		newDriver = func(c tart.Config) workspace.Driver { return tart.New(c) }
	}
	tartLog := io.Discard
	if s.log.Enabled(ctx, slog.LevelDebug) {
		tartLog = s.stderr
	}
	drv := newDriver(tart.Config{Bin: s.e.TartBin(ctx), WarmBase: s.warmBase, Log: tartLog})
	if err := destroyOrphans(ctx, drv, s.log); err != nil {
		return nil, err
	}

	po, err := hostPipelineOptions(cfg, runnerOptions{Logger: s.log, Home: s.d.Worker.Config.Home})
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(po)
	if err != nil {
		return nil, err
	}
	return workspace.NewRunner(workspace.RunnerOptions{
		Driver: drv, Pipeline: p, StateDir: s.stateDir, KeepWorkspace: s.keep,
		Listen: s.d.Listen, Logger: s.log,
	})
}

// destroyOrphans destroys every workspace the driver lists: at start nothing runs, so each is
// a killed supervisor's. Only workspace names (ws-tsk_…) are touched.
func destroyOrphans(ctx context.Context, drv workspace.Driver, log *slog.Logger) error {
	names, err := drv.List(ctx)
	if err != nil {
		return fmt.Errorf("list the leftover workspaces: %w", err)
	}
	for _, n := range names {
		if workspace.ValidName(n) != nil {
			continue
		}
		log.Info("destroying a leftover workspace", "workspace", n)
		if err := drv.Destroy(ctx, n); err != nil {
			return fmt.Errorf("destroy the leftover workspace %s: %w", n, err)
		}
	}
	return nil
}

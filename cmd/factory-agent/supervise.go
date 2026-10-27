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
	"os"
	"path/filepath"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/host"
	"github.com/ContainerSolutions/factory/internal/agent/katahelper"
	"github.com/ContainerSolutions/factory/internal/agent/login"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
	"github.com/ContainerSolutions/factory/internal/agent/probe"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/agent/workspace"
	"github.com/ContainerSolutions/factory/internal/agent/workspace/kata"
	"github.com/ContainerSolutions/factory/internal/agent/workspace/tart"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// superviseDeps are runSupervise's injectable dependencies; the zero value is production.
type superviseDeps struct {
	// Worker is the worker loop's (Probe: nil is supervise's own, the login store's).
	Worker workerDeps
	// Host is host check's environment; its zero fields get the production defaults.
	Host host.Env
	// HostCheck replaces host check's steps (tests); default host.Steps and host.Check.
	HostCheck func(ctx context.Context, e *host.Env) ([]host.Status, error)
	// NewDriver builds the tart driver; default tart.New.
	NewDriver func(tart.Config) workspace.Driver
	// NewKataDriver builds the kata driver; default kata.New.
	NewKataDriver func(kata.Config) workspace.Driver
	// Listen opens each gateway's listener (tests); default TCP on the address.
	Listen func(ctx context.Context, addr netip.AddrPort) (net.Listener, error)
}

func superviseCommand() command {
	return command{
		name:    "supervise",
		summary: "register with the dispatcher and run each leased task in its own VM workspace behind a gateway",
		run: func(ctx context.Context, args []string, _, stderr io.Writer) int {
			return runSupervise(ctx, args, stderr, superviseDeps{})
		},
	}
}

// errHostCheck is a host that host check doesn't pass; the error wrapping it names the driver
// host init must set up.
var errHostCheck = errors.New("host check failed")

func runSupervise(ctx context.Context, args []string, stderr io.Writer, d superviseDeps) int {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "the agent config file")
	keepWorkspace := fs.Bool("keep-workspace", false, "debugging: stop each workspace VM and keep it, and its run directory, instead of destroying them")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	driver := fs.String("driver", "", "the workspace driver: tart or kata (default: the config's workspace.driver, tart on a Mac and kata on Linux)")
	warmBase := fs.String("warm-base", host.WarmBaseVM, "tart: the VM each workspace is cloned from (host init builds it)")
	stateDir := fs.String("state-dir", "", "the agent's state dir: logins, runs and the gateway CA (default: the agent's per OS, ~/Library/Application Support/factory-agent on a Mac, $XDG_STATE_HOME/factory-agent or ~/.local/state/factory-agent on Linux)")
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
	warmBaseSet := false
	fs.Visit(func(f *flag.Flag) { warmBaseSet = warmBaseSet || f.Name == "warm-base" })
	switch *driver {
	case "", config.DriverTart, config.DriverKata:
	default:
		fmt.Fprintf(stderr, "factory-agent supervise: --driver %q is not tart or kata\n", *driver)
		return 2
	}
	if *driver == config.DriverKata && warmBaseSet {
		fmt.Fprintln(stderr, "factory-agent supervise: "+errWarmBaseKata.Error())
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
	s := &supervisor{d: d, e: &e, stderr: stderr, log: log, stateDir: *stateDir, driver: *driver,
		warmBase: *warmBase, warmBaseSet: warmBaseSet, keep: *keepWorkspace, unlock: func() {}}
	defer func() { s.unlock() }()
	return runAgentLoop(ctx, stderr, d.Worker, agentLoop{
		name: "supervisor", path: *path, stateDir: *stateDir, log: log, newRunner: s.newRunner, lending: s.lending, reload: s.reloadMemory,
	})
}

// errWarmBaseKata is --warm-base with the kata driver, whose workspaces come from host init's
// image and warm disk.
var errWarmBaseKata = errors.New("--warm-base is the tart driver's, not kata's")

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
	// driver is --driver: "" is the config's workspace.driver.
	driver      string
	warmBase    string
	warmBaseSet bool
	keep        bool
	// unlock releases the supervise lock newRunner takes, if it did.
	unlock func()
	// memTotal and workspaceGiB are the kata host's physical memory and one workspace's
	// (workspace.size.memory), set by newRunner before the first lending call; reloadMemory
	// re-reads memTotal on SIGHUP. After newRunner only the signal handler uses them.
	memTotal     uint64
	workspaceGiB int
}

// lending is the lending supervise registers, at start and on SIGHUP: tart runs one workspace at
// a time (supervise.md); kata honours lending.maxConcurrent, capped by kataConcurrency.
func (s *supervisor) lending(l v1alpha1.Lending) v1alpha1.Lending {
	if s.e.Workspace.Driver != config.DriverKata {
		l.MaxConcurrent = 1
		return l
	}
	want := max(l.MaxConcurrent, 1)
	n, by := kataConcurrency(want, s.memTotal, s.workspaceGiB)
	if n < want {
		s.log.Warn("lending.maxConcurrent capped", "configured", want, "max", n, "by", by)
	}
	l.MaxConcurrent = n
	return l
}

// reloadMemory re-reads the kata host's physical memory on SIGHUP, before lending caps the
// reloaded maxConcurrent with it: a VM host's memory can be resized without a restart. A failed
// read keeps the last good value. workspace.size stays the start's, as the runner's does.
func (s *supervisor) reloadMemory() {
	if s.e.Workspace.Driver != config.DriverKata {
		return
	}
	m, err := s.e.MemTotal()
	if err != nil {
		s.log.Warn("SIGHUP: physical memory not re-read; the lending cap keeps the last", "memoryGiB", s.memTotal>>30, "err", err)
		return
	}
	if m == s.memTotal {
		return
	}
	was, _ := kataConcurrency(katahelper.Slots, s.memTotal, s.workspaceGiB)
	now, _ := kataConcurrency(katahelper.Slots, m, s.workspaceGiB)
	s.log.Info("SIGHUP: physical memory changed", "memoryGiB", m>>30, "wasGiB", s.memTotal>>30, "maxWorkspaces", now, "wasMaxWorkspaces", was)
	s.memTotal = m
}

// kataConcurrency is the kata driver's most workspaces at once for a lending.maxConcurrent of
// want (workspace-kata.md#supervise-pr-6): the workspace slots, and the workspaces of
// workspaceGiB that fit in the physical memory less host.HeadroomGiB, at least one (host check
// has made sure one fits). by says which cap bit, when one did.
func kataConcurrency(want int, memTotal uint64, workspaceGiB int) (n int, by string) {
	n = want
	if n > katahelper.Slots {
		n, by = katahelper.Slots, fmt.Sprintf("the %d workspace slots", katahelper.Slots)
	}
	if workspaceGiB < 1 {
		return n, by
	}
	fit := 0
	if headroom := uint64(host.HeadroomGiB) << 30; memTotal > headroom {
		fit = int((memTotal - headroom) / (uint64(workspaceGiB) << 30)) //nolint:gosec // a count of workspaces fits an int
	}
	if fit = max(fit, 1); fit < n {
		n, by = fit, fmt.Sprintf("memory: %d GiB, less %d GiB for the host, fits %d workspaces of %d GiB", memTotal>>30, host.HeadroomGiB, fit, workspaceGiB)
	}
	return n, by
}

// newRunner gates on host check, takes the supervise lock, destroys the workspaces a killed
// supervisor left, and builds the workspace runner on the host pipeline.
func (s *supervisor) newRunner(ctx context.Context, cfg *config.Config) (runner.Runner, error) {
	w := config.Workspace{}
	if cfg.Workspace != nil {
		w = *cfg.Workspace
	}
	if s.driver != "" {
		w.Driver = s.driver
	}
	w = w.WithDefaults(s.e.GOOS)
	if w.Driver == config.DriverKata && s.warmBaseSet {
		return nil, fmt.Errorf("%w (workspace.driver is kata)", errWarmBaseKata)
	}
	s.e.Workspace = w
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
		return nil, fmt.Errorf("%w: run `factory-agent host init --driver %s`", errHostCheck, w.Driver)
	}
	warnSkipped(s.stderr, sts)
	// One supervisor per user (supervise.md): the orphan cleanup would destroy another's VM.
	// The lock is in the host's state dir, which host check made sure exists.
	unlock, err := host.LockSupervise(s.e)
	if err != nil {
		return nil, err
	}
	s.unlock = unlock

	drv, spec, err := s.newDriver(ctx, sts)
	if err != nil {
		return nil, err
	}
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
		Driver: drv, Pipeline: p, StateDir: s.stateDir, Spec: spec, KeepWorkspace: s.keep,
		GatewayPort: w.GatewayPort, Listen: s.d.Listen, Logger: s.log,
	})
}

// newDriver builds the workspace driver host check passed for, and the size of its workspaces:
// workspace.size for both drivers, which host init built tart's base VM with and host check's
// memory check assumes. kata's are from host init's image, on its warm disk when the katawarm
// step is ok, and the lending cap is computed from their memory.
func (s *supervisor) newDriver(ctx context.Context, sts []host.Status) (workspace.Driver, workspace.Spec, error) {
	w := s.e.Workspace
	if w.Driver == config.DriverTart {
		newDriver := s.d.NewDriver
		if newDriver == nil {
			newDriver = func(c tart.Config) workspace.Driver { return tart.New(c) }
		}
		tartLog := io.Discard
		if s.log.Enabled(ctx, slog.LevelDebug) {
			tartLog = s.stderr
		}
		return newDriver(tart.Config{Bin: s.e.TartBin(ctx), WarmBase: s.warmBase, Log: tartLog}), sizeSpec(w.Size), nil
	}
	img, err := s.e.ReadKataBase()
	if err != nil {
		return nil, workspace.Spec{}, fmt.Errorf("the workspace image: %w", err)
	}
	if s.memTotal, err = s.e.MemTotal(); err != nil {
		return nil, workspace.Spec{}, err
	}
	s.workspaceGiB = w.Size.MemoryGiB()
	warm := false
	for _, st := range sts {
		warm = warm || st.Step == host.KataWarmStep && st.State == host.StateOK
	}
	if _, err := os.Stat(s.e.KataWarmRecordPath()); err != nil {
		warm = false
	}
	if !warm {
		s.log.Warn("no warm disk: each workspace starts with an empty Docker disk (run `factory-agent host init --driver kata --only katawarm`)")
	}
	newKata := s.d.NewKataDriver
	if newKata == nil {
		newKata = func(c kata.Config) workspace.Driver { return kata.New(c) }
	}
	drv := newKata(kata.Config{Image: img.Image, DiskGiB: w.Size.DiskGiB(), Warm: warm})
	return drv, sizeSpec(w.Size), nil
}

// sizeSpec is workspace.size as the Spec each workspace is created with.
func sizeSpec(sz config.WorkspaceSize) workspace.Spec {
	return workspace.Spec{CPU: sz.CPUs, MemoryMiB: sz.MemoryGiB() << 10}
}

// destroyOrphans destroys every workspace the driver lists: at start nothing runs, so each is
// a killed supervisor's. Only workspace names (ws-tsk_…) are touched, and never host init's
// warm build workspace (host.KataWarmBuild), which an init running now may be using.
func destroyOrphans(ctx context.Context, drv workspace.Driver, log *slog.Logger) error {
	names, err := drv.List(ctx)
	if err != nil {
		return fmt.Errorf("list the leftover workspaces: %w", err)
	}
	for _, n := range names {
		if workspace.ValidName(n) != nil || n == host.KataWarmBuild {
			continue
		}
		log.Info("destroying a leftover workspace", "workspace", n)
		if err := drv.Destroy(ctx, n); err != nil {
			return fmt.Errorf("destroy the leftover workspace %s: %w", n, err)
		}
	}
	return nil
}

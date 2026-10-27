// Package host checks and sets up an agent host for VM workspaces (P1 §7,
// design/workstreams/agent/LLD/workspace-host.md). For the tart driver the steps are the
// platform, the state dir, Tart, Softnet, the pf anchor, the base VM and the warm base; for the
// kata driver (workspace-kata.md) the platform, the state dir, nerdctl-full, Kata, its config,
// the factory's containerd, the workspace network, the root helper, the workspace image and the
// warm disk. Check reports
// each step's state; Init applies the steps that aren't ok, in order, and runs their root
// actions in one sudo script.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/version"
)

// State is a step's state.
type State string

// The states a Check reports.
const (
	StateOK       State = "ok"
	StateMissing  State = "missing"
	StateOutdated State = "outdated"
	StateWrong    State = "wrong"
	// StateSkipped is a step the owner opted out of (host init --no-pf): AllOK counts it as
	// ok, and host check prints a warning.
	StateSkipped State = "skipped"
)

// Status is one step's state, with what was found.
type Status struct {
	Step   string `json:"step"`
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Plan says what a step's Apply does.
type Plan struct {
	// Do lists the unprivileged actions, one line each (printed by --dry-run).
	Do []string
	// Root are the shell lines that need root. Init batches every step's lines into one sudo
	// script, run after the Apply of the last step that has some.
	Root []string
}

// Step is one piece of an agent host's setup (P1 §7).
type Step interface {
	Name() string
	Check(ctx context.Context) (Status, error)
	Plan(ctx context.Context) (Plan, error)
	// Apply does the unprivileged part of the plan: downloads, verification, and the files the
	// root lines install, staged in Env.StageDir.
	Apply(ctx context.Context) error
}

// Runner runs a command; out gets its stdout and stderr.
type Runner interface {
	Run(ctx context.Context, out io.Writer, name string, args ...string) error
}

// ExecRunner runs real programs. When ctx ends, a command gets SIGKILL, but for a sudo -n
// call (the kata helper's verbs): see onCancel.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	onCancel(cmd, name, args)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Env is what the steps run against. Every field but Workspace has a production default
// (see Defaults), so tests replace the commands, the network and the file system root.
type Env struct {
	Workspace config.Workspace
	Pins      Pins
	Runner    Runner
	// Spawner runs tart run in the background and tart exec -i with a stdin.
	Spawner Spawner
	Fetcher Fetcher
	// Root prefixes the system paths (/usr/local, /etc, /Library, /dev, /proc, /sys, /run);
	// "" in production.
	Root string
	// Home is the owner's home: the state dir is under it.
	Home string
	// Getenv reads an environment variable (XDG_STATE_HOME, for the state dir on linux);
	// default os.Getenv.
	Getenv func(string) string
	// StageDir holds the files the root lines install. Init makes a temp dir when it's "".
	StageDir     string
	GOOS, GOARCH string
	// UID is the owner's user id (the state dir's owner).
	UID func() int
	// Owner reads a file's owner; ok is false when the platform has none.
	Owner func(fs.FileInfo) (uid, gid int, ok bool)
	// LookPath finds a program on PATH.
	LookPath func(string) (string, error)
	// Prompt asks a y/N question; Init asks it before the pf and privileges steps unless --yes.
	Prompt func(question string) (bool, error)
	// Executable is the running factory-agent, which the privileges step installs as the kata
	// helper; default os.Executable().
	Executable string
	// Username is the user running host init, the owner the kata helper's sudoers rule names;
	// default os/user's current user.
	Username func() (string, error)
	// Clock dates the pf opt-out and the VM records' builtAt.
	Clock clock.Clock
	// AgentVersion names the base VM (factory-agent-base-<version>); default version.Version.
	AgentVersion string
	// AgentLinux is the linux/arm64 factory-agent the base VM gets; default
	// factory-agent-linux-arm64 next to the running binary.
	AgentLinux string
	// KeepFailed keeps a VM build's .building VM when the build fails, for debugging.
	KeepFailed bool
	// VM is the VM builds' timeouts and poll interval.
	VM VMTimings
	// Out gets the plan and the progress.
	Out io.Writer
	Log *slog.Logger
}

// VMTimings bound a VM build's waits.
type VMTimings struct {
	// Boot is the wait for the VM's guest agent (tart exec true): default 3 minutes.
	Boot time.Duration
	// Provision bounds the provisioning script, and the warm base's image pulls: 30 minutes.
	Provision time.Duration
	// Shutdown is the wait for the VM to stop after shutdown, before tart stop: 2 minutes.
	Shutdown time.Duration
	// Poll is the interval between the waits' checks: 2 seconds.
	Poll time.Duration
}

func (t *VMTimings) defaults() {
	for _, d := range []struct {
		p *time.Duration
		v time.Duration
	}{{&t.Boot, 3 * time.Minute}, {&t.Provision, 30 * time.Minute}, {&t.Shutdown, 2 * time.Minute}, {&t.Poll, 2 * time.Second}} {
		if *d.p == 0 {
			*d.p = d.v
		}
	}
}

// Defaults fills the unset fields with the production ones.
func (e *Env) Defaults() error {
	if e.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		e.Home = h
	}
	if e.Pins == (Pins{}) {
		e.Pins = DefaultPins()
	}
	if e.Runner == nil {
		e.Runner = ExecRunner{}
	}
	if e.Spawner == nil {
		e.Spawner = ExecRunner{}
	}
	if e.AgentVersion == "" {
		e.AgentVersion = version.Version
	}
	if e.AgentLinux == "" {
		// without the executable's path, the basevm check reports the binary missing.
		if exe, err := os.Executable(); err == nil {
			e.AgentLinux = filepath.Join(filepath.Dir(exe), AgentLinuxName)
		}
	}
	e.VM.defaults()
	if e.Fetcher == nil {
		e.Fetcher = HTTPFetcher{}
	}
	if e.GOOS == "" {
		e.GOOS, e.GOARCH = runtime.GOOS, runtime.GOARCH
	}
	if e.UID == nil {
		e.UID = os.Getuid
	}
	if e.Owner == nil {
		e.Owner = fileOwner
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Prompt == nil {
		e.Prompt = func(string) (bool, error) {
			return false, errors.New("no terminal to ask: pass --yes (or, for the pf step, --no-pf)")
		}
	}
	if e.Executable == "" {
		// without it, the privileges step's plan and apply fail, naming it.
		if exe, err := os.Executable(); err == nil {
			e.Executable = exe
		}
	}
	if e.Username == nil {
		e.Username = func() (string, error) {
			u, err := user.Current()
			if err != nil {
				return "", err
			}
			return u.Username, nil
		}
	}
	if e.Clock == nil {
		e.Clock = clock.Real{}
	}
	if e.Out == nil {
		e.Out = io.Discard
	}
	if e.Log == nil {
		e.Log = slog.New(slog.DiscardHandler)
	}
	return nil
}

// sys is a system path under Root.
func (e *Env) sys(p string) string { return filepath.Join("/", e.Root, p) }

// StateDir is the agent's state dir (P1 §7), per OS: ~/Library/Application Support/factory-agent
// on darwin; elsewhere $XDG_STATE_HOME/factory-agent when XDG_STATE_HOME is an absolute path
// (the XDG spec ignores a relative one), else ~/.local/state/factory-agent (workspace-kata.md).
// An unset GOOS or Getenv is the running system's, so an Env with only Home has the owner's.
func (e *Env) StateDir() string {
	goos, getenv := e.GOOS, e.Getenv
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "darwin" {
		return filepath.Join(e.Home, "Library", "Application Support", "factory-agent")
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if x := getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "factory-agent")
	}
	return filepath.Join(e.Home, ".local", "state", "factory-agent")
}

// output runs a command and returns its trimmed output.
func (e *Env) output(ctx context.Context, name string, args ...string) (string, error) {
	var b bytes.Buffer
	if err := e.Runner.Run(ctx, &b, name, args...); err != nil {
		return "", fmt.Errorf("%w: %s", err, firstLine(b.String()))
	}
	return strings.TrimSpace(b.String()), nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// Steps are the workspace driver's steps, in order.
func Steps(e *Env) ([]Step, error) {
	switch e.Workspace.Driver {
	case config.DriverTart:
		return []Step{platformStep{e}, stateDirStep{e}, tartStep{e}, softnetStep{e}, pfStep{e}, baseVMStep{e}, warmBaseStep{e}}, nil
	case config.DriverKata:
		return []Step{kataPlatformStep{e}, stateDirStep{e}, nerdctlStep{e}, kataStep{e}, kataConfigStep{e},
			containerdStep{e}, networkStep{e}, privilegesStep{e}, kataImageStep{e}, kataWarmStep{e}}, nil
	}
	return nil, fmt.Errorf("no host steps for the %q driver", e.Workspace.Driver)
}

// Check runs every step's Check. A step whose check fails is reported wrong, with the error.
func Check(ctx context.Context, steps []Step) []Status {
	out := make([]Status, len(steps))
	for i, s := range steps {
		st, err := s.Check(ctx)
		if err != nil {
			st = Status{State: StateWrong, Detail: err.Error()}
		}
		st.Step = s.Name()
		out[i] = st
	}
	return out
}

// AllOK reports whether every status is ok (or skipped by the owner).
func AllOK(sts []Status) bool {
	for _, s := range sts {
		if s.State != StateOK && s.State != StateSkipped {
			return false
		}
	}
	return len(sts) > 0
}

// InitOptions are host init's flags.
type InitOptions struct {
	DryRun bool
	// Yes applies the pf and privileges steps without asking.
	Yes bool
	// NoPF skips the pf step and records the opt-out in the state dir.
	NoPF bool
	// Only, when set, names the steps to apply; the others are checked but not applied.
	Only []string
}

// pfNotice is what Init says before asking about the pf step.
func pfNotice() string {
	return fmt.Sprintf(`The pf step lets VMs on %[1]s reach only the gateway port on this Mac.
Softnet and vmnet number their bridges from bridge%[2]d up, one per running VM, so the rules
apply to every VM on %[1]s, not only factory workspaces: VMs on other shared
networking (plain Tart NAT, UTM, Docker Desktop on vmnet) can no longer reach this Mac's
other listening services. `+"`factory-agent host init --remove-pf`"+` undoes it.
`, pfBridgeRange(), pfFirstBridge)
}

// Init applies the steps that aren't ok, in order (only those in o.Only, when set), runs the
// root lines in one sudo script after the last step that has some, and checks again. It
// returns the final check (the first one with DryRun, which changes nothing), also when an
// Apply or the sudo script fails. A failed platform check stops it before anything changes.
// A pf step skipped (--no-pf, or the prompt declined) is recorded as an opt-out in the state
// dir; one that's installed removes it. The kata driver's privileges step is asked about the
// same way (unless o.Yes); declined, it's left out, and so are the steps that run the helper it
// installs (helperStep), each with a warning. Nothing records that decline: the next Init asks
// again, and the final check reports those steps as they are. With o.Only, the statedir step is applied
// too when it isn't ok, since the applies need the dir. The applies hold <statedir>/host.lock:
// a second Init fails while one runs.
func Init(ctx context.Context, e *Env, o InitOptions) ([]Status, error) {
	// the kata driver stages its downloads (about 1.3 GB) in <statedir>/stage, not in $TMPDIR,
	// which may be a tmpfs: it's made once the host lock is held (the dir exists by then) and
	// removed before the lock is released.
	stageInState := false
	if e.StageDir == "" {
		if e.Workspace.Driver == config.DriverKata {
			e.StageDir, stageInState = filepath.Join(e.StateDir(), "stage"), true
			defer func() { e.StageDir = "" }()
		} else {
			dir, err := os.MkdirTemp("", "factory-agent-host-")
			if err != nil {
				return nil, err
			}
			e.StageDir = dir
			defer func() { _ = os.RemoveAll(dir); e.StageDir = "" }()
		}
	}
	steps, err := Steps(e)
	if err != nil {
		return nil, err
	}
	only, err := onlySet(steps, o.Only)
	if err != nil {
		return nil, err
	}
	sts := Check(ctx, steps)
	var todo []Step
	var optOut string // the pf opt-out's reason, when the step is skipped
	pfTodo := false
	declined := false // the owner declined the privileges step: the helper's steps can't run
	for i, s := range steps {
		switch {
		case sts[i].State == StateOK:
			continue
		case only != nil && !only[s.Name()] && s.Name() != platformName:
			continue
		case s.Name() == platformName:
			return sts, fmt.Errorf("platform: %s", sts[i].Detail)
		case declined && usesHelper(s):
			fmt.Fprintf(e.Out, "warning: %s skipped: privileges were declined, and %s through the kata helper\n", s.Name(), s.(helperStep).throughHelper())
			continue
		case s.Name() == pfName && o.NoPF:
			fmt.Fprintln(e.Out, "warning: --no-pf: workspaces can reach every listening service on this Mac's bridge address")
			optOut = "--no-pf"
			continue
		case s.Name() == pfName && !o.Yes && !o.DryRun:
			fmt.Fprint(e.Out, pfNotice())
			yes, err := e.Prompt("Install the pf anchor? [y/N] ")
			if err != nil {
				return sts, err
			}
			if !yes {
				fmt.Fprintln(e.Out, "warning: pf skipped: workspaces can reach every listening service on this Mac's bridge address")
				optOut = "declined at host init's prompt"
				continue
			}
		case s.Name() == privilegesName && !o.Yes && !o.DryRun:
			fmt.Fprint(e.Out, privilegesNotice())
			yes, err := e.Prompt("Install the kata helper and its sudoers rule? [y/N] ")
			if err != nil {
				return sts, err
			}
			if !yes {
				fmt.Fprintln(e.Out, "warning: privileges skipped: supervise --driver kata can't run a workspace without the helper")
				declined = true
				continue
			}
		}
		pfTodo = pfTodo || s.Name() == pfName
		todo = append(todo, s)
	}
	// every apply holds the state dir's lock, and the tart, pf opt-out and VM steps write in
	// the dir: with --only, the statedir step comes first whenever it isn't ok.
	if only != nil && (len(todo) > 0 || optOut != "") && !slices.ContainsFunc(todo, func(s Step) bool { return s.Name() == stateDirName }) {
		if i := slices.IndexFunc(steps, func(s Step) bool { return s.Name() == stateDirName }); i >= 0 && sts[i].State != StateOK {
			todo = append([]Step{steps[i]}, todo...)
		}
	}
	var root []string
	lastRoot := -1 // the todo index after whose Apply the sudo script runs
	fmt.Fprintln(e.Out, "plan:")
	for i, s := range todo {
		p, err := s.Plan(ctx)
		if err != nil {
			return sts, fmt.Errorf("%s: %w", s.Name(), err)
		}
		for _, d := range p.Do {
			fmt.Fprintf(e.Out, "  %-9s %s\n", s.Name(), d)
		}
		if len(p.Root) > 0 {
			root, lastRoot = append(root, p.Root...), i
		}
	}
	if optOut != "" {
		fmt.Fprintf(e.Out, "  %-9s record the opt-out (%s) in %s\n", pfName, optOut, pfOptOutPath(e))
	}
	if len(todo) == 0 && optOut == "" {
		fmt.Fprintln(e.Out, "  nothing to do")
	}
	script := Script(root)
	if o.DryRun {
		if len(root) > 0 {
			fmt.Fprintf(e.Out, "sudo script (one sudo prompt):\n%s", script)
		}
		return sts, nil
	}
	// after a failure the statuses are checked again: some steps may have changed things.
	// The sudo script runs as soon as its steps are staged, so the long VM builds after it
	// don't hold back the password prompt, and a failed build doesn't skip it.
	//
	// The applies, all but the statedir step's (which makes the dir the lock is in), hold
	// <statedir>/host.lock: a second host init would delete this one's .building VMs.
	release := func() {}
	defer func() { release() }()
	locked := false
	lock := func() error {
		if locked {
			return nil
		}
		r, err := lockHost(e)
		if err != nil {
			return err
		}
		release, locked = r, true
		if stageInState {
			// a killed init's stage goes first. The state dir is the owner's, 0700.
			stage := e.StageDir
			release = func() { _ = os.RemoveAll(stage); r() }
			if err := os.RemoveAll(stage); err != nil {
				return err
			}
			if err := os.Mkdir(stage, 0o700); err != nil {
				return err
			}
		}
		return nil
	}
	if optOut != "" && !slices.ContainsFunc(todo, func(s Step) bool { return s.Name() == stateDirName }) {
		if err := lock(); err != nil {
			return Check(ctx, steps), err
		}
		if err := writePFOptOut(e, optOut); err != nil {
			return Check(ctx, steps), fmt.Errorf("%s: %w", pfName, err)
		}
		optOut = ""
	}
	for i, s := range todo {
		if s.Name() != stateDirName {
			if err := lock(); err != nil {
				return Check(ctx, steps), err
			}
		}
		e.Log.DebugContext(ctx, "host init: apply", "step", s.Name())
		if err := s.Apply(ctx); err != nil {
			return Check(ctx, steps), fmt.Errorf("%s: %w", s.Name(), err)
		}
		// the statedir step, just applied, made the dir the opt-out goes in.
		if s.Name() == stateDirName && optOut != "" {
			if err := lock(); err != nil {
				return Check(ctx, steps), err
			}
			if err := writePFOptOut(e, optOut); err != nil {
				return Check(ctx, steps), fmt.Errorf("%s: %w", pfName, err)
			}
			optOut = ""
		}
		if i == lastRoot {
			if err := runRoot(ctx, e, script); err != nil {
				return Check(ctx, steps), err
			}
			// pf's root lines are in the script, so it's installed now: before the VM builds,
			// which may fail.
			if pfTodo {
				if err := removePFOptOut(e); err != nil {
					return Check(ctx, steps), fmt.Errorf("%s: %w", pfName, err)
				}
			}
		}
	}
	return Check(ctx, steps), nil
}

// onlySet is --only's step names as a set; nil when there are none. An unknown name is an
// error.
func onlySet(steps []Step, names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	var all []string
	for _, s := range steps {
		known[s.Name()] = true
		all = append(all, s.Name())
	}
	set := map[string]bool{}
	for _, n := range names {
		if !known[n] {
			return nil, fmt.Errorf("--only: no step %q (the steps: %s)", n, strings.Join(all, ", "))
		}
		set[n] = true
	}
	return set, nil
}

// Script is the sudo batch: the root lines under set -eu.
func Script(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "#!/bin/sh\n# factory-agent host init: the root actions, run once with sudo.\nset -eu\n" + strings.Join(lines, "\n") + "\n"
}

// runRoot writes the script 0600 into the stage dir, runs it with one sudo, and removes it.
func runRoot(ctx context.Context, e *Env, script string) error {
	p := filepath.Join(e.StageDir, "root.sh")
	if err := os.WriteFile(p, []byte(script), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(p) }()
	fmt.Fprintln(e.Out, "running the root actions with sudo (it may ask for your password)")
	if err := e.Runner.Run(ctx, e.Out, "sudo", "/bin/sh", p); err != nil {
		return fmt.Errorf("the sudo script: %w", err)
	}
	return nil
}

var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// quote quotes s for sh.
func quote(s string) string {
	if shellSafeRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ErrHostLocked is Init's error while another host init holds the state dir's lock.
var ErrHostLocked = errors.New("another factory-agent host init is running")

// hostLockPath is the lock Init holds while it applies.
func hostLockPath(e *Env) string { return filepath.Join(e.StateDir(), "host.lock") }

// ErrSuperviseLocked is LockSupervise's error while another supervise holds the lock.
var ErrSuperviseLocked = errors.New("another factory-agent supervise is running")

// superviseLockPath is the lock a supervise holds for its life: under the host's state dir,
// whatever its --state-dir, since the VMs it cleans up are the user's.
func superviseLockPath(e *Env) string { return filepath.Join(e.StateDir(), "supervise.lock") }

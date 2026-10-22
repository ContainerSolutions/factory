// Package host checks and sets up an agent host for VM workspaces (P1 §7,
// design/workstreams/agent/LLD/workspace-host.md). For the tart driver the steps are the
// platform, the state dir, Tart, Softnet and the pf anchor. Check reports each step's state;
// Init applies the steps that aren't ok, in order, and runs their root actions in one sudo
// script.
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
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// State is a step's state.
type State string

// The states a Check reports.
const (
	StateOK       State = "ok"
	StateMissing  State = "missing"
	StateOutdated State = "outdated"
	StateWrong    State = "wrong"
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
	// script, run after every step's Apply.
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

// ExecRunner runs real programs.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 10 * time.Second
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
	Fetcher   Fetcher
	// Root prefixes the system paths (/usr/local, /etc, /Library); "" in production.
	Root string
	// Home is the owner's home: the state dir is under it.
	Home string
	// StageDir holds the files the root lines install. Init makes a temp dir when it's "".
	StageDir     string
	GOOS, GOARCH string
	// UID is the owner's user id (the state dir's owner).
	UID func() int
	// Owner reads a file's owner; ok is false when the platform has none.
	Owner func(fs.FileInfo) (uid, gid int, ok bool)
	// LookPath finds a program on PATH.
	LookPath func(string) (string, error)
	// Prompt asks a y/N question; Init asks it before the pf step unless --yes.
	Prompt func(question string) (bool, error)
	// Out gets the plan and the progress.
	Out io.Writer
	Log *slog.Logger
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
	if e.Prompt == nil {
		e.Prompt = func(string) (bool, error) { return false, errors.New("no terminal to ask: pass --yes or --no-pf") }
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

// StateDir is the agent's state dir (P1 §7).
func (e *Env) StateDir() string {
	return filepath.Join(e.Home, "Library", "Application Support", "factory-agent")
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

// Steps are the tart driver's steps, in order. The kata driver's come in P1 phase 9.
func Steps(e *Env) ([]Step, error) {
	if e.Workspace.Driver != config.DriverTart {
		return nil, fmt.Errorf("the %s driver's host init comes in P1 phase 9", e.Workspace.Driver)
	}
	return []Step{platformStep{e}, stateDirStep{e}, tartStep{e}, softnetStep{e}, pfStep{e}}, nil
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

// AllOK reports whether every status is ok.
func AllOK(sts []Status) bool {
	for _, s := range sts {
		if s.State != StateOK {
			return false
		}
	}
	return len(sts) > 0
}

// InitOptions are host init's flags.
type InitOptions struct {
	DryRun bool
	// Yes installs the pf step without asking.
	Yes bool
	// NoPF skips the pf step.
	NoPF bool
}

const pfNotice = `The pf step lets VMs on Softnet bridges reach only the gateway port on this Mac.
pf's "bridge" group is every bridgeN interface, so the rule also applies to other VMs' shared
networking (plain Tart NAT, UTM, Docker Desktop on vmnet): they can no longer reach this Mac's
other listening services. ` + "`factory-agent host init --remove-pf`" + ` undoes it.
`

// Init applies the steps that aren't ok, in order, then runs the root lines in one sudo
// script and checks again. It returns the final check (the first one with DryRun, which
// changes nothing). A failed platform check stops it before anything changes.
func Init(ctx context.Context, e *Env, o InitOptions) ([]Status, error) {
	if e.StageDir == "" {
		dir, err := os.MkdirTemp("", "factory-agent-host-")
		if err != nil {
			return nil, err
		}
		e.StageDir = dir
		defer func() { _ = os.RemoveAll(dir); e.StageDir = "" }()
	}
	steps, err := Steps(e)
	if err != nil {
		return nil, err
	}
	sts := Check(ctx, steps)
	var todo []Step
	for i, s := range steps {
		switch {
		case sts[i].State == StateOK:
			continue
		case s.Name() == platformName:
			return sts, fmt.Errorf("platform: %s", sts[i].Detail)
		case s.Name() == pfName && o.NoPF:
			fmt.Fprintln(e.Out, "warning: --no-pf: workspaces can reach every listening service on this Mac's bridge address")
			continue
		case s.Name() == pfName && !o.Yes && !o.DryRun:
			fmt.Fprint(e.Out, pfNotice)
			yes, err := e.Prompt("Install the pf anchor? [y/N] ")
			if err != nil {
				return sts, err
			}
			if !yes {
				fmt.Fprintln(e.Out, "warning: pf skipped: workspaces can reach every listening service on this Mac's bridge address")
				continue
			}
		}
		todo = append(todo, s)
	}
	var root []string
	fmt.Fprintln(e.Out, "plan:")
	for _, s := range todo {
		p, err := s.Plan(ctx)
		if err != nil {
			return sts, fmt.Errorf("%s: %w", s.Name(), err)
		}
		for _, d := range p.Do {
			fmt.Fprintf(e.Out, "  %-9s %s\n", s.Name(), d)
		}
		root = append(root, p.Root...)
	}
	if len(todo) == 0 {
		fmt.Fprintln(e.Out, "  nothing to do")
	}
	script := Script(root)
	if o.DryRun {
		if len(root) > 0 {
			fmt.Fprintf(e.Out, "sudo script (one sudo prompt):\n%s", script)
		}
		return sts, nil
	}
	for _, s := range todo {
		e.Log.DebugContext(ctx, "host init: apply", "step", s.Name())
		if err := s.Apply(ctx); err != nil {
			return sts, fmt.Errorf("%s: %w", s.Name(), err)
		}
	}
	if len(root) > 0 {
		if err := runRoot(ctx, e, script); err != nil {
			return sts, err
		}
	}
	return Check(ctx, steps), nil
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

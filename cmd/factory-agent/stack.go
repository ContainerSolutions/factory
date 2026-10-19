package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/stack"
	"github.com/compliance-framework/factory/internal/clock"
)

// stackDeps are the stack command's injectable dependencies; the zero value is production.
type stackDeps struct {
	Config config.Options
	// Runner runs make and docker; default stack.ExecRunner.
	Runner stack.Runner
	// GitHub reads trains and releases; default stack.GitHub with $GH_TOKEN when set.
	GitHub     stack.GitHubReader
	HTTPClient *http.Client
	Clock      clock.Clock
	Getenv     func(string) string
}

// Exit codes beyond 0, 1 (failed) and 2 (usage).
const exitStackBusy = 3

func stackCommand() command {
	return command{
		name:    "stack",
		summary: "bring the local CCF stack up at pinned versions, wait for it, take it down",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runStack(ctx, args, stdout, stderr, stackDeps{})
		},
	}
}

func stackUsage(w io.Writer) {
	fmt.Fprint(w, `usage: factory-agent stack <op> [flags]   (local-stack.md)

  up   [--versions versions.json | --train <owner/repo#n> | --release <repo>@<tag>]
       [--profile showcase|free] [--task <id>] [--local-dev <dir>] [--dry-run]
         runs local-dev's make demo-up-<profile> with the versions pinned, under the compose
         project factory-<task>; components not pinned run their latest final release.
         Prints the stack's environment. --dry-run prints the make invocation instead.
  wait [--timeout 10m]   polls the API's health and the UI until both answer 200
  down                   compose down --volumes for the stack's project, then releases the lock
  env                    prints FACTORY_STACK_URL, FACTORY_STACK_API, … for the stack that is up

common flags: --config <agent.yaml> (localDev), --state-dir <dir> (the lock; default
$FACTORY_STACK_DIR, else ~/.factory-agent). One stack per machine: up exits 3 when another
stack is up.
`)
}

// runStack dispatches a stack op.
func runStack(ctx context.Context, args []string, stdout, stderr io.Writer, d stackDeps) int {
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if len(args) == 0 {
		stackUsage(stderr)
		return 2
	}
	op, args := args[0], args[1:]
	switch op {
	case "help", "-h", "--help":
		stackUsage(stdout)
		return 0
	case "up", "wait", "down", "env":
	default:
		fmt.Fprintf(stderr, "factory-agent stack: unknown op %q\n\n", op)
		stackUsage(stderr)
		return 2
	}
	s := &stackOp{d: d, stdout: stdout, stderr: stderr, op: op}
	fs := flag.NewFlagSet("stack "+op, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&s.configPath, "config", config.DefaultPath, "the agent config file (localDev)")
	fs.StringVar(&s.stateDir, "state-dir", "", "the directory of the stack lock (default $FACTORY_STACK_DIR, else ~/.factory-agent)")
	var (
		up      upFlags
		timeout time.Duration
	)
	switch op {
	case "up":
		fs.StringVar(&up.versions, "versions", "", "a versions JSON file {api, ui, agent, plugins}")
		fs.StringVar(&up.train, "train", "", "a release train's tracking issue, owner/repo#n")
		fs.StringVar(&up.release, "release", "", "one release, <repo>@<tag>")
		fs.StringVar(&up.profile, "profile", stack.ProfileShowcase, "showcase (seeded data, for GIFs) or free")
		fs.StringVar(&up.task, "task", "", "the task the stack is for: its compose project is factory-<task> (default $FACTORY_TASK_ID, else manual)")
		fs.StringVar(&up.localDev, "local-dev", "", "the local-dev checkout (default: the stack's, else the config's localDev)")
		fs.BoolVar(&up.dryRun, "dry-run", false, "print the make invocation and its environment; run nothing")
	case "wait":
		fs.DurationVar(&timeout, "timeout", stack.DefaultWaitTimeout, "how long to wait")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "factory-agent stack %s: unexpected arguments %v\n", op, fs.Args())
		return 2
	}
	m, err := s.manager()
	if err != nil {
		return s.fail(err)
	}
	switch op {
	case "up":
		return s.up(ctx, m, up)
	case "wait":
		if _, err := m.Current(); err != nil {
			return s.fail(err)
		}
		if err := m.Wait(ctx, timeout); err != nil {
			return s.fail(err)
		}
		fmt.Fprintln(stderr, "the stack is ready")
		return 0
	case "down":
		cur, err := m.Current()
		if errors.Is(err, stack.ErrNoStack) {
			fmt.Fprintln(stderr, "no stack is up")
			return 0
		}
		if err != nil {
			return s.fail(err)
		}
		if err := m.WithOutput(stderr).Down(ctx, cur.Project); err != nil {
			return s.fail(err)
		}
		fmt.Fprintf(stderr, "stack %s is down\n", cur.Project)
		return 0
	default: // env
		cur, err := m.Current()
		if err != nil {
			return s.fail(err)
		}
		if err := stack.FormatEnv(stdout, m.Env(cur)); err != nil {
			return s.fail(err)
		}
		return 0
	}
}

type upFlags struct {
	versions, train, release, profile, task, localDev string
	dryRun                                            bool
}

type stackOp struct {
	d              stackDeps
	stdout, stderr io.Writer
	op             string
	configPath     string
	stateDir       string
}

func (s *stackOp) fail(err error) int {
	fmt.Fprintf(s.stderr, "factory-agent stack %s: %v\n", s.op, err)
	if stack.IsBusy(err) {
		return exitStackBusy
	}
	return 1
}

func (s *stackOp) home() func() (string, error) {
	if s.d.Config.Home != nil {
		return s.d.Config.Home
	}
	return os.UserHomeDir
}

// manager is the stack manager for the state directory: --state-dir, else
// $FACTORY_STACK_DIR (set in a run's environment), else ~/.factory-agent.
func (s *stackOp) manager() (*stack.Manager, error) {
	dir := s.stateDir
	if dir == "" {
		dir = s.d.Getenv(stack.EnvDir)
	}
	if dir == "" {
		dir = stack.DefaultStateDir
	}
	dir, err := config.ExpandPath(dir, s.home())
	if err != nil {
		return nil, err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	return stack.New(stack.Options{StateDir: dir, Runner: s.d.Runner, HTTPClient: s.d.HTTPClient, Clock: s.d.Clock})
}

func (s *stackOp) up(ctx context.Context, m *stack.Manager, f upFlags) int {
	set := 0
	for _, v := range []string{f.versions, f.train, f.release} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		fmt.Fprintln(s.stderr, "factory-agent stack up: give at most one of --versions, --train and --release")
		return 2
	}
	task := f.task
	if task == "" {
		task = s.d.Getenv("FACTORY_TASK_ID")
	}
	if task == "" {
		task = "manual"
	}
	localDev, err := s.localDev(m, f.localDev)
	if err != nil {
		return s.fail(err)
	}
	gh := s.d.GitHub
	if gh == nil {
		gh = stack.GitHub{Token: s.d.Getenv("GH_TOKEN"), HTTPClient: s.d.HTTPClient}
	}
	var (
		v   stack.StackVersions
		src stack.Source
	)
	switch {
	case f.versions != "":
		b, rerr := os.ReadFile(f.versions)
		if rerr != nil {
			return s.fail(rerr)
		}
		if v, err = stack.DecodeVersions(b); err == nil {
			v, err = stack.FillLatest(ctx, v, gh)
		}
	case f.release != "":
		if src, err = stack.ParseRelease(f.release); err == nil {
			v, err = stack.Resolve(ctx, src, gh)
		}
	default:
		src.Train = f.train
		v, err = stack.Resolve(ctx, src, gh)
	}
	if err != nil {
		return s.fail(err)
	}
	sourceName := src.String()
	if f.versions != "" {
		sourceName = "versions file " + f.versions
	}
	st := stack.State{
		Project: stack.ProjectName(task), Task: task, Profile: f.profile, LocalDev: localDev,
		Versions: v, Source: sourceName, StartedAt: s.d.Clock.Now().UTC(),
	}
	if f.dryRun {
		plan, err := stack.NewPlan(st)
		if err != nil {
			return s.fail(err)
		}
		if err := plan.Format(s.stdout, st); err != nil {
			return s.fail(err)
		}
		return 0
	}
	if err := m.WithOutput(s.stderr).Up(ctx, st); err != nil {
		return s.fail(err)
	}
	fmt.Fprintf(s.stderr, "stack %s is up (api %s, ui %s, agent %s); `factory-agent stack wait` waits for it\n",
		st.Project, v.API, v.UI, v.Agent)
	if err := stack.FormatEnv(s.stdout, m.Env(st)); err != nil {
		return s.fail(err)
	}
	return 0
}

// localDev is --local-dev, else the checkout of the stack that is up, else the config's
// localDev.
func (s *stackOp) localDev(m *stack.Manager, flagValue string) (string, error) {
	if flagValue != "" {
		p, err := config.ExpandPath(flagValue, s.home())
		if err != nil {
			return "", err
		}
		return filepath.Abs(p)
	}
	if cur, err := m.Current(); err == nil && cur.LocalDev != "" {
		return cur.LocalDev, nil
	}
	co := s.d.Config
	co.Warn = func(w string) { fmt.Fprintln(s.stderr, "warning:", w) }
	cfg, err := config.Load(s.configPath, co)
	if err != nil {
		return "", fmt.Errorf("no --local-dev, and %w", err)
	}
	if cfg.LocalDev == "" {
		return "", errors.New("no --local-dev, and the config sets no localDev")
	}
	return strings.TrimRight(cfg.LocalDev, "/"), nil
}

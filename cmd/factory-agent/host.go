package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/host"
)

// hostDeps are the host command's injectable dependencies; the zero value is production.
type hostDeps struct {
	Config config.Options
	// Env is the steps' environment (its zero fields get the production defaults).
	Env host.Env
	// Stdin answers the pf prompt; default os.Stdin.
	Stdin io.Reader
}

func hostCommand() command {
	return command{
		name:    "host",
		summary: "check or set up this machine as a VM workspace host (Tart, Softnet, pf)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runHost(ctx, args, stdout, stderr, hostDeps{})
		},
	}
}

func hostUsage(w io.Writer) {
	fmt.Fprint(w, `usage: factory-agent host <op> [flags]   (workspace-host.md)

  check [--driver tart] [--json]
        one line per step (ok, missing, outdated, wrong, skipped); exit 0 when every step is
        ok or skipped, else 1. skipped is a pf step the owner opted out of (a warning)
  init  [--driver tart] [--dry-run] [--yes] [--no-pf] [--remove-pf]
        applies the steps that aren't ok (one sudo prompt for Softnet and pf), then checks
        again: its exit code is that check's. --dry-run prints the plan and the sudo script
        and changes nothing. --yes installs pf without asking; --no-pf skips it and records
        the opt-out in the state dir (a later init without --no-pf asks again).
        --remove-pf removes the pf anchor, its LaunchDaemon and its pf.conf line.

common flags: --config <agent.yaml> (only its workspace block is read; without the file, the
defaults). The kata driver's host init comes in P1 phase 9.
`)
}

func runHost(ctx context.Context, args []string, stdout, stderr io.Writer, d hostDeps) int {
	if len(args) == 0 {
		hostUsage(stderr)
		return 2
	}
	op, args := args[0], args[1:]
	switch op {
	case "help", "-h", "--help":
		hostUsage(stdout)
		return 0
	case "check", "init":
	default:
		fmt.Fprintf(stderr, "factory-agent host: unknown op %q\n\n", op)
		hostUsage(stderr)
		return 2
	}
	fs := flag.NewFlagSet("host "+op, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "the agent config file (its workspace block)")
	driver := fs.String("driver", "", "the workspace driver: tart (default: the config's)")
	asJSON := fs.Bool("json", false, "check: print the statuses as JSON")
	var o host.InitOptions
	removePF := fs.Bool("remove-pf", false, "init: remove the pf anchor instead")
	fs.BoolVar(&o.DryRun, "dry-run", false, "init: print the plan and the sudo script; change nothing")
	fs.BoolVar(&o.Yes, "yes", false, "init: install the pf anchor without asking")
	fs.BoolVar(&o.NoPF, "no-pf", false, "init: skip the pf anchor and record the opt-out")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if fs.NArg() > 0 || op == "check" && (set["dry-run"] || set["yes"] || set["no-pf"] || set["remove-pf"]) ||
		op == "init" && set["json"] || *removePF && (o.NoPF || o.Yes) {
		fmt.Fprintf(stderr, "factory-agent host %s: unexpected arguments or flags (see factory-agent host help)\n", op)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "factory-agent host %s: %v\n", op, err)
		return 1
	}
	co := d.Config
	co.Warn = func(w string) { fmt.Fprintln(stderr, "warning:", w) }
	w, err := config.LoadWorkspace(*configPath, !set["config"], co)
	if err != nil {
		return fail(err)
	}
	if *driver != "" {
		w.Driver = *driver
	}
	if w.Driver == config.DriverKata {
		return fail(errors.New("the kata driver's host init comes in P1 phase 9"))
	}
	if w.Driver != config.DriverTart {
		fmt.Fprintf(stderr, "factory-agent host %s: --driver %q is not tart or kata\n", op, w.Driver)
		return 2
	}
	e := d.Env
	e.Workspace, e.Out = w, stdout
	if e.Prompt == nil {
		in := d.Stdin
		if in == nil {
			in = os.Stdin
		}
		e.Prompt = linePrompt(in, stderr)
	}
	if err := e.Defaults(); err != nil {
		return fail(err)
	}
	var sts []host.Status
	switch {
	case *removePF:
		if err := host.RemovePF(ctx, &e, o.DryRun); err != nil {
			return fail(err)
		}
		return 0
	case op == "init":
		sts, err = host.Init(ctx, &e, o)
		if err != nil || o.DryRun {
			_ = printStatuses(stdout, sts, false)
			warnSkipped(stderr, sts)
			if err != nil {
				return fail(err)
			}
			return 0
		}
	default:
		steps, err := host.Steps(&e)
		if err != nil {
			return fail(err)
		}
		sts = host.Check(ctx, steps)
	}
	if err := printStatuses(stdout, sts, *asJSON); err != nil {
		return fail(err)
	}
	warnSkipped(stderr, sts)
	if !host.AllOK(sts) {
		return 1
	}
	return 0
}

func printStatuses(w io.Writer, sts []host.Status, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(sts)
	}
	for _, s := range sts {
		if _, err := fmt.Fprintf(w, "%-9s %-8s %s\n", s.Step, s.State, s.Detail); err != nil {
			return err
		}
	}
	return nil
}

// warnSkipped warns about the steps the owner opted out of.
func warnSkipped(w io.Writer, sts []host.Status) {
	for _, s := range sts {
		if s.State == host.StateSkipped {
			fmt.Fprintf(w, "warning: %s skipped: %s\n", s.Step, s.Detail)
		}
	}
}

// linePrompt asks on out and reads one line from in: y or yes is a yes.
func linePrompt(in io.Reader, out io.Writer) func(string) (bool, error) {
	r := bufio.NewReader(in)
	return func(q string) (bool, error) {
		fmt.Fprint(out, q)
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes", nil
	}
}

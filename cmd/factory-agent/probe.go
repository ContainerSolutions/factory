package main

import (
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
	"github.com/compliance-framework/factory/internal/agent/login"
	"github.com/compliance-framework/factory/internal/agent/probe"
)

func probeCommand() command {
	return command{
		name:    "probe",
		summary: "print this machine's capabilities (no call to the dispatcher)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runProbe(ctx, args, stdout, stderr, probe.Options{Tooling: probe.BaseTooling{}}, config.Options{})
		},
	}
}

// runProbe loads the config, probes, and prints the report: text, or JSON with --json. Exit 1
// when supervise couldn't start with what was found. As supervise does, it offers a harness
// whose login is in the state dir (loginProbe) unless po has its own Harnesses, and it checks
// po's Tooling: the host's git, which fetches the skills.
func runProbe(ctx context.Context, args []string, stdout, stderr io.Writer, po probe.Options, co config.Options) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "the agent config file")
	stateDir := fs.String("state-dir", "", "the agent's state dir, where the logins are (default: the agent's per OS, ~/Library/Application Support/factory-agent on a Mac, $XDG_STATE_HOME/factory-agent or ~/.local/state/factory-agent on Linux)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "factory-agent probe: unexpected arguments %v\n", fs.Args())
		return 2
	}
	co.Warn = func(s string) { fmt.Fprintln(stderr, "warning:", s) }
	cfg, err := config.Load(*path, co)
	if err != nil {
		fmt.Fprintln(stderr, "factory-agent probe:", err)
		return 1
	}
	if po.Harnesses == nil {
		if *stateDir == "" {
			home := co.Home
			if home == nil {
				home = os.UserHomeDir
			}
			h, err := home()
			if err != nil {
				fmt.Fprintln(stderr, "factory-agent probe: the home directory:", err)
				return 1
			}
			*stateDir = (&host.Env{Home: h, Getenv: os.Getenv}).StateDir()
		}
		po.Harnesses = loginProbe(login.ForStateDir(*stateDir)).Harnesses
	}
	rep := probe.Run(ctx, cfg, po)
	ready, why := rep.Ready()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "factory-agent probe:", err)
			return 1
		}
	} else {
		printReport(stdout, rep)
	}
	if !ready {
		fmt.Fprintln(stderr, "factory-agent probe: supervise can't start:", strings.Join(why, "; "))
		return 1
	}
	return 0
}

func printReport(w io.Writer, rep probe.Report) {
	fmt.Fprintln(w, "harnesses:")
	if len(rep.Capabilities.Harnesses) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, h := range rep.Capabilities.Harnesses {
		models := strings.Join(h.Models, ", ")
		if h.DefaultModel != "" {
			models += " (default " + h.DefaultModel + ")"
		}
		line := fmt.Sprintf("  %s: auth %s, models %s", h.Name, orNone(h.Auth), models)
		if len(h.ReasoningEffort) > 0 {
			line += ", efforts " + strings.Join(h.ReasoningEffort, ", ")
		}
		if h.Version != "" {
			line += ", version " + h.Version
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "labels:", orNone(strings.Join(rep.Capabilities.Labels, ", ")))
	if len(rep.Dropped) > 0 {
		fmt.Fprintln(w, "dropped:")
		for _, d := range rep.Dropped {
			fmt.Fprintf(w, "  %s %s: %s\n", d.Kind, d.Name, d.Reason)
		}
	}
	if len(rep.Tooling) > 0 {
		fmt.Fprintln(w, "base tooling:")
		for _, t := range rep.Tooling {
			if t.OK {
				fmt.Fprintf(w, "  %s: ok\n", t.Name)
			} else {
				fmt.Fprintf(w, "  %s: MISSING (%s)\n", t.Name, t.Problem)
			}
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

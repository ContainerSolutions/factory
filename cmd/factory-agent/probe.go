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
	"github.com/compliance-framework/factory/internal/agent/probe"
)

// probeOptions is how the commands probe: each registered harness adapter's Probe (a
// configured harness without one is probed with probe.Configured) and the base tooling.
// Labels are advertised as configured.
func probeOptions() probe.Options {
	o := probe.Options{Tooling: probe.BaseTooling{}, Harnesses: map[string]probe.Prober{}}
	for name, mk := range harnessAdapters {
		// Probe takes the harness's config as an argument: the adapter's own is unused.
		o.Harnesses[name] = mk(config.Harness{})
	}
	return o
}

func probeCommand() command {
	return command{
		name:    "probe",
		summary: "print this machine's capabilities (no call to the dispatcher)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runProbe(ctx, args, stdout, stderr, probeOptions(), config.Options{})
		},
	}
}

// runProbe loads the config, probes, and prints the report: text, or JSON with --json. Exit 1
// when the worker couldn't start with what was found.
func runProbe(ctx context.Context, args []string, stdout, stderr io.Writer, po probe.Options, co config.Options) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "the agent config file")
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
		fmt.Fprintln(stderr, "factory-agent probe: the worker can't start:", strings.Join(why, "; "))
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

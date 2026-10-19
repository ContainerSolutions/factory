// Command factory-agent is the CCF Factory agent. Each subcommand lives in its own file and is
// registered with one line in commands below.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/compliance-framework/factory/internal/version"
)

// commands are the subcommands, one line each.
var commands = []command{
	versionCommand(),
	workerCommand(),
	probeCommand(),
	fxCommand(),
	mcpCommand(),
	stackCommand(),
}

// command is one subcommand. run gets the arguments after the subcommand's name and returns
// the process exit code.
type command struct {
	name, summary string
	run           func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

func main() {
	// Signals are each subcommand's own business: the worker drains on the first SIGTERM and
	// aborts on the second.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand; with no arguments it prints the version. Exit code 2 is a
// usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"version"}
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "factory-agent: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: factory-agent <command> [flags]")
	fmt.Fprintln(w, "\ncommands:")
	cs := append([]command(nil), commands...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	for _, c := range cs {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
}

func versionCommand() command {
	return command{
		name:    "version",
		summary: "print the version",
		run: func(_ context.Context, _ []string, stdout, _ io.Writer) int {
			fmt.Fprintln(stdout, version.String("factory-agent"))
			return 0
		},
	}
}

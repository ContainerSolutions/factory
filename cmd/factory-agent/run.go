package main

import (
	"context"
	"io"

	"github.com/ContainerSolutions/factory/internal/agent/vmrun"
)

// runCommand is the runtime inside a VM workspace (workspace-run.md §factory-agent run): the
// supervisor execs it with the run bundle in place; stdout is the event stream.
func runCommand() command {
	return command{
		name:    "run",
		summary: "run one task inside a VM workspace (the supervisor starts it)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return vmrun.Main(ctx, args, stdout, stderr)
		},
	}
}

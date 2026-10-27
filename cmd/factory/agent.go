package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

func agentCommand() command {
	return command{
		name:    "agent",
		summary: "enroll agent hosts (agent enroll; agent enrollments [list|delete <id>])",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runAgent(ctx, args, os.Getenv, stdout, stderr)
		},
	}
}

const agentUsage = "usage: factory agent enroll --pool <pool> --owner <login> --name <name> [--ttl 7d] [--server URL]\n" +
	"       factory agent enrollments [list] [--server URL]\n" +
	"       factory agent enrollments delete <id> [--server URL]"

// runAgent dispatches factory agent's subcommands. Each takes the CLI token from
// FACTORY_CLI_TOKEN (a full admin).
func runAgent(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "enroll":
			return runAgentEnroll(ctx, args[1:], getenv, stdout, stderr)
		case "enrollments":
			return runAgentEnrollments(ctx, args[1:], getenv, stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, agentUsage)
	return exitUsage
}

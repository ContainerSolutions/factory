package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/stack"
	"github.com/compliance-framework/factory/internal/clock"
)

// agentShimDeps are the agent-shim op's injectable dependencies; the zero value but Getenv
// is production.
type agentShimDeps struct {
	Getenv func(string) string
	// HTTPClient polls the stack in wait; default the stack package's.
	HTTPClient *http.Client
	Clock      clock.Clock
}

// runAgentShim is `factory-agent fx agent-shim <args>`, the workspace's bin/factory-agent
// (run-pipeline.md §Environment): the factory-agent a run sees. It has `stack env` and
// `stack wait` only, both on the run's stack as the pipeline put it in the environment
// (stack.RunEnv), so it reads no lock and no state directory; everything else (stack up and
// down, worker, …) refuses with exit 2. Output and exit codes are `factory-agent stack`'s:
// env prints KEY=VALUE lines quoted for eval, wait says on stderr when the stack is ready, a
// failure exits 1.
func runAgentShim(ctx context.Context, args []string, stdout, stderr io.Writer, d agentShimDeps) int {
	if len(args) == 0 {
		agentShimUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		agentShimUsage(stdout)
		return 0
	case "stack":
	default:
		fmt.Fprintf(stderr, "factory-agent %s: not available inside a run: a run's factory-agent has only `stack env` and `stack wait` (the worker runs everything else)\n", args[0])
		return 2
	}
	args = args[1:]
	if len(args) == 0 {
		agentShimUsage(stderr)
		return 2
	}
	op, args := args[0], args[1:]
	switch op {
	case "help", "-h", "-help", "--help":
		agentShimUsage(stdout)
		return 0
	case "env", "wait":
	default:
		fmt.Fprintf(stderr, "factory-agent stack %s: refused inside a run: the worker manages the run's stack (bring it up and down outside the run); use `factory-agent stack env` or `stack wait`\n", op)
		return 2
	}
	fs := flag.NewFlagSet("stack "+op, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var timeout time.Duration
	if op == "wait" {
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
	env := stack.RunEnv(d.Getenv)
	if op == "env" {
		if env == nil {
			fmt.Fprintln(stderr, "factory-agent stack env: this run has no stack")
			return 0
		}
		if err := stack.FormatEnv(stdout, env); err != nil {
			fmt.Fprintf(stderr, "factory-agent stack env: %v\n", err)
			return 1
		}
		return 0
	}
	if env == nil {
		fmt.Fprintf(stderr, "factory-agent stack wait: %v: this run has none (%s and %s are unset)\n", stack.ErrNoStack, stack.EnvURL, stack.EnvAPI)
		return 1
	}
	o := stack.Options{UIURL: env[stack.EnvURL], APIURL: env[stack.EnvAPI], HTTPClient: d.HTTPClient, Clock: d.Clock}
	if err := stack.WaitURLs(ctx, o, timeout); err != nil {
		fmt.Fprintf(stderr, "factory-agent stack wait: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "the stack is ready")
	return 0
}

func agentShimUsage(w io.Writer) {
	fmt.Fprint(w, strings.TrimLeft(`
usage: factory-agent stack env|wait   (inside a run; local-stack.md)

  env                    prints the run's FACTORY_STACK_URL, FACTORY_STACK_API, … as KEY=VALUE
                         lines for eval "$(factory-agent stack env)"; nothing when the run has
                         no stack
  wait [--timeout 10m]   polls the run's stack (the API's health and the UI) until both answer
                         200; exits 1 when it isn't ready in time, or the run has no stack

This is the run's factory-agent: the worker brings the run's stack up and down, and every
other command (stack up|down, worker, …) refuses (exit 2).
`, "\n"))
}

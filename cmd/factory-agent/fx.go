package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/effector"
	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness/claudecode"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
)

// fxCommand is the Effector CLI (effector.md), on a run's PATH as `factory`: the effect ops
// (internal/agent/effector), `mcp` (the same ops as a stdio MCP server), and the internal
// ops the run pipeline and the adapters need: the git credential helper, Claude Code's
// PreToolUse hook, and agent-shim, the run's `factory-agent` (stack env|wait only).
func fxCommand() command {
	return command{
		name:    "fx",
		summary: "the Effector CLI, `factory` in a run (`fx help` lists the ops)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runFx(ctx, args, os.Stdin, stdout, stderr, os.Getenv)
		},
	}
}

// runFx dispatches an Effector op. Exit codes are effector.md's: 2 usage, 3 not allowed, 4
// refused or failed, 5 git failure; claude-hook's are Claude Code's hook protocol (0 allow, 2
// block).
func runFx(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fxUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		fxUsage(stdout)
		return 0
	case "mcp":
		return runMCP(ctx, args[1:], stdin, stdout, stderr, getenv)
	case "git-credential":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: factory-agent fx git-credential get|store|erase")
			return 2
		}
		if err := gitcred.Helper(ctx, args[1:], stdin, stdout, getenv(gitcred.EnvSocket)); err != nil {
			fmt.Fprintln(stderr, "factory git-credential:", err)
			return 5
		}
		return 0
	case claudecode.HookOp:
		return claudecode.HookMain(args[1:], stdin, stderr)
	case pipeline.AgentShimOp:
		return runAgentShim(ctx, args[1:], stdout, stderr, agentShimDeps{Getenv: getenv})
	}
	if !effector.IsOp(args) {
		fmt.Fprintf(stderr, "factory-agent fx: unknown op %q\n\n", strings.Join(args, " "))
		fxUsage(stderr)
		return 2
	}
	e, err := effector.FromEnv(getenv, nil)
	if err != nil {
		fmt.Fprintln(stderr, "factory:", err)
		return 2
	}
	return effector.Main(ctx, e, args, stdin, stdout, stderr)
}

// fxUsage lists the effect ops and the internal ones.
func fxUsage(w io.Writer) {
	effector.Usage(w)
	fmt.Fprintln(w, "\ninternal ops:\n  git-credential get|store|erase   the run's git credential helper (git calls it)\n"+
		"  claude-hook pre-tool-use         Claude Code's PreToolUse hook (claude calls it)\n"+
		"  agent-shim stack env|wait        the run's `factory-agent` (bin/factory-agent calls it)")
}

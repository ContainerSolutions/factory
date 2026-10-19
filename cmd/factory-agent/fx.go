package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness/claudecode"
)

// fxCommand is the Effector CLI (effector.md), on a run's PATH as `factory`. Today it has
// the ops the run pipeline and the adapters need: the git credential helper and Claude
// Code's PreToolUse hook; AGT-S1-T05 adds the others.
func fxCommand() command {
	return command{
		name:    "fx",
		summary: "the Effector CLI, `factory` in a run (today: git-credential, claude-hook)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runFx(ctx, args, os.Stdin, stdout, stderr, os.Getenv)
		},
	}
}

// runFx dispatches an Effector op. Exit codes are effector.md's: 2 usage, 5 git failure;
// claude-hook's are Claude Code's hook protocol (0 allow, 2 block).
func runFx(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: factory-agent fx <op> [flags]\n\nops:\n  git-credential get|store|erase   the run's git credential helper (git calls it)\n"+
			"  claude-hook pre-tool-use         Claude Code's PreToolUse hook (claude calls it)")
		return 2
	}
	switch args[0] {
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
	}
	fmt.Fprintf(stderr, "factory-agent fx: unknown op %q\n", args[0])
	return 2
}

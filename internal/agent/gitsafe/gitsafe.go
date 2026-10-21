// Package gitsafe is the git config that keeps the worker's own git (the pipeline's clones,
// the Effector's pushes and rebases) from running code planted in a checkout: the agent can
// write the task checkout's .git, and git run outside the harness's sandbox there would
// otherwise run its hooks (.git/hooks, core.hooksPath) and core.fsmonitor.
//
// The pairs are command-line config: git reads that scope last, so it wins over the
// system, global, repo and worktree config. The harness's own git doesn't get them: it runs
// in the sandbox, where a repo's own hooks keep working.
package gitsafe

import (
	"fmt"
	"strconv"
	"strings"
)

// Config are the overriding pairs. /dev/null is no directory, so git finds no hook there;
// core.fsmonitor=false is the boolean (git 2.36+; the Effector's `rebase --update-refs`
// already needs 2.38).
func Config() [][2]string {
	return [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"core.fsmonitor", "false"},
	}
}

// Args is args with Config prepended as `-c key=value` options, for a git command line.
// Git hands them on to the git processes it starts (GIT_CONFIG_PARAMETERS).
func Args(args ...string) []string {
	cfg := Config()
	out := make([]string, 0, 2*len(cfg)+len(args))
	for _, p := range cfg {
		out = append(out, "-c", p[0]+"="+p[1])
	}
	return append(out, args...)
}

// Env is env with Config added as process-scoped config (GIT_CONFIG_COUNT/KEY_n/VALUE_n),
// for a program that runs git itself and takes no `-c` (gh extensions). The pairs go after
// any already there (the credential helper's): GIT_CONFIG_COUNT is raised, never reset. A
// missing or unparsable count counts as 0, as git would refuse it anyway. env isn't
// modified.
func Env(env []string) []string {
	n := 0
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			c, err := strconv.Atoi(v)
			if err != nil || c < 0 {
				c = 0
			}
			n = c // the last one wins, as in exec.Cmd
		}
	}
	cfg := Config()
	// Our indexes may hold stale entries past the old count: drop them, and the old count.
	stale := map[string]bool{"GIT_CONFIG_COUNT": true}
	for i := range cfg {
		stale[fmt.Sprintf("GIT_CONFIG_KEY_%d", n+i)] = true
		stale[fmt.Sprintf("GIT_CONFIG_VALUE_%d", n+i)] = true
	}
	out := make([]string, 0, len(env)+1+2*len(cfg))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !stale[k] {
			out = append(out, kv)
		}
	}
	for i, p := range cfg {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, p[1]))
	}
	return append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+len(cfg)))
}

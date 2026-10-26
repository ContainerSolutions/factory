package katahelper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// Protocol is the helper's verbs' version: bump it whenever a verb's arguments or behaviour
// change. host check compares the installed helper's with this binary's, so a new
// factory-agent needs sudo again only when the helper changed.
const Protocol = 1

// Env is the whole environment of the commands the helper runs: nothing comes from the
// caller's. NERDCTL_TOML names /dev/null, so no /etc/nerdctl/nerdctl.toml changes nerdctl's
// defaults.
var Env = []string{"PATH=" + Path, "HOME=/root", "NERDCTL_TOML=/dev/null"}

// NerdctlGlobal are nerdctl's global flags for the factory's containerd, before every verb.
var NerdctlGlobal = []string{
	"--address", ContainerdSocket, "--namespace", Namespace, "--data-root", NerdctlDataRoot,
	"--cni-path", CNIPluginDir, "--cni-netconfpath", CNIConfDir,
}

// listTimeout bounds list's nerdctl: a hung containerd mustn't hang the caller's sudo.
const listTimeout = time.Minute

// Runner runs a command with exactly env as its environment.
type Runner interface {
	Run(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error
}

// ExecRunner runs real programs.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = env, "/"
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

// Main runs one verb: kata-helper <verb> [args]. It returns the exit code: 0 on success, 2 for
// usage, 1 for any other failure (with one line on stderr).
func Main(args []string, stdout, stderr io.Writer) int {
	return Run(context.Background(), ExecRunner{}, args, stdout, stderr)
}

// errUsage is a usage error: exit 2.
var errUsage = errors.New("usage: kata-helper protocol | list")

// Run is Main with its runner injected.
func Run(ctx context.Context, r Runner, args []string, stdout, stderr io.Writer) int {
	err := dispatch(ctx, r, args, stdout)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintf(stderr, "%s %s: %s\n", Name, args[0], oneLine(err.Error()))
	return 1
}

func dispatch(ctx context.Context, r Runner, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch verb, rest := args[0], args[1:]; verb {
	case "protocol":
		if len(rest) > 0 {
			return errUsage
		}
		_, err := fmt.Fprintln(stdout, Protocol)
		return err
	case "list":
		if len(rest) > 0 {
			return errUsage
		}
		return list(ctx, r, stdout)
	}
	return errUsage
}

// list prints the workspaces in the factory namespace, one name per line: only names that are
// workspace.ValidName's, so nothing else in the namespace (the warm build's, a person's) is
// ever listed.
func list(ctx context.Context, r Runner, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	args := append(append([]string{}, NerdctlGlobal...), "ps", "-a", "--format", "{{.Names}}")
	if err := r.Run(ctx, Env, &out, &errOut, Nerdctl, args...); err != nil {
		if l := lastLine(errOut.String()); l != "" {
			return fmt.Errorf("nerdctl ps: %w: %s", err, l)
		}
		return fmt.Errorf("nerdctl ps: %w", err)
	}
	var names []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		if n := strings.TrimSpace(sc.Text()); workspace.ValidName(n) == nil {
			names = append(names, n)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("nerdctl ps's output: %w", err)
	}
	for _, n := range names {
		if _, err := fmt.Fprintln(stdout, n); err != nil {
			return err
		}
	}
	return nil
}

// lastLine is s's last non-empty line: nerdctl prints its error last.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// oneLine keeps an error to one line on stderr: control characters become spaces.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

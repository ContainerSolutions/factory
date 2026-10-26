package katahelper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// Protocol is the helper's verbs' version: bump it whenever a verb's arguments or behaviour
// change. host check compares the installed helper's with this binary's, so a new
// factory-agent needs sudo again only when the helper changed.
//
// 2: create and rm, a workspace's slot, network, disk and container.
// 3: start, stop, inspect and exec.
// 4: exec runs as admin (3's ran as root).
// 5: image-build, image-exists and image-rm.
// 6: warm-load, warm-promote and warm-exists.
const Protocol = 6

// Env is the whole environment of the commands the helper runs: nothing comes from the
// caller's. NERDCTL_TOML names /dev/null, so no /etc/nerdctl/nerdctl.toml changes nerdctl's
// defaults.
var Env = []string{"PATH=" + Path, "HOME=/root", "NERDCTL_TOML=/dev/null"}

// NerdctlGlobal are nerdctl's global flags for the factory's containerd, before every verb.
var NerdctlGlobal = []string{
	"--address", ContainerdSocket, "--namespace", Namespace, "--data-root", NerdctlDataRoot,
	"--cni-path", CNIPluginDir, "--cni-netconfpath", CNIConfDir,
}

// cmdTimeout bounds each command the helper runs but exec's: a hung containerd mustn't hang
// the caller's sudo.
const cmdTimeout = 2 * time.Minute

// Runner runs a command with exactly env as its environment; stdin may be nil.
type Runner interface {
	Run(ctx context.Context, env []string, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

// ExecRunner runs real programs.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, env []string, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = env, "/"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

// Options is what a verb runs with; Main's are the real ones.
type Options struct {
	Runner Runner
	// Root prefixes every path the helper reads or writes itself (its slot files, conflists,
	// lock, the disks it checks and removes, /sys): "" but in tests. The commands it runs get
	// the real paths.
	Root           string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Bound is rm's limit on each nerdctl stop and rm -f (60 s); KillWait its pause after
	// killing a hung workspace's processes (2 s).
	Bound, KillWait time.Duration
}

// Main runs one verb: kata-helper <verb> [args]. It returns the exit code: 0 on success, 2 for
// usage, 1 for any other failure (with one line on stderr); exec's are execVerb's.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx := context.Background()
	if len(args) > 0 && args[0] == "exec" {
		// a signal stops nerdctl exec and the helper says so, rather than leaving it behind.
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		defer stop()
	}
	return Run(ctx, Options{Runner: ExecRunner{}, Stdin: stdin, Stdout: stdout, Stderr: stderr}, args)
}

// Usage is the helper's usage line.
const Usage = "usage: kata-helper protocol | list | create <name> <cpus> <memMiB> <diskGiB> warm|empty <image> | start|stop|rm|inspect <name> | exec [-i] <name> <argv…>" +
	" | image-build <image> < context.tar | image-exists|image-rm <image> | warm-load <name> <ref> | warm-promote <name> | warm-exists"

// usageError is a usage error: exit 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

var errUsage = &usageError{Usage}

// badArg is an invalid argument: a usage error naming it.
func badArg(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

// Run is Main with its options injected.
func Run(ctx context.Context, o Options, args []string) int {
	if o.Bound == 0 {
		o.Bound = time.Minute
	}
	if o.KillWait == 0 {
		o.KillWait = 2 * time.Second
	}
	h := &helper{o: o}
	if len(args) > 0 && args[0] == "exec" {
		return h.execVerb(ctx, args[1:])
	}
	err := h.dispatch(ctx, args)
	var ue *usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(o.Stderr, Usage)
		return 2
	case errors.As(err, &ue):
		fmt.Fprintf(o.Stderr, "%s %s: %s\n", Name, args[0], oneLine(ue.msg))
		return 2
	}
	fmt.Fprintf(o.Stderr, "%s %s: %s\n", Name, args[0], oneLine(err.Error()))
	return 1
}

// helper is one verb's run.
type helper struct{ o Options }

func (h *helper) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "protocol", "list", "warm-exists":
		if len(rest) > 0 {
			return errUsage
		}
		switch verb {
		case "list":
			return h.list(ctx)
		case "warm-exists":
			return h.warmExists()
		}
		_, err := fmt.Fprintln(h.o.Stdout, Protocol)
		return err
	case "image-build", "image-exists", "image-rm":
		if len(rest) != 1 {
			return errUsage
		}
		if err := validImage(rest[0]); err != nil {
			return err
		}
		switch verb {
		case "image-build":
			return h.imageBuild(ctx, rest[0])
		case "image-exists":
			return h.imageExists(ctx, rest[0])
		}
		return h.imageRm(ctx, rest[0])
	case "warm-load":
		if len(rest) != 2 {
			return errUsage
		}
		if err := workspace.ValidName(rest[0]); err != nil {
			return badArg("%v", err)
		}
		if err := validRef(rest[1]); err != nil {
			return err
		}
		return h.warmLoad(ctx, rest[0], rest[1])
	case "create":
		if len(rest) != 6 {
			return errUsage
		}
		a, err := parseCreate(rest)
		if err != nil {
			return err
		}
		return h.create(ctx, a)
	case "start", "stop", "rm", "inspect", "warm-promote":
		if len(rest) != 1 {
			return errUsage
		}
		name := rest[0]
		if err := workspace.ValidName(name); err != nil {
			return badArg("%v", err)
		}
		switch verb {
		case "start":
			return h.nerdctl(ctx, nil, "start", name)
		case "stop":
			return h.nerdctl(ctx, nil, "stop", "-t", "30", name)
		case "rm":
			return h.rm(ctx, name)
		case "warm-promote":
			return h.warmPromote(ctx, name)
		}
		return h.inspect(ctx, name)
	}
	return errUsage
}

// run runs a program with the helper's environment, bounded by timeout, and returns its
// stdout; a failure names the program and carries stderr's last line.
func (h *helper) run(ctx context.Context, timeout time.Duration, what, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errOut bytes.Buffer
	if err := h.o.Runner.Run(ctx, Env, nil, &out, &errOut, name, args...); err != nil {
		if l := lastLine(errOut.String()); l != "" {
			err = fmt.Errorf("%w: %s", err, l)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w (timed out after %s)", err, timeout)
		}
		return out.String(), fmt.Errorf("%s: %w", what, err)
	}
	return out.String(), nil
}

// nerdctl runs nerdctl with the global flags, bounded by cmdTimeout; stdout gets its output
// when it's set.
func (h *helper) nerdctl(ctx context.Context, stdout io.Writer, args ...string) error {
	out, err := h.run(ctx, cmdTimeout, "nerdctl "+args[0], Nerdctl, append(append([]string{}, NerdctlGlobal...), args...)...)
	if err == nil && stdout != nil {
		_, err = io.WriteString(stdout, out)
	}
	return err
}

// list prints the workspaces in the factory namespace, one name per line: only names that are
// workspace.ValidName's, so nothing else in the namespace (a person's) is ever listed.
func (h *helper) list(ctx context.Context) error {
	var out bytes.Buffer
	if err := h.nerdctl(ctx, &out, "ps", "-a", "--format", "{{.Names}}"); err != nil {
		return err
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
		if _, err := fmt.Fprintln(h.o.Stdout, n); err != nil {
			return err
		}
	}
	return nil
}

// path is p under Root.
func (h *helper) path(p string) string { return h.o.Root + p }

// exists reports whether p (under Root) exists, not following a final symlink.
func (h *helper) exists(p string) (bool, error) {
	_, err := os.Lstat(h.path(p))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
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

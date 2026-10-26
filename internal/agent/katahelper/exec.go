package katahelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"

	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// ExecFailed is exec's exit status for a failure of its own or nerdctl's, with ExecFailedPrefix
// starting its last stderr line; any other status is the remote command's.
const (
	ExecFailed       = 125
	ExecFailedPrefix = Name + ": "
)

// execStatusRE is nerdctl's last stderr line when the remote command exits non-zero: nerdctl
// exits 1 and prints the status only here (Q8 H01).
var execStatusRE = regexp.MustCompile(`exec failed with exit code ([0-9]+)"?$`)

// execVerb runs exec [-i] <name> <argv…>: nerdctl exec, with stdin passed through for -i and
// stdout and stderr always. It returns the remote command's exit status, read from nerdctl's
// last stderr line; for any other failure, its own or nerdctl's (usage included), ExecFailed
// with ExecFailedPrefix and the error as the last stderr line. The VM can forge nerdctl's line,
// which only changes how its own failure is reported.
func (h *helper) execVerb(ctx context.Context, args []string) int {
	tail := &tailBuffer{n: 4 << 10}
	code, err := h.exec(ctx, args, tail)
	if err != nil {
		// the line must be the last one, whatever the remote command left unterminated.
		if len(tail.b) > 0 && tail.b[len(tail.b)-1] != '\n' {
			fmt.Fprintln(h.o.Stderr)
		}
		fmt.Fprintf(h.o.Stderr, "%s%s\n", ExecFailedPrefix, oneLine(err.Error()))
		return ExecFailed
	}
	return code
}

// exec runs nerdctl exec with stderr's end kept in tail.
func (h *helper) exec(ctx context.Context, args []string, tail *tailBuffer) (int, error) {
	var stdin io.Reader
	nargs := append(append([]string{}, NerdctlGlobal...), "exec")
	if len(args) > 0 && args[0] == "-i" {
		stdin, nargs, args = h.o.Stdin, append(nargs, "-i"), args[1:]
	}
	if len(args) < 2 {
		return 0, errUsage
	}
	if err := workspace.ValidName(args[0]); err != nil {
		return 0, err
	}
	// nerdctl exec stops parsing its flags at the container's name: argv is passed as it is.
	nargs = append(nargs, args...)
	err := h.o.Runner.Run(ctx, Env, stdin, h.o.Stdout, io.MultiWriter(h.o.Stderr, tail), Nerdctl, nargs...)
	if err == nil {
		return 0, nil
	}
	if exitedWith(err, 1) {
		if m := execStatusRE.FindStringSubmatch(lastLine(string(tail.b))); m != nil {
			if code, cerr := strconv.Atoi(m[1]); cerr == nil && code > 0 && code < 256 {
				return code, nil
			}
		}
	}
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		err = fmt.Errorf("%w (%w)", err, cerr)
	}
	return 0, fmt.Errorf("nerdctl exec: %w", err)
}

// tailBuffer keeps the last n bytes written to it.
type tailBuffer struct {
	b []byte
	n int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.n {
		t.b = append(t.b[:0], t.b[len(t.b)-t.n:]...)
	}
	return len(p), nil
}

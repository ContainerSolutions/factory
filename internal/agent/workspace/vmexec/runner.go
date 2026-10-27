package vmexec

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/host"
	"github.com/ContainerSolutions/factory/internal/agent/sudocancel"
)

// Cmd is one invocation of a driver's command (tart, or sudo and the kata helper).
type Cmd struct {
	Name string
	Args []string
	// Env is added to the environment (a later entry wins).
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner runs a driver's commands. Tests use a fake; nothing in them runs the real ones.
type Runner interface {
	// Run runs c to completion; ctx being done kills it (a sudo call gets SIGTERM first).
	Run(ctx context.Context, c Cmd) error
	// Start starts c in its own process group, not tied to a context: its owner stops it.
	Start(c Cmd) (host.Process, error)
}

// ExecRunner runs real programs.
type ExecRunner struct {
	// SudoGrace is how long a cancelled sudo call (the kata helper's verbs) gets after SIGTERM
	// before SIGKILL. Default: sudocancel.HelperGrace.
	SudoGrace time.Duration
}

func (c Cmd) apply(cmd *exec.Cmd) {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
}

// Run implements Runner. When ctx ends, a sudo call gets SIGTERM, which sudo relays to the kata
// helper, and SIGKILL only after SudoGrace; anything else (tart) gets SIGKILL (sudocancel.Apply).
func (r ExecRunner) Run(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	c.apply(cmd)
	sudocancel.Apply(cmd, cmp.Or(r.SudoGrace, sudocancel.HelperGrace))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", c.Name, err)
	}
	return nil
}

// Start implements Runner, with host.StartCmd: Stop sends the group SIGTERM, then SIGKILL.
func (ExecRunner) Start(c Cmd) (host.Process, error) {
	cmd := exec.Command(c.Name, c.Args...)
	c.apply(cmd)
	return host.StartCmd(cmd)
}

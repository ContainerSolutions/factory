package host

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Spawner runs the commands a VM build needs besides Runner's: one fed on stdin, and one left
// running in the background (tart run).
type Spawner interface {
	// RunInput runs a command with in as its stdin; out gets its stdout and stderr.
	RunInput(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error
	// Start starts a command in its own process group and returns without waiting for it.
	Start(ctx context.Context, out io.Writer, name string, args ...string) (Process, error)
}

// Process is a started command.
type Process interface {
	// Exited is closed once the process has exited.
	Exited() <-chan struct{}
	// Err is how it exited (nil for status 0); valid once Exited is closed.
	Err() error
	// Stop ends the process's group (SIGTERM, then SIGKILL after stopGrace) and waits for it.
	// Stopping an exited process does nothing.
	Stop()
}

// stopGrace is how long Stop waits after SIGTERM: tart run shuts the VM down on it.
const stopGrace = 30 * time.Second

// RunInput implements Spawner.
func (ExecRunner) RunInput(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Start implements Spawner. The process isn't tied to ctx: its owner stops it, also after ctx
// is done (an interrupt), so the VM gets SIGTERM and not a kill.
func (ExecRunner) Start(_ context.Context, out io.Writer, name string, args ...string) (Process, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	return StartCmd(cmd)
}

// StartCmd starts cmd, which must not be tied to a context, in its own process group, and
// returns it as a Process (the tart workspace driver's tart run and tart exec use it too).
func StartCmd(cmd *exec.Cmd) (Process, error) {
	// the output pipe can outlive the process when a child keeps it open.
	cmd.WaitDelay = 10 * time.Second
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(cmd.Path), err)
	}
	p := &execProcess{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // set before done is closed
	once sync.Once
}

func (p *execProcess) Exited() <-chan struct{} { return p.done }

func (p *execProcess) Err() error { <-p.done; return p.err }

func (p *execProcess) Stop() {
	p.once.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		signalGroup(p.cmd, false)
		select {
		case <-p.done:
		case <-time.After(stopGrace):
			signalGroup(p.cmd, true)
			<-p.done
		}
	})
}

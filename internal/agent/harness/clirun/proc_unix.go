//go:build unix

package clirun

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// inOwnGroup starts cmd as the leader of a new process group, so the commands the harness starts
// can be killed with it.
func inOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks the process group led by pid to stop (SIGTERM).
func terminateGroup(pid int) error { return signalGroup(pid, syscall.SIGTERM) }

// killGroup kills what is left of the process group led by pid (SIGKILL).
func killGroup(pid int) error { return signalGroup(pid, syscall.SIGKILL) }

func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// detachedGroups are the process groups of the harness's descendants that left its group. A
// command the harness starts in a session of its own (opencode's shell tool starts every
// command so) leads a group the harness's group signal misses: a build would run on after the
// harness was killed and keep the workspace busy while the run wraps up. They are found when
// the run is cancelled, while the harness still lives: once it has exited, they are no longer
// its descendants.
type detachedGroups struct {
	mu     sync.Mutex
	groups []int
}

// terminate finds the groups of pid's descendants (descendantGroups) and asks them to stop
// (SIGTERM).
func (d *detachedGroups) terminate(pid int) {
	groups := descendantGroups(pid)
	d.mu.Lock()
	d.groups = groups
	d.mu.Unlock()
	for _, g := range groups {
		_ = signalGroup(g, syscall.SIGTERM)
	}
}

// kill kills what is left of the groups terminate found (SIGKILL).
func (d *detachedGroups) kill() {
	d.mu.Lock()
	groups := d.groups
	d.mu.Unlock()
	for _, g := range groups {
		_ = signalGroup(g, syscall.SIGKILL)
	}
}

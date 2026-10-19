//go:build unix

package clirun

import (
	"errors"
	"os/exec"
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

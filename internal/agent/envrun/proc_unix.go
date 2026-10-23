//go:build unix

package envrun

import (
	"errors"
	"os/exec"
	"syscall"
)

// inOwnGroup starts cmd as the leader of a new process group, so a cancelled script's commands
// stop with it.
func inOwnGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// terminateGroup sends SIGTERM to the process group led by pid.
func terminateGroup(pid int) error { return signalGroup(pid, syscall.SIGTERM) }

// killGroup sends SIGKILL to what is left of the process group led by pid.
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

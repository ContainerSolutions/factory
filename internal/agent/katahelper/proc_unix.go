//go:build unix

package katahelper

import (
	"errors"
	"os/exec"
	"syscall"
)

// ownGroup starts cmd as the leader of a new process group.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup kills the process group cmd leads (SIGKILL); a group that's gone is no error.
func killGroup(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

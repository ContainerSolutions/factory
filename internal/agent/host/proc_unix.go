//go:build unix

package host

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in its own process group: a Ctrl-C in the terminal reaches only
// factory-agent, which then stops the group itself.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// signalGroup sends SIGTERM (SIGKILL with kill) to cmd's process group.
func signalGroup(cmd *exec.Cmd, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

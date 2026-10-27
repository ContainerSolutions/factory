// Package sudocancel is how factory-agent cancels a command it runs with a context: a sudo call
// gets SIGTERM, which sudo relays to its command, and SIGKILL only after a grace; anything else
// gets SIGKILL at once. host init, the kata workspace driver and the in-VM runtime share it.
//
// sudo can't relay a SIGKILL, so its command, root and out of factory-agent's reach, would run
// on to its own bounds: the kata helper's nerdctl, a root script's install. On SIGTERM, sudo
// also restores the terminal's echo when it's interrupted at its password prompt.
//
// sudo stays in factory-agent's process group, so a terminal's Ctrl-C or hangup reaches it as
// well, and it relays those too. It ignores a signal sent from a process group that sudo or its
// command leads (sudo 1.9.15's exec_nopty.c and exec_pty.c), never factory-agent's.
package sudocancel

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// HelperGrace is the kata helper's: it stops its nerdctl and removes what it made (an
// image-build's context copy) in well under it.
const HelperGrace = 30 * time.Second

// OutputDelay is how long Wait waits, after a command that isn't sudo is killed, for a child
// that holds its output open.
const OutputDelay = 10 * time.Second

// Apply sets what cmd, made by exec.CommandContext, does when its context ends: SIGTERM, then
// SIGKILL after grace, when its program is sudo (with or without -n); else SIGKILL, and
// OutputDelay for its output. Call it before cmd starts.
func Apply(cmd *exec.Cmd, grace time.Duration) {
	cmd.WaitDelay = OutputDelay
	if IsSudo(cmd) {
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = grace
	}
}

// IsSudo reports whether cmd's program is sudo.
func IsSudo(cmd *exec.Cmd) bool { return filepath.Base(cmd.Path) == "sudo" }

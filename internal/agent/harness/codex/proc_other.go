//go:build !unix

package codex

import (
	"os"
	"os/exec"
)

// Without process groups only codex itself is signalled.

func inOwnGroup(*exec.Cmd) {}

func terminateGroup(pid int) error { return killGroup(pid) }

func killGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = p.Kill()
	return nil
}

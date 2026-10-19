//go:build !unix

package clirun

import (
	"os"
	"os/exec"
)

// Without process groups only the harness itself is signalled.

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

//go:build !unix

package envrun

import (
	"os"
	"os/exec"
)

// Without process groups only the script itself is signalled.

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

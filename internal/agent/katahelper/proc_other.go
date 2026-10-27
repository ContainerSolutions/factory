//go:build !unix

package katahelper

import "os/exec"

// ownGroup: without process groups, the command runs as it is.
func ownGroup(*exec.Cmd) {}

// killGroup kills the command alone.
func killGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }

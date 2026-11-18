//go:build unix && !linux

package clirun

// descendantGroups finds nothing without /proc: only the harness's own group is signalled. The
// harnesses run in Linux workspaces.
func descendantGroups(int) []int { return nil }

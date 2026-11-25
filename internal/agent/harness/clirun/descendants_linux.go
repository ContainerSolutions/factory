package clirun

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// descendantGroups are the process groups of pid's descendants, read from /proc, but pid's own
// group and the caller's: the groups of the commands that left the group pid leads. nil when
// /proc can't be read.
func descendantGroups(pid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	group := map[int]int{}
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // it has exited
		}
		if ppid, pgrp, ok := parseStat(b); ok {
			children[ppid] = append(children[ppid], p)
			group[p] = pgrp
		}
	}
	seen := map[int]bool{pid: true, syscall.Getpgrp(): true}
	var groups []int
	visited := map[int]bool{pid: true}
	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		for _, c := range children[queue[0]] {
			if visited[c] {
				continue
			}
			visited[c] = true
			queue = append(queue, c)
			if g := group[c]; g > 1 && !seen[g] {
				seen[g] = true
				groups = append(groups, g)
			}
		}
	}
	return groups
}

// parseStat reads the parent and the process group from a /proc/<pid>/stat line,
// "<pid> (<comm>) <state> <ppid> <pgrp> …", where comm may hold spaces and parentheses: the
// fields follow its last ')'.
func parseStat(b []byte) (ppid, pgrp int, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 3 {
		return 0, 0, false
	}
	ppid, err := strconv.Atoi(string(f[1]))
	if err != nil {
		return 0, 0, false
	}
	pgrp, err = strconv.Atoi(string(f[2]))
	if err != nil {
		return 0, 0, false
	}
	return ppid, pgrp, true
}

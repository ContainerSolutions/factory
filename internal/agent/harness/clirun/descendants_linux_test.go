package clirun

import "testing"

func TestParseStat(t *testing.T) {
	for _, tt := range []struct {
		line       string
		ppid, pgrp int
		ok         bool
	}{
		{"4242 (node) S 4200 4242 4242 0 -1 4194560 1210", 4200, 4242, true},
		{"77 (a (b) c) R 1 70 70 0", 1, 70, true}, // comm holds spaces and parentheses
		{"77 (node) S 1", 0, 0, false},
		{"77 (node) S x 70 70", 0, 0, false},
		{"no comm", 0, 0, false},
	} {
		ppid, pgrp, ok := parseStat([]byte(tt.line))
		if ppid != tt.ppid || pgrp != tt.pgrp || ok != tt.ok {
			t.Errorf("parseStat(%q) = %d, %d, %v; want %d, %d, %v", tt.line, ppid, pgrp, ok, tt.ppid, tt.pgrp, tt.ok)
		}
	}
}

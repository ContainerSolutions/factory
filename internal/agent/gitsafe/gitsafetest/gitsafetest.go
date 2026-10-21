// Package gitsafetest plants, in a checkout, the code an agent could leave for the worker's
// git to run, so tests can check it never runs.
package gitsafetest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Vector is a way a checkout's .git makes git run a program.
type Vector int

const (
	// HooksDir: executable hooks in .git/hooks.
	HooksDir Vector = 1 << iota
	// HooksPath: core.hooksPath in .git/config, naming a directory of hooks.
	HooksPath
	// FSMonitor: core.fsmonitor in .git/config, naming a script.
	FSMonitor
	// All plants every vector (core.hooksPath shadows .git/hooks, so a firing hook comes
	// from the core.hooksPath directory).
	All = HooksDir | HooksPath | FSMonitor
)

// Hooks are the hooks planted: what push, fetch, checkout, commit, merge and rebase run.
var Hooks = []string{
	"pre-push", "post-checkout", "pre-commit", "commit-msg", "post-commit", "post-merge",
	"pre-rebase", "post-rewrite", "reference-transaction", "pre-auto-gc", "post-index-change",
}

// Trap is a planted checkout.
type Trap struct {
	marker string
}

// Plant plants the vectors in the checkout at repo (its .git directory): each planted
// program appends "<vector>:<name>" to a marker file outside the checkout.
func Plant(t testing.TB, repo string, v Vector) *Trap {
	t.Helper()
	tr := &Trap{marker: filepath.Join(t.TempDir(), "fired")}
	gitDir := filepath.Join(repo, ".git")
	script := func(path, tag string) {
		t.Helper()
		body := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(tag) + " >> " + shellQuote(tr.marker) + "\nexit 0\n"
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if v&HooksDir != 0 {
		for _, h := range Hooks {
			script(filepath.Join(gitDir, "hooks", h), "hooks:"+h)
		}
	}
	var cfg strings.Builder
	if v&HooksPath != 0 {
		dir := filepath.Join(t.TempDir(), "planted-hooks")
		for _, h := range Hooks {
			script(filepath.Join(dir, h), "hooksPath:"+h)
		}
		cfg.WriteString("\thooksPath = " + strconv.Quote(dir) + "\n")
	}
	if v&FSMonitor != 0 {
		p := filepath.Join(t.TempDir(), "fsmonitor.sh")
		script(p, "fsmonitor")
		cfg.WriteString("\tfsmonitor = " + strconv.Quote(p) + "\n")
	}
	if cfg.Len() > 0 {
		f, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("[core]\n" + cfg.String()); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

// Fired lists what ran, in order (nil when nothing did).
func (tr *Trap) Fired(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile(tr.marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// Reset forgets what ran so far.
func (tr *Trap) Reset(t testing.TB) {
	t.Helper()
	if err := os.Remove(tr.marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

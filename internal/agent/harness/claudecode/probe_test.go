package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// recCommands records Probe's commands and answers them.
type recCommands struct {
	mu   sync.Mutex
	out  string
	err  error
	envs [][]string
	args [][]string
}

func (c *recCommands) Output(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, env)
	c.args = append(c.args, append([]string{name}, args...))
	return []byte(c.out), c.err
}

func TestProbe(t *testing.T) {
	t.Parallel()
	signedIn := ownerConfigDir(t) // Linux layout: .credentials.json
	macOS := t.TempDir()          // the login in the Keychain: only .claude.json
	if err := os.WriteFile(filepath.Join(macOS, stateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	cfg := func(dir string) config.Harness {
		return config.Harness{ConfigDir: dir, Models: []string{"claude-sonnet-5-5", "claude-opus-5-5"}, Efforts: []string{"low", "max"}, Default: "claude-sonnet-5-5"}
	}
	base := []string{"PATH=/usr/bin", "HOME=/Users/owner", "LANG=C"}
	tests := []struct {
		name     string
		version  string
		goos     string
		env      []string
		cfg      config.Harness
		wantAuth string
		wantErr  string
	}{
		{"subscription, the login in a file", "2.1.3 (Claude Code)", "linux", base, cfg(signedIn), AuthSubscription, ""},
		{"subscription on macOS", "2.4.0 (Claude Code)", "darwin", base, cfg(macOS), AuthSubscription, ""},
		{"subscription token", "2.1.3", "linux", append(slices.Clone(base), "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-x"), cfg(empty), AuthSubscription, ""},
		{"API key", "2.1.3", "linux", append(slices.Clone(base), "ANTHROPIC_API_KEY=sk-ant-api03-worker-key"), cfg(signedIn), AuthAPIKey, ""},
		{"macOS state file isn't a login elsewhere", "2.1.3", "linux", base, cfg(macOS), "", "not signed in"},
		{"not signed in", "2.1.3", "darwin", base, cfg(empty), "", "not signed in"},
		{"too old", "2.0.14 (Claude Code)", "linux", base, cfg(signedIn), "", "older than " + MinVersion},
		{"no version", "Claude Code dev", "linux", base, cfg(signedIn), "", "no version"},
		{"no config dir", "2.1.3", "linux", base, config.Harness{Models: []string{"m"}}, "", "absolute"},
		{"missing config dir", "2.1.3", "linux", base, cfg(filepath.Join(empty, "nope")), "", "doesn't exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmds := &recCommands{out: tt.version + "\n"}
			env := tt.env
			a := New(Options{LookPath: sandboxed, Commands: cmds, GOOS: tt.goos, Environ: func() []string { return env }})
			off, err := a.Probe(context.Background(), tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Probe = %+v, %v; want an error with %q", off, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if off.Name != "claude-code" || off.Auth != tt.wantAuth || off.DefaultModel != "claude-sonnet-5-5" ||
				!slices.Equal(off.Models, []string{"claude-sonnet-5-5", "claude-opus-5-5"}) || !slices.Equal(off.ReasoningEffort, []string{"low", "max"}) {
				t.Errorf("offer %+v", off)
			}
			if !strings.HasPrefix(tt.version, off.Version) {
				t.Errorf("version %q from %q", off.Version, tt.version)
			}
			// Only `claude --version` ran, with no credential in its environment.
			if len(cmds.args) != 1 || !slices.Equal(cmds.args[0], []string{"claude", "--version"}) {
				t.Errorf("commands %q", cmds.args)
			}
			if !slices.Equal(cmds.envs[0], append(slices.Clone(base), "CLAUDE_CONFIG_DIR="+tt.cfg.ConfigDir)) {
				t.Errorf("probe env %q", cmds.envs[0])
			}
		})
	}
}

func TestProbeDefaultsEfforts(t *testing.T) {
	t.Parallel()
	off, err := New(Options{LookPath: sandboxed, Commands: &recCommands{out: "2.1.3"}, GOOS: "linux", Environ: func() []string { return nil }}).
		Probe(context.Background(), config.Harness{ConfigDir: ownerConfigDir(t), Models: []string{"claude-sonnet-5-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(off.ReasoningEffort, config.Efforts()) {
		t.Errorf("efforts %v", off.ReasoningEffort)
	}
}

// TestProbeWithTheFakeBinary runs Probe's real command path against the fake claude.
func TestProbeWithTheFakeBinary(t *testing.T) {
	t.Parallel()
	fc := newFakeClaude(t, fakePlan{Version: "2.2.0 (Claude Code)"})
	off, err := New(Options{LookPath: sandboxed, Binary: fc.Binary, GOOS: "linux", Environ: os.Environ}).
		Probe(context.Background(), config.Harness{ConfigDir: ownerConfigDir(t), Models: []string{"claude-sonnet-5-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if off.Version != "2.2.0" {
		t.Errorf("version %q", off.Version)
	}
}

// TestProbeWithoutTheSandbox: a machine without claude's OS sandbox doesn't offer the
// harness (the worker then takes no claude-code task).
func TestProbeWithoutTheSandbox(t *testing.T) {
	t.Parallel()
	cmds := &recCommands{out: "2.1.3"}
	_, err := New(Options{Commands: cmds, GOOS: "linux", Environ: func() []string { return nil },
		LookPath: func(n string) (string, error) { return "", errors.New(n + " not found") }}).
		Probe(context.Background(), config.Harness{ConfigDir: ownerConfigDir(t), Models: []string{"claude-sonnet-5-5"}})
	if err == nil || !strings.Contains(err.Error(), "bwrap and socat") || len(cmds.args) != 0 {
		t.Errorf("Probe = %v after %q", err, cmds.args)
	}
}

package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The adapter's files in the workspace: a scratch directory of the run's HOME (claude's own
// state stays in the dedicated CLAUDE_CONFIG_DIR, where the login is).
const (
	scratchDir = ".factory-claude"
	// settingsFile is --settings: plugins, effort, the PreToolUse hook.
	settingsFile = "settings.json"
	// mcpFile is --mcp-config: the Effector.
	mcpFile = "mcp.json"
	// sessionFile records the run Repair continues.
	sessionFile = "factory-session.json"
	// promptFile keeps the run's prompt for a Repair that can't resume.
	promptFile = "factory-prompt.md"
)

// scratch is the run's scratch directory.
func scratch(ws harness.Workspace) string { return filepath.Join(ws.Home, scratchDir) }

// Prepare implements harness.Adapter: the OS sandbox must be supported, the dedicated config
// dir must exist, every plugin directory must have its manifest, and the scratch directory
// gets the settings file (no marketplace plugin, t's reasoning effort, the sandbox and its
// permission rules, the PreToolUse hook) and the MCP config (the
// Effector, with the run's variables passed by name).
func (a *Adapter) Prepare(_ context.Context, ws harness.Workspace, t v1alpha1.Task) error {
	if err := a.sandboxSupport(); err != nil {
		return err // never a run without the OS sandbox
	}
	if err := checkConfigDir(a.o.Config.ConfigDir); err != nil {
		return err
	}
	if ws.Home == "" || ws.Bin == "" {
		return errors.New("claude-code: the workspace has no scratch HOME or bin/")
	}
	for _, s := range ws.SkillList {
		if s.Plugin == "" {
			return fmt.Errorf("claude-code: skill %s isn't in a plugin", s.Name)
		}
	}
	for _, d := range pluginDirs(ws) {
		if _, err := os.Stat(filepath.Join(d, ".claude-plugin", "plugin.json")); err != nil {
			return fmt.Errorf("claude-code: plugin %s: %w", filepath.Base(d), err)
		}
	}
	dir := scratch(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("claude-code: %w", err)
	}
	effort := ""
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		effort = h.Resolved.ReasoningEffort
	}
	settings, err := renderSettings(ws, effort, clirun.LookupEnv(a.o.Environ(), "HOME"), a.o.Config.ConfigDir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, settingsFile), settings, 0o600); err != nil {
		return fmt.Errorf("claude-code: %w", err)
	}
	mcp, err := renderMCP(ws)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, mcpFile), mcp, 0o600); err != nil {
		return fmt.Errorf("claude-code: %w", err)
	}
	return nil
}

// pluginDirs are the task's plugin directories, skills/<plugin>, each once, in SkillList's
// order: claude takes the pipeline's plugin layout as it is.
func pluginDirs(ws harness.Workspace) []string {
	var out []string
	for _, s := range ws.SkillList {
		d := filepath.Join(ws.Skills, s.Plugin)
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// writableRoots are where the PreToolUse hook lets the file tools write: out/ and tmp/, and
// the checkouts the harness may write.
func writableRoots(ws harness.Workspace) []string {
	var roots []string
	add := func(d string) {
		if d != "" && !slices.Contains(roots, d) {
			roots = append(roots, d)
		}
	}
	add(ws.Out)
	add(ws.Tmp)
	if ws.RepoWritable {
		add(ws.Repo)
	}
	for _, m := range ws.Mounts {
		if m.Writable {
			add(m.Dir)
		}
	}
	return roots
}

// hookCommand is the PreToolUse hook: the workspace's `factory` CLI, quoted for the shell
// claude runs it with.
func hookCommand(ws harness.Workspace) string {
	parts := []string{gitcred.ShellQuote(clirun.FactoryCLI(ws)), HookOp, hookEventArg}
	for _, r := range writableRoots(ws) {
		parts = append(parts, "--write", gitcred.ShellQuote(r))
	}
	return strings.Join(parts, " ")
}

// renderSettings is the --settings file: no marketplace plugin, the effort, the OS sandbox
// and its permission rules (ownerHome is the worker's HOME, whose credentials stay
// unreadable), and the PreToolUse hook on the write tools and Bash.
func renderSettings(ws harness.Workspace, effort, ownerHome, configDir string) ([]byte, error) {
	sandbox, permissions := sandboxSettings(ws, ownerHome, configDir)
	s := map[string]any{
		keyEnabledPlugins: map[string]any{},
		keySandbox:        sandbox,
		keyPermissions:    permissions,
		keyHooks: map[string]any{
			hookPreToolUse: []any{map[string]any{
				keyMatcher: hookMatcher,
				keyHooks: []any{map[string]any{
					keyHookType: hookTypeCommand, keyHookCommand: hookCommand(ws), keyHookTimeout: hookTimeout,
				}},
			}},
		},
	}
	if effort != "" {
		s[keyEffort] = nativeEffort(effort)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("claude-code: settings: %w", err)
	}
	return append(b, '\n'), nil
}

// renderMCP is the --mcp-config file: the Effector's server, with every variable of the run's
// environment passed by name (${NAME}); their values, tokens among them, never reach the
// file.
func renderMCP(ws harness.Workspace) ([]byte, error) {
	env := map[string]string{}
	for _, n := range clirun.EnvNames(ws.Env) {
		env[n] = mcpEnvRefStart + n + mcpEnvRefEnd
	}
	m := map[string]any{keyMCPServers: map[string]any{
		clirun.EffectorServer: map[string]any{
			keyMCPType: mcpTypeStdio, keyMCPCommand: clirun.FactoryCLI(ws), keyMCPArgs: clirun.EffectorArgs(), keyMCPEnv: env,
		},
	}}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("claude-code: MCP config: %w", err)
	}
	return append(b, '\n'), nil
}

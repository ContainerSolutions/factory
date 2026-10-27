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
// state is in the scratch CLAUDE_CONFIG_DIR, configDir).
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
	// configDir is CLAUDE_CONFIG_DIR: a scratch directory of the run's HOME, since a VM
	// workspace has no login of the owner's.
	configDir = ".claude"
)

// scratch is the run's scratch directory.
func scratch(ws harness.Workspace) string { return filepath.Join(ws.Home, scratchDir) }

// configPath is the run's CLAUDE_CONFIG_DIR.
func configPath(ws harness.Workspace) string { return filepath.Join(ws.Home, configDir) }

// readOnly reports whether the run may not write its primary checkout (there is none, or it
// is read-only): it then runs without the write tools.
func readOnly(ws harness.Workspace) bool { return ws.Repo == "" || !ws.RepoWritable }

// Prepare implements harness.Adapter: every plugin directory must have its manifest, the
// scratch CLAUDE_CONFIG_DIR is made, and the scratch directory gets the settings file (no
// marketplace plugin, t's reasoning effort, the PreToolUse hook; no sandbox block and no
// permission rules: the VM is the boundary) and the MCP config (the Effector, with the run's
// variables passed by name).
func (a *Adapter) Prepare(_ context.Context, ws harness.Workspace, t v1alpha1.Task) error {
	if err := a.checkVM(); err != nil {
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
	if err := os.MkdirAll(configPath(ws), 0o700); err != nil {
		return fmt.Errorf("claude-code: %w", err)
	}
	effort := ""
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		effort = h.Resolved.ReasoningEffort
	}
	settings, err := renderSettings(ws, effort)
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

// writableRoots are where the PreToolUse hook lets the file tools write in a read-only run
// (vmWritableRoots): out/ and tmp/, and the checkouts the harness may write.
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

// vmWritableRoots are where the PreToolUse hook lets the file tools write: anywhere (/) when
// the run may write its checkout, since the VM is the boundary; else writableRoots (the write
// tools are disallowed then anyway).
func vmWritableRoots(ws harness.Workspace) []string {
	if readOnly(ws) {
		return writableRoots(ws)
	}
	return []string{string(filepath.Separator)}
}

// hookCommand is the PreToolUse hook: the workspace's `factory` CLI, quoted for the shell
// claude runs it with, letting the file tools write under roots.
func hookCommand(ws harness.Workspace, roots []string) string {
	parts := []string{gitcred.ShellQuote(clirun.FactoryCLI(ws)), HookOp, hookEventArg}
	for _, r := range roots {
		parts = append(parts, "--write", gitcred.ShellQuote(r))
	}
	return strings.Join(parts, " ")
}

// renderSettings is the --settings file: no marketplace plugin, the effort and the
// PreToolUse hook, which keeps its git push and gh write rules (pushes go
// through `factory push`, other GitHub writes through the Effector) and lets the file tools
// write vmWritableRoots. No sandbox block and no permission rules: the VM is the boundary.
func renderSettings(ws harness.Workspace, effort string) ([]byte, error) {
	return marshalSettings(baseSettings(effort, hookCommand(ws, vmWritableRoots(ws))))
}

// baseSettings are the keys of the settings file: no marketplace plugin, the effort when
// there is one, and the PreToolUse hook (hook) on the write tools and Bash.
func baseSettings(effort, hook string) map[string]any {
	s := map[string]any{
		keyEnabledPlugins: map[string]any{},
		keyHooks: map[string]any{
			hookPreToolUse: []any{map[string]any{
				keyMatcher: hookMatcher,
				keyHooks: []any{map[string]any{
					keyHookType: hookTypeCommand, keyHookCommand: hook, keyHookTimeout: hookTimeout,
				}},
			}},
		},
	}
	if effort != "" {
		s[keyEffort] = nativeEffort(effort)
	}
	return s
}

func marshalSettings(s map[string]any) ([]byte, error) {
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

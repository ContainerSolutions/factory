package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// The adapter's own files, in the workspace's scratch HOME (not opencode's config dir).
const (
	ownDir           = ".factory-opencode"
	instructionsFile = "instructions.md"
)

// Prepare implements harness.Adapter: the task runs on the model server's key (selfHosted), on
// its resolved model or the config's default, with the served model's limits set. It writes
// instructions.md (the headless preamble, the Result schema's path and, for a read-only
// checkout, that it may not be changed) in <home>/.factory-opencode/, and, under the scratch
// XDG_CONFIG_HOME's opencode/, renderConfig's opencode.json (new, 0600, never over an
// existing file or link) and skills/, the task's skills flattened (clirun.FlattenSkills).
func (a *Adapter) Prepare(_ context.Context, ws harness.Workspace, t v1alpha1.Task) error {
	var res v1alpha1.HarnessResolved
	if h := t.Spec.Harness; h != nil && h.Resolved != nil {
		res = *h.Resolved
	}
	if res.Auth != "" && res.Auth != AuthSelfHosted {
		return fmt.Errorf("opencode: unknown auth %q: opencode runs on the model server's key (%s)", res.Auth, AuthSelfHosted)
	}
	in := configInput{model: res.Model, contextWindow: a.o.ContextWindow, maxOutput: a.o.MaxOutput}
	if in.model == "" {
		in.model = a.o.Config.Default
	}
	if in.model == "" {
		return errors.New("opencode: the task names no model and the opencode config has no default (there is none for a self-hosted server)")
	}
	if in.contextWindow <= 0 || in.maxOutput <= 0 {
		return errors.New("opencode: the served model's context window and maximum output aren't set (the run bundle has no model)")
	}
	if ws.Home == "" {
		return errors.New("opencode: the workspace has no scratch HOME")
	}
	if sendReasoningEffort && res.ReasoningEffort != "" {
		in.effort = nativeEffort(res.ReasoningEffort)
	}
	if setProviderProxy {
		in.proxy = lookupEnv(ws.Env, envHTTPProxy)
	}
	own, cfg := filepath.Join(ws.Home, ownDir), filepath.Join(xdgConfigHome(ws), configDir)
	for _, d := range []string{own, cfg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("opencode: %w", err)
		}
	}
	in.instructions = filepath.Join(own, instructionsFile)
	if err := clirun.WriteNew(in.instructions, []byte(instructions(ws))); err != nil {
		return fmt.Errorf("opencode: the instructions: %w", err)
	}
	b, err := renderConfig(ws, in)
	if err != nil {
		return err
	}
	if err := clirun.WriteNew(filepath.Join(cfg, configFile), b); err != nil {
		return fmt.Errorf("opencode: the config: %w", err)
	}
	if err := clirun.FlattenSkills(ws, filepath.Join(cfg, skillsDir)); err != nil {
		return fmt.Errorf("opencode skills: %w", err)
	}
	return nil
}

// configInput is what opencode.json depends on besides the workspace.
type configInput struct {
	model, effort, proxy, instructions string
	contextWindow, maxOutput           int
}

// renderConfig is opencode.json: the one provider, factory, at the gateway's model endpoint
// with the placeholder key (and in.proxy when set); in.model with its limits and in.effort
// when set, and nothing else enabled; no update check and no sharing; the instructions file;
// the Effector's MCP server, which inherits the run's environment, so no variable is named or
// written; and permissions. No value from the run's environment is in it.
func renderConfig(ws harness.Workspace, in configInput) ([]byte, error) {
	model := modelDoc{ToolCall: true, Limit: modelLimit{Context: in.contextWindow, Output: in.maxOutput}}
	if in.effort != "" {
		model.Options = &modelOptions{ReasoningEffort: in.effort}
	}
	doc := configDoc{
		Schema:           configSchema,
		Model:            providerID + "/" + in.model,
		EnabledProviders: []string{providerID},
		Share:            shareDisabled,
		Instructions:     []string{in.instructions},
		Provider: map[string]providerDoc{providerID: {
			NPM:     providerNPM,
			Options: providerOptions{BaseURL: providerBaseURL, APIKey: harness.Placeholder, Proxy: in.proxy},
			Models:  map[string]modelDoc{in.model: model},
		}},
		MCP: map[string]mcpDoc{clirun.EffectorServer: {
			Type: mcpLocal, Command: append([]string{clirun.FactoryCLI(ws)}, clirun.EffectorArgs()...), Enabled: true,
		}},
		Permission: permissions(ws),
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("opencode: the config: %w", err)
	}
	return append(b, '\n'), nil
}

// permissions allow everything but a push and, for a read-only primary checkout, an edit
// outside out/ (the Result's directory, as the path relative to the checkout opencode
// matches). Nothing in the VM stops a shell write: the instructions say not to, and the
// gateway refuses the push.
func permissions(ws harness.Workspace) permissionDoc {
	edit := rules{{permAll, permAllow}}
	if ws.Repo != "" && !ws.RepoWritable {
		edit = rules{{permAll, permDeny}}
		if rel, err := filepath.Rel(ws.Repo, ws.Out); ws.Out != "" && err == nil {
			edit = append(edit, [2]string{filepath.ToSlash(rel) + "/*", permAllow})
		}
	}
	return permissionDoc{All: permAllow, Edit: edit, Bash: rules{{permAll, permAllow}, {gitPushPattern, permDeny}}}
}

// instructions is the file opencode.json's instructions names: the headless preamble, the
// Result schema's path and, for a read-only checkout, that the task may not change it.
func instructions(ws harness.Workspace) string {
	s := "<!-- Generated by factory-agent for this run. -->\n\n" + strings.TrimSpace(ws.Preamble) + "\n"
	if ws.Schema != "" {
		s += "\nThe Result's JSON Schema is " + ws.Schema + ".\n"
	}
	if ws.Repo != "" && !ws.RepoWritable {
		s += "\nThe primary checkout, " + ws.Repo + ", is read-only to you: don't change it (a push to it is refused).\n"
	}
	return s
}

// runEnv is opencode's environment for a run: ws.Env with OPENCODE_CONFIG naming the
// generated config and runSwitches set. XDG_DATA_HOME stays the system's, so opencode's
// sessions and logs go to the run user's ~/.local/share/opencode in the VM.
func runEnv(ws harness.Workspace) []string {
	env := clirun.WithEnv(ws.Env, envConfig, filepath.Join(xdgConfigHome(ws), configDir, configFile))
	for _, k := range runSwitches {
		env = clirun.WithEnv(env, k, "1")
	}
	return env
}

// xdgConfigHome is the run's XDG_CONFIG_HOME, where opencode looks: ws.Env's (the pipeline's
// scratch <home>/.config), else <home>/.config as without it.
func xdgConfigHome(ws harness.Workspace) string {
	if v := lookupEnv(ws.Env, envXDGConfigHome); filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(ws.Home, ".config")
}

// lookupEnv is key's last value in env, or "".
func lookupEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

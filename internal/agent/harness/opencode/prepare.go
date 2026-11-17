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

// Prepare implements harness.Adapter: on the model server's key (selfHosted), the resolved
// model or the config's default, and the served model's limits, it writes instructions.md in
// <home>/.factory-opencode/ and, in the scratch XDG_CONFIG_HOME's opencode/, opencode.json
// (new, 0600, never over an existing file or link) and the flattened skills/.
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

// renderConfig is opencode.json: the one provider at the gateway with the placeholder key,
// in.model with its limits (and in.effort, in.proxy when set), no update check or sharing,
// the instructions, the Effector's MCP server and permissions. No variable's value is in it.
func renderConfig(ws harness.Workspace, in configInput) ([]byte, error) {
	model := modelDoc{ToolCall: true}
	model.Limit.Context, model.Limit.Output = in.contextWindow, in.maxOutput
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
// outside out/, the Result's directory (a shell write isn't stopped: the gateway refuses the
// push).
func permissions(ws harness.Workspace) permissionDoc {
	edit := map[string]string{permAll: permAllow}
	if ws.Repo != "" && !ws.RepoWritable {
		edit[permAll] = permDeny
		if rel, err := filepath.Rel(ws.Repo, ws.Out); ws.Out != "" && err == nil {
			edit[filepath.ToSlash(rel)+"/*"] = permAllow
		}
	}
	return permissionDoc{All: permAllow, Edit: edit, Bash: map[string]string{permAll: permAllow, gitPushPattern: permDeny}}
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

// runEnv is OC-5's run environment: ws.Env with OPENCODE_CONFIG and runSwitches set.
// XDG_DATA_HOME stays the system's: opencode's sessions go to the VM's ~/.local/share.
func runEnv(ws harness.Workspace) []string {
	env := clirun.WithEnv(ws.Env, envConfig, filepath.Join(xdgConfigHome(ws), configDir, configFile))
	for _, k := range runSwitches {
		env = clirun.WithEnv(env, k, "1")
	}
	return env
}

// xdgConfigHome is ws.Env's XDG_CONFIG_HOME (the pipeline's <home>/.config), else the default.
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

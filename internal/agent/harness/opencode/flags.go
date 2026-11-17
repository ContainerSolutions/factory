package opencode

import (
	"bytes"
	"encoding/json"

	"github.com/ContainerSolutions/factory/internal/agent/gateway/policy"
)

// This file is every part of opencode's interface the adapter relies on: environment
// variables, config keys, file names and (with OC-5) the run flags and the JSON events. As
// for codex and claude, opencode isn't run while the adapter is written. Each name was read
// instead from the source of the release the image installs, and is marked with the file it
// was read from; what the source can't settle (how a served model or a compiled Bun binary
// behaves) is marked UNVERIFIED (owner to verify on a VM workspace run), as is the
// harness-adapters.md §OpenCode list.
//
// The pinned release: anomalyco/opencode v1.19.6 (sst/opencode redirects there), published
// 2026-11-17, the latest stable release with Linux binaries when the adapter was written. The
// version base.sh pins (OC-6) is this one, from these assets and their sha256 digests (the
// releases API's):
//
//	opencode-linux-arm64.tar.gz  5b68c1f4c5e75b4134e664cc8f36d634d64208d4fcfe5bcf3b48bab699c2dbac
//	opencode-linux-x64.tar.gz    47734e08968b30cc22a0bffb6d9feeed76ad4aa743213a12122040392d09a057
//
// Source paths below are relative to github.com/anomalyco/opencode at the tag v1.19.6.

// ---- The environment ----------------------------------------------------------------------

// envConfig names a config file opencode loads after its global one.
// read from packages/core/src/flag/flag.ts and packages/opencode/src/config/config.ts
const envConfig = "OPENCODE_CONFIG"

// envXDGConfigHome is where opencode's global config dir is, as configDir/ under it (or under
// ~/.config without it): opencode.json is also loaded from there, and skills are found in its
// skills/.
// read from packages/core/src/global.ts (xdg-basedir)
const envXDGConfigHome = "XDG_CONFIG_HOME"

// envHTTPProxy is the run's proxy, the gateway, which opencode's fetch takes from the
// environment for the model endpoint's http:// URL (setProviderProxy).
// read from packages/web/src/content/docs/network.mdx; that the compiled binary's fetch
// honours it is UNVERIFIED (owner to verify on a VM workspace run)
const envHTTPProxy = "HTTP_PROXY"

// runSwitches are the variables runEnv adds to the run's environment, each "1" (opencode
// takes "1" or "true"): no update check, no models.dev fetch (the provider's models are in
// the config), no LSP server downloads, and none of Claude Code's files (a repo's .claude/
// skills, CLAUDE.md, ~/.claude/CLAUDE.md).
// read from packages/core/src/flag/flag.ts (the first two) and
// packages/opencode/src/effect/runtime-flags.ts (the last two)
var runSwitches = []string{
	"OPENCODE_DISABLE_AUTOUPDATE",
	"OPENCODE_DISABLE_MODELS_FETCH",
	"OPENCODE_DISABLE_LSP_DOWNLOAD",
	"OPENCODE_DISABLE_CLAUDE_CODE",
}

// ---- Files --------------------------------------------------------------------------------

// configDir under XDG_CONFIG_HOME is opencode's global config dir; configFile in it is loaded
// as the global config (and, named by OPENCODE_CONFIG, once more: the same values).
// read from packages/core/src/global.ts and packages/opencode/src/config/config.ts
const (
	configDir  = "opencode"
	configFile = "opencode.json"
)

// skillsDir in configDir is where opencode finds skills: it scans every config dir for
// {skill,skills}/**/SKILL.md, and a skill's name is its SKILL.md's front matter name.
// read from packages/opencode/src/skill/index.ts
const skillsDir = "skills"

// ---- opencode.json ------------------------------------------------------------------------

// configSchema is opencode.json's $schema. opencode writes it into a config file that lacks
// it, which would rewrite the generated file in the run.
// read from packages/opencode/src/config/config.ts
const configSchema = "https://opencode.ai/config.json"

// providerID is the one provider in the config, whose models a run picks as factory/<model>.
const providerID = "factory"

// providerNPM is the provider's AI SDK package, the OpenAI chat-completions one vLLM serves.
// It is bundled in the binary, so it is never fetched from npm.
// read from packages/opencode/src/provider/provider.ts (BUNDLED_PROVIDERS)
const providerNPM = "@ai-sdk/openai-compatible"

// providerBaseURL is the gateway's model endpoint as the VM sees it (gateway.md §The model
// endpoint): plain HTTP through the proxy, which forwards /v1/chat/completions and /v1/models.
const providerBaseURL = "http://" + policy.ModelEndpointHost + "/v1"

// shareDisabled turns off session sharing.
// read from packages/core/src/v1/config/config.ts
const shareDisabled = "disabled"

// mcpLocal is a local (stdio) MCP server's type. Its process inherits opencode's whole
// environment, so the config passes no environment map: the Effector gets the run's
// variables without naming them (opencode's {env:NAME} would paste each value into the
// config's text unescaped before parsing it, so a value holding a quote would break it).
// read from packages/core/src/v1/config/mcp.ts, packages/opencode/src/mcp/index.ts and
// packages/opencode/src/config/variable.ts
const mcpLocal = "local"

// Permission actions. Every rule the config writes is one of them, never "ask": `opencode
// run` rejects whatever would ask, and a rejection ends the session's loop (a deny only fails
// the tool call).
// read from packages/core/src/v1/config/permission.ts, packages/opencode/src/cli/cmd/run.ts
// and packages/opencode/src/session/processor.ts
const (
	permAllow = "allow"
	permDeny  = "deny"
)

// permAll is the pattern and permission name every request matches. The rules of a
// permission are evaluated in the config's order and the last match wins, so the config's
// permission starts with "*": allow, which overrides every default "ask" (doom_loop,
// external_directory outside the project, a read of *.env).
// read from packages/opencode/src/permission/index.ts, packages/opencode/src/agent/agent.ts
// and packages/core/src/util/wildcard.ts
const permAll = "*"

// gitPushPattern is the bash rule's pattern a push matches: bash's patterns are each command's
// text, and a trailing " *" also matches the command without arguments.
// read from packages/opencode/src/tool/shell.ts and packages/core/src/util/wildcard.ts
const gitPushPattern = "git push *"

// configDoc is opencode.json: v1.19.6's config schema (still the v1 one, with provider.<id>.npm
// and options.baseURL), the keys the adapter sets and nothing else.
// read from packages/core/src/v1/config/{config,provider,mcp,permission}.ts
type configDoc struct {
	Schema           string                 `json:"$schema"`
	Model            string                 `json:"model"`
	EnabledProviders []string               `json:"enabled_providers"`
	Autoupdate       bool                   `json:"autoupdate"`
	Share            string                 `json:"share"`
	Instructions     []string               `json:"instructions"`
	Provider         map[string]providerDoc `json:"provider"`
	MCP              map[string]mcpDoc      `json:"mcp"`
	Permission       permissionDoc          `json:"permission"`
}

type providerDoc struct {
	NPM     string              `json:"npm"`
	Options providerOptions     `json:"options"`
	Models  map[string]modelDoc `json:"models"`
}

// providerOptions are the provider's options. Proxy is written only when setProviderProxy.
type providerOptions struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
	Proxy   string `json:"proxy,omitzero"`
}

type modelDoc struct {
	ToolCall bool          `json:"tool_call"`
	Limit    modelLimit    `json:"limit"`
	Options  *modelOptions `json:"options,omitzero"`
}

type modelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// modelOptions are the model's request options: reasoningEffort is the name opencode's own
// effort variants use for an openai-compatible model (sent as reasoning_effort).
// read from packages/opencode/src/provider/transform.ts and
// packages/core/src/v1/config/provider-options.ts
type modelOptions struct {
	ReasoningEffort string `json:"reasoningEffort"`
}

type mcpDoc struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

// permissionDoc is the config's permission, in its order: everything allowed, then edit and
// bash. A tool's write (write, edit, patch) asks for edit with the file's path relative to the
// project's worktree.
// read from packages/opencode/src/tool/write.ts and packages/opencode/src/config/config.ts
type permissionDoc struct {
	All  string `json:"*"`
	Edit rules  `json:"edit"`
	Bash rules  `json:"bash"`
}

// rules is one permission's pattern → action map, written in order (the last match wins).
type rules [][2]string

func (r rules) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range r {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(kv[0])
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(kv[1])
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ---- run ----------------------------------------------------------------------------------

// The run subcommand and the flags OC-5's Run and Repair use. `opencode run` sends one prompt
// to a new session (or the --session one), streams the session's events on stdout as JSON
// lines with --format json, and exits when the session is idle. --file attaches a file to the
// message, the fallback should the prompt not come through stdin (promptOnStdin).
// read from packages/opencode/src/cli/cmd/run.ts
const (
	cmdRun      = "run"
	flagFormat  = "--format"
	formatJSON  = "json"
	flagModel   = "--model" // <provider>/<model>: the provider's id, then the rest
	flagSession = "--session"
	flagFile    = "--file"
)

// promptOnStdin sends the prompt on stdin: run reads stdin whenever it isn't a terminal and
// appends it to the message arguments (none here).
// read from packages/opencode/src/cli/cmd/run.ts
const promptOnStdin = true

// ---- Switches -----------------------------------------------------------------------------

// sendReasoningEffort writes the task's effort as the model's reasoningEffort. A model
// without reasoning (Qwen3-Coder) may refuse the field: turn it off then.
// UNVERIFIED (owner to verify on a VM workspace run)
const sendReasoningEffort = true

// setProviderProxy writes the run's HTTP_PROXY as the provider's proxy option, against a
// compiled Bun binary whose fetch ignores the environment. It is off: v1.19.6 has no such
// option (the provider's options go to the AI SDK's factory, which ignores proxy, and its
// fetch is the global one), so the key would do nothing. The proxy is the environment's.
// read from packages/opencode/src/provider/provider.ts
const setProviderProxy = false

// ---- Efforts ------------------------------------------------------------------------------

// effortMapping is the effort mapping the adapter declares (harness-adapters.md: opencode's
// reasoningEffort takes low, medium and high). nativeEffort maps minimal and max onto it.
// UNVERIFIED (owner to verify on a VM workspace run)
var effortMapping = map[string]string{
	"low":    "low",
	"medium": "medium",
	"high":   "high",
}

// nativeEffort is the reasoningEffort for a normalised effort: minimal → low, max → high,
// else effortMapping's. An unknown effort passes through unchanged.
func nativeEffort(effort string) string {
	switch effort {
	case "minimal":
		return effortMapping["low"]
	case "max":
		return effortMapping["high"]
	}
	if v, ok := effortMapping[effort]; ok {
		return v
	}
	return effort
}

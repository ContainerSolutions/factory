package opencode

import "github.com/ContainerSolutions/factory/internal/agent/gateway/policy"

// This file is every part of opencode's interface the adapter relies on. As for codex and
// claude, opencode isn't run while the adapter is written: each name was read from the source
// of the release the image installs, marked "read from <file>" (paths in
// github.com/anomalyco/opencode at v1.19.6), or is UNVERIFIED (owner to verify on a VM
// workspace run) where the source can't settle it (harness-adapters.md §OpenCode's list).
//
// The pinned release, which base.sh installs (OC-6): anomalyco/opencode v1.19.6 (sst/opencode
// redirects there), published 2026-11-17, the latest stable one with Linux binaries. Its
// assets and their sha256 digests (the releases API's):
//
//	opencode-linux-arm64.tar.gz  5b68c1f4c5e75b4134e664cc8f36d634d64208d4fcfe5bcf3b48bab699c2dbac
//	opencode-linux-x64.tar.gz    47734e08968b30cc22a0bffb6d9feeed76ad4aa743213a12122040392d09a057

// ---- The environment ----------------------------------------------------------------------

// envConfig names a config file opencode loads after its global one.
// read from packages/core/src/flag/flag.ts and packages/opencode/src/config/config.ts
const envConfig = "OPENCODE_CONFIG"

// envXDGConfigHome holds opencode's global config dir, configDir (else ~/.config does).
// read from packages/core/src/global.ts
const envXDGConfigHome = "XDG_CONFIG_HOME"

// envHTTPProxy is the run's proxy (the gateway), which opencode's fetch takes from the
// environment for the endpoint's http:// URL (see setProviderProxy).
// read from packages/web/src/content/docs/network.mdx; that the compiled binary honours it is
// UNVERIFIED (owner to verify on a VM workspace run)
const envHTTPProxy = "HTTP_PROXY"

// runSwitches are set to "1" for a run: no update check, no models.dev fetch, no LSP server
// downloads, and none of Claude Code's files (a repo's .claude/ skills, CLAUDE.md).
// read from packages/core/src/flag/flag.ts (the first two) and
// packages/opencode/src/effect/runtime-flags.ts (the last two)
var runSwitches = []string{
	"OPENCODE_DISABLE_AUTOUPDATE",
	"OPENCODE_DISABLE_MODELS_FETCH",
	"OPENCODE_DISABLE_LSP_DOWNLOAD",
	"OPENCODE_DISABLE_CLAUDE_CODE",
}

// ---- Files --------------------------------------------------------------------------------

// configDir under XDG_CONFIG_HOME is opencode's global config dir, whose configFile it loads
// (and, named by OPENCODE_CONFIG, loads again: the same values). It finds skills in every
// config dir's {skill,skills}/**/SKILL.md, named by their front matter: skillsDir.
// read from packages/core/src/global.ts, packages/opencode/src/config/config.ts and
// packages/opencode/src/skill/index.ts
const (
	configDir  = "opencode"
	configFile = "opencode.json"
	skillsDir  = "skills"
)

// ---- opencode.json ------------------------------------------------------------------------

const (
	// configSchema is $schema: opencode writes it into a config file without one, which would
	// rewrite the generated file in the run (config/config.ts).
	configSchema = "https://opencode.ai/config.json"
	// providerID is the one provider: a run's model is factory/<model>.
	providerID = "factory"
	// providerNPM, the OpenAI chat-completions AI SDK package, is bundled in the binary, so it
	// is never fetched from npm (provider/provider.ts, BUNDLED_PROVIDERS).
	providerNPM = "@ai-sdk/openai-compatible"
	// providerBaseURL is the gateway's model endpoint as the VM sees it (gateway.md).
	providerBaseURL = "http://" + policy.ModelEndpointHost + "/v1"
	shareDisabled   = "disabled"
	// mcpLocal is a stdio MCP server, which inherits opencode's whole environment: the config
	// names no variable. opencode's {env:NAME} would paste each value into the config's text
	// unescaped before parsing it (config/variable.ts), so a quote in one would break it.
	// read from packages/opencode/src/mcp/index.ts
	mcpLocal = "local"
)

// Permission rules are allow or deny, never "ask": run rejects an ask, which ends the loop (a
// deny only fails the tool call). The last matching rule wins, so permAll: allow comes first,
// over opencode's default asks; encoding/json sorts a map's keys and "*" sorts first. Bash's
// patterns are each command's text ("git push *" also matches a bare push), edit's the path
// relative to the worktree.
// read from packages/opencode/src/{cli/cmd/run,session/processor,permission/index,agent/agent,
// tool/shell,tool/write}.ts and packages/core/src/util/wildcard.ts
const (
	permAllow      = "allow"
	permDeny       = "deny"
	permAll        = "*"
	gitPushPattern = "git push *"
)

// configDoc is opencode.json, in v1.19.6's schema (still v1: provider.<id>.npm and
// options.baseURL): the keys the adapter sets and nothing else.
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

type providerOptions struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
	Proxy   string `json:"proxy,omitzero"` // only with setProviderProxy
}

type modelDoc struct {
	ToolCall bool `json:"tool_call"`
	Limit    struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Options *modelOptions `json:"options,omitzero"`
}

// modelOptions: reasoningEffort is what opencode's own effort variants set for an
// openai-compatible model (sent as reasoning_effort).
// read from packages/opencode/src/provider/transform.ts
type modelOptions struct {
	ReasoningEffort string `json:"reasoningEffort"`
}

type mcpDoc struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

// permissionDoc is the config's permission, in its order: everything, then edit and bash.
type permissionDoc struct {
	All  string            `json:"*"`
	Edit map[string]string `json:"edit"`
	Bash map[string]string `json:"bash"`
}

// ---- run ----------------------------------------------------------------------------------

// The run flags OC-5 uses: --format json streams the session's events as JSON lines,
// --model is <provider>/<model> (split at the first "/"), --file the stdin fallback.
// read from packages/opencode/src/cli/cmd/run.ts
const (
	cmdRun      = "run"
	flagFormat  = "--format"
	formatJSON  = "json"
	flagModel   = "--model"
	flagSession = "--session"
	flagFile    = "--file"
)

// promptOnStdin sends the prompt on stdin, which run reads whenever it isn't a terminal.
// read from packages/opencode/src/cli/cmd/run.ts
const promptOnStdin = true

// ---- Switches -----------------------------------------------------------------------------

// sendReasoningEffort writes the task's effort as the model's reasoningEffort. A served model
// without reasoning (Qwen3-Coder) may refuse it: turn it off then.
// UNVERIFIED (owner to verify on a VM workspace run)
const sendReasoningEffort = true

// setProviderProxy writes the run's HTTP_PROXY as the provider's proxy option. Off: v1.19.6
// has no such option (the provider's options go to the AI SDK's factory, which ignores it,
// and its fetch is the global one), so the proxy is the environment's.
// read from packages/opencode/src/provider/provider.ts
const setProviderProxy = false

// ---- Efforts ------------------------------------------------------------------------------

// effortMapping is the declared mapping: reasoningEffort takes low, medium and high.
// UNVERIFIED (owner to verify on a VM workspace run)
var effortMapping = map[string]string{"low": "low", "medium": "medium", "high": "high"}

// nativeEffort is the reasoningEffort for a normalised effort: minimal → low, max → high,
// else effortMapping's; an unknown effort passes through.
func nativeEffort(effort string) string {
	switch effort {
	case "minimal":
		effort = "low"
	case "max":
		effort = "high"
	}
	if v, ok := effortMapping[effort]; ok {
		return v
	}
	return effort
}

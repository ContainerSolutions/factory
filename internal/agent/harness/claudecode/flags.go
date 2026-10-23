package claudecode

import (
	"strconv"
	"strings"
)

// This file is every part of the Claude Code CLI's interface the adapter relies on: the
// minimum version, the environment variables and files of its login, its print-mode flags,
// the settings and MCP config keys, the hook protocol, tool names and the stream-json event
// names. The LLD (harness-adapters.md §Claude Code) marks most of them *verify*. The owner
// decided (2026-10-19) that claude isn't run while the adapter is written, so they were
// written unchecked and marked UNVERIFIED, to be fixed here, in one place, after
// `make smoke-claude`. The owner's smoke run with claude 2.1.304 (2026-10-20) accepted the
// print-mode flags it passes; those are no longer marked. The rest still are.

// ---- Version and login ---------------------------------------------------------------------

// MinVersion is the oldest claude the adapter accepts: one with --json-schema (and the
// structured_output result field), --plugin-dir, --setting-sources and stream-json's
// system/init listing plugins.
// UNVERIFIED (owner to verify with make smoke-claude)
const MinVersion = "2.1.0"

// versionArgs print the version, as "2.1.3 (Claude Code)".
// UNVERIFIED (owner to verify with make smoke-claude)
var versionArgs = []string{"--version"}

// envConfigDir is the directory claude keeps its config, sessions and (outside macOS) its
// login in; the macOS Keychain entry of the login is keyed by it too.
// UNVERIFIED (owner to verify with make smoke-claude)
const envConfigDir = "CLAUDE_CONFIG_DIR"

// envAPIKey set in the worker's environment makes the agent run claude on that API key
// (beyond the PoC, 10-poc.md); claude prefers it over a subscription login.
// UNVERIFIED (owner to verify with make smoke-claude)
const envAPIKey = "ANTHROPIC_API_KEY"

// envOAuthToken is a subscription's long-lived token (`claude setup-token`), an alternative
// to the config dir's login for a headless machine.
// UNVERIFIED (owner to verify with make smoke-claude)
const envOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"

// envIsSandbox=1 tells claude it runs in a sandbox it may trust: it refuses
// bypassPermissions as root without it. Only a root run under IsolationVM sets it (a plain
// container; the VM runs as admin).
// UNVERIFIED (owner to verify with make smoke-claude)
const envIsSandbox = "IS_SANDBOX"

// credentialsFile is where claude stores a subscription login in its config dir when it has
// no keychain (Linux). Probe only stats it.
// UNVERIFIED (owner to verify with make smoke-claude)
const credentialsFile = ".credentials.json"

// stateFile is claude's state in its config dir. On macOS the login is in the Keychain, which
// the agent never reads: the state file's presence is all Probe can check there.
// UNVERIFIED (owner to verify with make smoke-claude)
const stateFile = ".claude.json"

// loginHint is how the owner signs the dedicated config dir in once (%[1]s is the dir).
// UNVERIFIED (owner to verify with make smoke-claude)
const loginHint = "run `CLAUDE_CONFIG_DIR=%[1]s claude` once and sign in with /login, or set ANTHROPIC_API_KEY in the worker's environment"

// runEnv is set for every run: no auto-update and no non-essential traffic (telemetry, error
// reports) from a headless run.
// UNVERIFIED (owner to verify with make smoke-claude)
var runEnv = [][2]string{
	{"DISABLE_AUTOUPDATER", "1"},
	{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"},
}

// ---- Print mode ----------------------------------------------------------------------------

// Flags of `claude -p`.
const (
	// flagPrint runs one prompt headless and exits; with no prompt argument it reads the
	// prompt from stdin.
	flagPrint = "--print"
	// flagOutputFormat stream-json prints one JSON event per line; print mode needs
	// flagVerbose with it.
	flagOutputFormat = "--output-format"
	outputStreamJSON = "stream-json"
	flagVerbose      = "--verbose"
	// flagModel picks the model.
	flagModel = "--model"
	// flagAppendSystemPrompt appends to claude's own system prompt (the headless preamble).
	flagAppendSystemPrompt = "--append-system-prompt"
	// flagPluginDir loads a plugin from a directory (<dir>/.claude-plugin/plugin.json and
	// <dir>/skills/), for this session only; repeated once per plugin.
	flagPluginDir = "--plugin-dir"
	// flagMCPConfig loads MCP servers from a JSON file; flagStrictMCPConfig ignores every
	// other MCP config (the target repo's .mcp.json, the user's).
	flagMCPConfig       = "--mcp-config"
	flagStrictMCPConfig = "--strict-mcp-config"
	// flagJSONSchema is the JSON Schema the final output must match, given inline (not a
	// path) and in draft-07 (see useJSONSchema); the result event then carries
	// structured_output.
	flagJSONSchema = "--json-schema"
	// flagMaxTurns stops the run after that many turns (result subtype error_max_turns).
	flagMaxTurns = "--max-turns"
	// flagMaxBudget stops the run past a USD budget (error_max_budget_usd); API keys only.
	// UNVERIFIED (owner to verify with make smoke-claude)
	flagMaxBudget = "--max-budget-usd"
	// flagPermissionMode bypassPermissions: no permission prompts (there is no one to
	// answer them); --disallowedTools and the PreToolUse hook still apply.
	flagPermissionMode = "--permission-mode"
	permissionBypass   = "bypassPermissions"
	// flagDisallowedTools removes tools, as one comma-separated list.
	// UNVERIFIED (owner to verify with make smoke-claude)
	flagDisallowedTools = "--disallowedTools"
	// flagSettings loads a settings file on top of the setting sources.
	flagSettings = "--settings"
	// flagSettingSources limits the settings files read: user only (the dedicated config
	// dir's), never the target repo's project or local settings (.claude/settings*.json),
	// which could add hooks or permissions.
	flagSettingSources = "--setting-sources"
	settingSourcesUser = "user"
	// flagAddDir gives the tools another directory besides the working directory.
	flagAddDir = "--add-dir"
	// flagResume continues a session by id (from the same working directory).
	// UNVERIFIED (owner to verify with make smoke-claude)
	flagResume = "--resume"
)

// useJSONSchema passes --json-schema (the LLD's invocation). claude 2.1.304 validates it as
// draft-07 and refused the Result schema's draft 2020-12 $schema ("--json-schema is not a
// valid JSON Schema: no schema with key or ref
// "https://json-schema.org/draft/2020-12/schema""), so compactSchema hands it the
// workspace's self-contained schema converted to draft-07 (toDraft7), with a plain object
// root (objectRoot: the Messages API's rule for the StructuredOutput tool's input_schema,
// which a TaskSpec schema's root allOf broke). Set it to false if claude refuses a Result
// schema anyway: the agent still writes $FACTORY_RESULT and the final message, which the
// pipeline validates with its repair turn.
const useJSONSchema = true

// repairMaxTurns bounds the repair run (the pipeline doesn't count its turns against
// limits.maxTurns): writing the Result and answering it takes a few.
const repairMaxTurns = 5

// ---- Tools ---------------------------------------------------------------------------------

// Tool names (stream-json tool_use names, hook tool_name, --disallowedTools).
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	toolBash             = "Bash"
	toolWrite            = "Write"
	toolEdit             = "Edit"
	toolMultiEdit        = "MultiEdit"
	toolNotebookEdit     = "NotebookEdit"
	toolRead             = "Read"
	toolGrep             = "Grep"
	toolGlob             = "Glob"
	toolWebSearch        = "WebSearch"
	toolWebFetch         = "WebFetch"
	toolTask             = "Task"
	toolSkill            = "Skill"
	toolTodoWrite        = "TodoWrite"
	toolStructuredOutput = "StructuredOutput" // --json-schema's final output, as a tool call
	mcpToolPrefix        = "mcp__"            // mcp__<server>__<tool>
)

// writeTools write files; a read-only workspace disallows them.
// UNVERIFIED (owner to verify with make smoke-claude)
var writeTools = []string{toolWrite, toolEdit, toolMultiEdit, toolNotebookEdit}

// Tool input fields the adapter reads.
// UNVERIFIED (owner to verify with make smoke-claude)
type toolInput struct {
	Command      string `json:"command"`       // Bash
	FilePath     string `json:"file_path"`     // Write, Edit, MultiEdit, Read
	NotebookPath string `json:"notebook_path"` // NotebookEdit
	Pattern      string `json:"pattern"`       // Grep, Glob
	Query        string `json:"query"`         // WebSearch
	URL          string `json:"url"`           // WebFetch
	Description  string `json:"description"`   // Task
	Skill        string `json:"skill"`         // Skill
	Todos        []struct {
		Status string `json:"status"` // pending, in_progress, completed
	} `json:"todos"` // TodoWrite
}

const todoCompleted = "completed"

// ---- settings.json, the PreToolUse hook and mcp.json ---------------------------------------

// Keys of the generated settings file (see renderSettings).
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	// keyEnabledPlugins {}: no marketplace plugin, only the --plugin-dir ones.
	keyEnabledPlugins = "enabledPlugins"
	// keyEffort is the reasoning effort (the LLD's example key).
	keyEffort = "effortLevel"
	// keyHooks.PreToolUse: [{matcher, hooks: [{type: command, command, timeout}]}].
	keyHooks        = "hooks"
	hookPreToolUse  = "PreToolUse"
	keyMatcher      = "matcher"
	keyHookType     = "type"
	hookTypeCommand = "command"
	keyHookCommand  = "command"
	keyHookTimeout  = "timeout" // seconds
)

// hookMatcher selects the tools the PreToolUse hook checks (a regular expression).
// UNVERIFIED (owner to verify with make smoke-claude)
const hookMatcher = "Write|Edit|MultiEdit|NotebookEdit|Bash"

// hookTimeout bounds one hook call, in seconds.
const hookTimeout = 30

// The PreToolUse hook protocol: claude writes {tool_name, tool_input, cwd, …} to the hook's
// stdin; exit 0 lets the call run, exit hookExitBlock blocks it and gives the hook's stderr
// to the model.
// UNVERIFIED (owner to verify with make smoke-claude)
const hookExitBlock = 2

// Keys of the generated MCP config: {"mcpServers": {<name>: {type, command, args, env}}},
// with env values ${NAME} expanded by claude from its own environment (no value on disk).
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	keyMCPServers  = "mcpServers"
	keyMCPType     = "type"
	mcpTypeStdio   = "stdio"
	keyMCPCommand  = "command"
	keyMCPArgs     = "args"
	keyMCPEnv      = "env"
	mcpEnvRefStart = "${"
	mcpEnvRefEnd   = "}"
)

// ---- OS sandbox ----------------------------------------------------------------------------

// Keys of the settings file's sandbox block: claude runs Bash commands inside the OS sandbox
// (Seatbelt on macOS, bubblewrap on Linux). Its filesystem and network limits come from the
// permission rules below.
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	keySandbox        = "sandbox"
	keySandboxEnabled = "enabled"
	// keyAllowUnsandboxed false: no command may ask to run outside the sandbox
	// (dangerouslyDisableSandbox).
	keyAllowUnsandboxed = "allowUnsandboxedCommands"
	// keyAutoAllowBash: sandboxed commands run without a permission prompt.
	keyAutoAllowBash = "autoAllowBashIfSandboxed"
	// keyExcludedCommands []: no command is exempt from the sandbox.
	keyExcludedCommands = "excludedCommands"
	// keyWeakerNested false: never the weaker sandbox meant for running inside a container.
	keyWeakerNested = "enableWeakerNestedSandbox"
	// keySandboxNetwork: allowUnixSockets (the git credential helper's socket) and
	// allowLocalBinding (off: commands can't listen).
	keySandboxNetwork    = "network"
	keyAllowUnixSockets  = "allowUnixSockets"
	keyAllowLocalBinding = "allowLocalBinding"
	// keyPermissions: allow and deny rules. The sandbox takes its writable paths from the
	// Edit allow rules, its unreadable paths from the Read deny rules and its network
	// allow-list from the WebFetch(domain:…) allow rules. Deny wins over allow.
	keyPermissions = "permissions"
	keyAllow       = "allow"
	keyDeny        = "deny"
)

// Permission rule forms; an absolute path is written with a leading "//" (a single "/" is
// relative to the settings file).
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	ruleEditFormat   = "Edit(/%s/**)"
	ruleReadFormat   = "Read(/%s)"
	ruleReadAllForm  = "Read(/%s/**)"
	ruleDomainFormat = "WebFetch(domain:%s)"
)

// harnessDomains are what the harness itself needs from inside the sandbox (its API;
// non-essential traffic is off, see runEnv).
// UNVERIFIED (owner to verify with make smoke-claude)
var harnessDomains = []string{"api.anthropic.com"}

// hostDomains are the domains that come with a git host: github.com's API and raw content.
// Another host (GHE) serves its API itself.
// UNVERIFIED (owner to verify with make smoke-claude)
var hostDomains = map[string][]string{
	"github.com": {"github.com", "api.github.com", "*.githubusercontent.com"},
}

// deniedHomePaths, under the worker's HOME, are never readable from the sandbox: other
// credentials and logins (keys, cloud, gh, docker, package registries), the agent's own
// config and join token, and both harnesses' default and dedicated config dirs. An entry
// that holds the workspace (a work root inside it) is left out.
var deniedHomePaths = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".config/gcloud", ".config/gh", ".config/factory-agent",
	".netrc", ".git-credentials", ".docker", ".kube", ".npmrc", ".pypirc",
	".claude", ".claude.json", ".codex", ".factory-agent/codex-home", ".factory-agent/claude",
	"Library/Keychains",
}

// sandboxTools are what the OS sandbox needs on each platform; elsewhere it isn't
// supported and the adapter refuses to run.
// UNVERIFIED (owner to verify with make smoke-claude)
var sandboxTools = map[string][]string{
	"darwin": {"sandbox-exec"},
	"linux":  {"bwrap", "socat"},
}

// ---- Effort --------------------------------------------------------------------------------

// effortMapping maps a normalised effort to the effortLevel setting. max is the highest
// level claude takes (the LLD: "max → highest available").
// UNVERIFIED (owner to verify with make smoke-claude)
var effortMapping = map[string]string{
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"max":     "high",
}

// nativeEffort is the effortLevel for a normalised effort ("" for none); an unknown effort
// passes through unchanged.
func nativeEffort(effort string) string {
	if v, ok := effortMapping[effort]; ok {
		return v
	}
	return effort
}

// ---- stream-json ---------------------------------------------------------------------------

// Event types and subtypes of `--output-format stream-json --verbose`.
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	evSystem    = "system" // subtype init: {session_id, model, tools, mcp_servers, plugins, plugin_errors}
	subInit     = "init"
	evAssistant = "assistant"        // {message: {id, content: [text|tool_use|thinking], usage}, parent_tool_use_id, error}
	evUser      = "user"             // {message: {content: [tool_result]}}
	evResult    = "result"           // {subtype, is_error, result, num_turns, usage, total_cost_usd, structured_output, permission_denials, errors}
	evRateLimit = "rate_limit_event" // {rate_limit_info: {status, resetsAt, rateLimitType}}

	resultSuccess          = "success"
	resultMaxTurns         = "error_max_turns"
	resultMaxBudget        = "error_max_budget_usd"
	resultStructuredFailed = "error_max_structured_output_retries"

	// An assistant message's error when the API refused the turn.
	assistantErrRateLimit = "rate_limit"

	mcpConnected = "connected"

	rateLimitRejected = "rejected"
	rateLimitWarning  = "allowed_warning"
)

// Content block types.
// UNVERIFIED (owner to verify with make smoke-claude)
const (
	blockText    = "text"
	blockToolUse = "tool_use"
)

// ---- Rate and plan limits ------------------------------------------------------------------

// rateLimitWords, found case-insensitively in an error, say the plan's usage limit or the
// API's rate limit was hit ("Claude AI usage limit reached|<reset>", "5-hour limit reached ∙
// resets 3pm", "API Error: 429 … rate_limit_error").
// UNVERIFIED (owner to verify with make smoke-claude)
var rateLimitWords = []string{"usage limit", "rate limit", "rate_limit", "too many requests", "429", "quota", "limit reached", "hit your limit"}

// ---- Command line --------------------------------------------------------------------------

// runOptions are what one claude invocation is built from.
type runOptions struct {
	Model    string
	Preamble string
	// PluginDirs are the task's plugins (skills/<plugin>).
	PluginDirs []string
	// Settings and MCPConfig are the generated files.
	Settings  string
	MCPConfig string
	// Schema, when set (and useJSONSchema), is the Result schema's JSON, compacted.
	Schema       string
	MaxTurns     int
	MaxBudgetUSD float64
	// ReadOnly disallows the write tools.
	ReadOnly bool
	// AddDirs are the directories besides the working directory (out/, other mounts).
	AddDirs []string
	// Resume, when set, continues that session.
	Resume string
}

// runArgs is claude's command line (after the binary): the LLD's `claude -p --output-format
// stream-json --verbose --model <model> --append-system-prompt <preamble> --plugin-dir
// skills/<plugin> --mcp-config mcp.json --json-schema <schema> --max-turns <n>
// --permission-mode <mode>` with the prompt on stdin, plus the settings file and its sources,
// the strict MCP config, the disallowed write tools of a read-only run, the extra directories
// and the resumed session.
func runArgs(o runOptions) []string {
	args := []string{flagPrint, flagOutputFormat, outputStreamJSON, flagVerbose}
	if o.Model != "" {
		args = append(args, flagModel, o.Model)
	}
	if o.Resume != "" {
		args = append(args, flagResume, o.Resume)
	}
	if o.Preamble != "" {
		args = append(args, flagAppendSystemPrompt, o.Preamble)
	}
	for _, d := range o.PluginDirs {
		args = append(args, flagPluginDir, d)
	}
	if o.Settings != "" {
		args = append(args, flagSettings, o.Settings)
	}
	args = append(args, flagSettingSources, settingSourcesUser)
	if o.MCPConfig != "" {
		args = append(args, flagMCPConfig, o.MCPConfig, flagStrictMCPConfig)
	}
	if o.Schema != "" && useJSONSchema {
		args = append(args, flagJSONSchema, o.Schema)
	}
	if o.MaxTurns > 0 {
		args = append(args, flagMaxTurns, strconv.Itoa(o.MaxTurns))
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, flagMaxBudget, strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64))
	}
	args = append(args, flagPermissionMode, permissionBypass)
	if o.ReadOnly {
		args = append(args, flagDisallowedTools, strings.Join(writeTools, ","))
	}
	for _, d := range o.AddDirs {
		args = append(args, flagAddDir, d)
	}
	return args
}

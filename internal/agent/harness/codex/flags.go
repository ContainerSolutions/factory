package codex

import (
	"path"
	"strconv"
	"strings"
)

// This file is every part of the Codex CLI's interface the adapter relies on: the minimum
// version, subcommands, flags, config keys, file names and the JSONL event names. The LLD
// (harness-adapters.md §Codex) marks most of them *verify*. The owner decided (2026-10-19)
// that codex isn't run while the adapter is written, so none of them was checked against an
// installed codex: each is marked UNVERIFIED and is fixed here, in one place, after
// `make smoke-codex`.

// ---- Version and login ---------------------------------------------------------------------

// MinVersion is the oldest codex the adapter accepts: the one with `exec --json` emitting
// the thread/turn/item JSONL events, `--output-schema` and skills in $CODEX_HOME/skills.
// UNVERIFIED (owner to verify with make smoke-codex)
const MinVersion = "0.72.0"

// versionArgs print the version, as "codex-cli 0.72.0".
// UNVERIFIED (owner to verify with make smoke-codex)
var versionArgs = []string{"--version"}

// loginStatusArgs report how $CODEX_HOME is signed in; exit 0 when it is.
// UNVERIFIED (owner to verify with make smoke-codex)
var loginStatusArgs = []string{"login", "status"}

// What `codex login status` prints for a ChatGPT (subscription) login and for an API key.
// UNVERIFIED (owner to verify with make smoke-codex)
const (
	loginSubscriptionMarker = "ChatGPT"
	loginAPIKeyMarker       = "API key"
)

// loginCommand is what the owner runs once to sign the dedicated CODEX_HOME in (in errors).
// UNVERIFIED (owner to verify with make smoke-codex)
const loginCommand = "codex login --device-auth"

// ---- Files in CODEX_HOME -------------------------------------------------------------------

// authFile is the login codex keeps in CODEX_HOME. The scratch CODEX_HOME links to the
// owner's; codex must write a refreshed token through the link (open + truncate), not
// replace the file (write a temporary file and rename it over).
// UNVERIFIED (owner to verify with make smoke-codex)
const authFile = "auth.json"

// configFile is codex's config in CODEX_HOME.
// UNVERIFIED (owner to verify with make smoke-codex)
const configFile = "config.toml"

// agentsFile in CODEX_HOME holds the user-level instructions codex adds to every session.
// UNVERIFIED (owner to verify with make smoke-codex)
const agentsFile = "AGENTS.md"

// skillsDir in CODEX_HOME is where codex discovers skills: skills/<name>/SKILL.md (the LLD's
// fallback, a repo-local .agents/skills/, isn't used: it would put files in the checkout).
// UNVERIFIED (owner to verify with make smoke-codex)
const skillsDir = "skills"

// ---- exec ----------------------------------------------------------------------------------

// The exec subcommand and its flags.
const (
	// UNVERIFIED (owner to verify with make smoke-codex)
	cmdExec = "exec"
	// cmdResume continues a session: `codex exec [flags] resume <thread id> -`. The exec flags
	// go before `resume`.
	// UNVERIFIED (owner to verify with make smoke-codex)
	cmdResume = "resume"
	// flagJSON streams JSONL events on stdout.
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagJSON = "--json"
	// flagModel picks the model.
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagModel = "--model"
	// flagConfig overrides one config key: -c key=<TOML value>.
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagConfig = "-c"
	// flagSandbox is the sandbox mode, always workspace-write (see workDirFor; the LLD notes
	// 2026 renames towards a permission profile).
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagSandbox = "--sandbox"
	// flagCd is the directory codex works in: the writable checkout, else out/ (workDirFor).
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagCd = "--cd"
	// flagOutputSchema is the JSON Schema the final message must match. OpenAI's strict
	// structured outputs accept a subset of JSON Schema (no remote $ref, every property
	// required) the Result schemas are outside of: not passed, see useOutputSchema.
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagOutputSchema = "--output-schema"
	// flagLastMessage writes the final message to a file.
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagLastMessage = "--output-last-message"
	// flagSkipGitRepoCheck lets exec run outside a git checkout (in out/ when the checkout is
	// read-only or there is none).
	// UNVERIFIED (owner to verify with make smoke-codex)
	flagSkipGitRepoCheck = "--skip-git-repo-check"
	// promptStdin reads the prompt from stdin.
	// UNVERIFIED (owner to verify with make smoke-codex)
	promptStdin = "-"
)

// useOutputSchema passes --output-schema (the LLD's invocation). It is off: codex sends the
// schema as an OpenAI strict structured output, which needs every property required and
// additionalProperties false everywhere, so it can't express the Result (optional fields,
// data a free-form map). The owner's smoke run (codex 0.163.0, 2026-10-20) failed with
// 400 "Invalid schema for response_format 'codex_output_schema': 'schema.required' is
// required to include every key in properties. Missing 'outcome'." The agent still writes
// $FACTORY_RESULT (else the final message, --output-last-message, is the Result), which the
// pipeline validates with its repair turn. The code path stays: flipping this constant
// passes the workspace's schema again.
const useOutputSchema = false

// sandboxWorkspaceWrite is the sandbox every run gets. codex's read-only mode isn't used: it
// denies the agent every write ($FACTORY_OUT, $FACTORY_RESULT, TMPDIR) and the network (the
// `factory` CLI), so a read-only checkout runs workspace-write from out/ instead (workDirFor).
// UNVERIFIED (owner to verify with make smoke-codex)
const sandboxWorkspaceWrite = "workspace-write"

// Config keys set with -c. The LLD's `--ask-for-approval never` is a top-level codex flag
// that exec doesn't take as far as known, so the approval policy goes through -c.
// UNVERIFIED (owner to verify with make smoke-codex)
const (
	keyEffort     = "model_reasoning_effort"
	keyApproval   = "approval_policy"
	approvalNever = "never"
)

// ---- config.toml ---------------------------------------------------------------------------

// Keys of the generated config.toml (see renderConfig).
// UNVERIFIED (owner to verify with make smoke-codex)
const (
	// keyUpdateCheck off: no update check (and no version file) at every start.
	keyUpdateCheck = "check_for_update_on_startup"
	// tableSandboxWrite tunes workspace-write: network_access (git, gh and the factory CLI
	// need the network) and writable_roots (out/, the scratch caches and the writable
	// checkouts' git dirs besides the cwd, see gitDirName).
	tableSandboxWrite = "sandbox_workspace_write"
	keyNetworkAccess  = "network_access"
	keyWritableRoots  = "writable_roots"
	// tableShellEnv: commands get the whole run environment. By default codex drops
	// variables named like *KEY*, *SECRET*, *TOKEN*, which would drop FACTORY_TOKEN and
	// GH_TOKEN.
	tableShellEnv     = "shell_environment_policy"
	keyInherit        = "inherit"
	keyIgnoreExcludes = "ignore_default_excludes"
	// tableMCPServers.<name>: command, args, env_vars, the names of the variables passed
	// through from codex's environment (values never written to disk), and
	// default_tools_approval_mode (keyMCPToolsApproval).
	tableMCPServers = "mcp_servers"
	keyMCPCommand   = "command"
	keyMCPArgs      = "args"
	keyMCPEnvVars   = "env_vars"
	// keyMCPToolsApproval = "approve" runs the factory server's tools without asking. With
	// approval_policy never, codex otherwise denies them in a workspace-write sandbox: a live
	// plan-epic run (2026-10-21) reported "the MCP result tool required unavailable
	// approval". From codex's source (openai/codex at d138398, 2026-10-21; the same in
	// rust-v0.163.0), not a run:
	//   - codex-rs/core/src/mcp_tool_call.rs: the mode is the server's tool_approval_mode;
	//     maybe_request_mcp_tool_approval skips approval when
	//     mcp_permission_prompt_is_auto_approved or !requires_mcp_tool_approval_for_mode;
	//     else request_mcp_tool_user_approval denies the call under approval_policy never
	//     ("MCP tool call requires approval, but approval policy is never");
	//   - codex-rs/codex-mcp/src/mcp/mod.rs: mcp_permission_prompt_is_auto_approved is true
	//     for mode approve, else under never only with full disk write access (not
	//     workspace-write);
	//   - requires_mcp_tool_approval_for_mode: the default mode, auto, asks unless the tool's
	//     annotations say readOnlyHint (or destructiveHint and openWorldHint both false);
	//     approve never asks;
	//   - codex-rs/config/src/mcp_types.rs: AppToolApproval (auto, prompt, writes, approve,
	//     snake_case) and McpServerConfig.default_tools_approval_mode, overridden per tool by
	//     tools.<name>.approval_mode; codex-rs/codex-mcp/src/server.rs:
	//     McpServerMetadata::tool_approval_mode resolves them.
	// Only the factory server gets it: the Effector checks every op against the task's
	// allowed effects itself.
	keyMCPToolsApproval = "default_tools_approval_mode"
	mcpToolsApprove     = "approve"
)

// gitDirName and gitDirPointerKey are git's names for a checkout's git dir: .git, a directory
// or a "gitdir: <path>" pointer file. codex's workspace-write keeps <writable root>/.git (and
// the gitdir a pointer names) read-only, the cwd included, unless that path is itself a
// writable root; gitDirs lists the writable checkouts' git dirs for that. From codex's source
// (openai/codex at af91f6e, 2026-10-21), not a run:
//   - codex-rs/protocol/src/permissions.rs: get_writable_roots_with_cwd_impl and
//     from_legacy_sandbox_policy_for_cwd skip a default read-only subpath (.git, the
//     resolved gitdir) that has an explicit entry; metadata_write_denial allows a write
//     under a writable root at or below the protected path; at equal depth write beats read;
//   - codex-rs/sandboxing/src/seatbelt.rs: each writable root is its own (allow file-write*)
//     clause, so the git dir's clause grants it whatever the cwd's clause excludes;
//   - codex-rs/linux-sandbox/src/bwrap.rs: writable roots are bound shallowest first, so the
//     git dir's rw bind comes after the cwd's read-only carveouts.
//
// UNVERIFIED (owner to verify with a release-docs or implement run that commits and pushes)
const (
	gitDirName       = ".git"
	gitDirPointerKey = "gitdir"
)

// ---- Effort --------------------------------------------------------------------------------

// effortMapping maps a normalised effort to model_reasoning_effort (the LLD's mapping).
// UNVERIFIED (owner to verify with make smoke-codex)
var effortMapping = map[string]string{
	"minimal": "minimal",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"max":     "high",
}

// xhighModels take model_reasoning_effort=xhigh, which max maps to for them.
// UNVERIFIED (owner to verify with make smoke-codex)
var xhighModels = []string{"*-codex-max", "*-codex-max-*"}

// noMinimalModels don't take minimal (the codex-tuned models start at low), so minimal maps
// to low for them.
// UNVERIFIED (owner to verify with make smoke-codex)
var noMinimalModels = []string{"*-codex", "*-codex-*"}

// nativeEffort is the model_reasoning_effort for a normalised effort and model: the
// mapping, adjusted for the models above. An unknown effort passes through unchanged.
func nativeEffort(model, effort string) string {
	v, ok := effortMapping[effort]
	if !ok {
		return effort
	}
	switch {
	case effort == "max" && matchAny(xhighModels, model):
		return "xhigh"
	case v == "minimal" && matchAny(noMinimalModels, model):
		return "low"
	}
	return v
}

func matchAny(globs []string, s string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, s); ok {
			return true
		}
	}
	return false
}

// ---- JSONL stream --------------------------------------------------------------------------

// Event types of `codex exec --json`.
// UNVERIFIED (owner to verify with make smoke-codex)
const (
	evThreadStarted = "thread.started" // {thread_id}
	evTurnStarted   = "turn.started"
	evTurnCompleted = "turn.completed" // {usage: {input_tokens, cached_input_tokens, output_tokens}}
	evTurnFailed    = "turn.failed"    // {error: {message}}
	evItemStarted   = "item.started"   // {item: {id, type, …}}
	evItemUpdated   = "item.updated"
	evItemCompleted = "item.completed"
	evError         = "error"       // {message}
	evTokenCount    = "token_count" // older streams: {info: {last_token_usage: {…}}}
)

// Item types inside item.* events.
// UNVERIFIED (owner to verify with make smoke-codex)
const (
	itemAgentMessage = "agent_message"     // {text}
	itemReasoning    = "reasoning"         // {text}
	itemCommand      = "command_execution" // {command, aggregated_output, exit_code, status}
	itemFileChange   = "file_change"       // {changes: [{path, kind}], status}
	itemMCPToolCall  = "mcp_tool_call"     // {server, tool, status}
	itemWebSearch    = "web_search"        // {query}
	itemTodoList     = "todo_list"         // {items: [{text, completed}]}
	itemError        = "error"             // {message}
)

// ---- Rate and plan limits ------------------------------------------------------------------

// rateLimitWords, found case-insensitively in an error, say the plan's usage limit or the
// API's rate limit was hit.
// UNVERIFIED (owner to verify with make smoke-codex)
var rateLimitWords = []string{"usage limit", "rate limit", "rate_limit", "too many requests", "429", "quota"}

// ---- helpers -------------------------------------------------------------------------------

// configArg is one -c override: key=<TOML string>.
func configArg(key, value string) string { return key + "=" + tomlString(value) }

// tomlString is a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(`\u`)
				h := strconv.FormatInt(int64(r), 16)
				b.WriteString(strings.Repeat("0", 4-len(h)) + h)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlList is a TOML array of strings.
func tomlList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// execOptions are what one exec invocation is built from.
type execOptions struct {
	Model   string
	Effort  string // normalised
	Sandbox string
	WorkDir string
	// Schema, when set and useOutputSchema (off), is passed as --output-schema.
	Schema string
	// LastMessage is the file the final message is written to.
	LastMessage string
	// Resume, when set, continues that thread instead of starting one.
	Resume string
}

// execArgs is codex's command line (after the binary) for one exec: the LLD's
// `codex exec --json -m <model> -c model_reasoning_effort=<effort> --sandbox <mode>
// --ask-for-approval never --output-schema <schema> -C repo/ -`, with the approval policy
// as a -c override, the final message to a file and the git-repo check off.
func execArgs(o execOptions) []string {
	args := []string{cmdExec, flagJSON}
	if o.Model != "" {
		args = append(args, flagModel, o.Model)
	}
	if o.Effort != "" {
		args = append(args, flagConfig, configArg(keyEffort, nativeEffort(o.Model, o.Effort)))
	}
	args = append(args, flagConfig, configArg(keyApproval, approvalNever))
	if o.Sandbox != "" {
		args = append(args, flagSandbox, o.Sandbox)
	}
	if o.WorkDir != "" {
		args = append(args, flagCd, o.WorkDir)
	}
	args = append(args, flagSkipGitRepoCheck)
	if o.Schema != "" && useOutputSchema {
		args = append(args, flagOutputSchema, o.Schema)
	}
	if o.LastMessage != "" {
		args = append(args, flagLastMessage, o.LastMessage)
	}
	if o.Resume != "" {
		args = append(args, cmdResume, o.Resume)
	}
	return append(args, promptStdin)
}

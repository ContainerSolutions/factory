# LLD — Harness adapters (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: AGT-S1-T03 (Codex), AGT-S1-T04 (Claude Code) · Package: `internal/agent/harness/{codex,claudecode,fake}`
> Design: [05 §Harness adapter contract](../../../05-agent-runtime.md#harness-adapter-contract)

> **Rule:** every flag marked **verify** must be checked against the installed CLI (`--help`, docs) at the start of the task; the PR body lists each with ✅/changed.
>
> **Phase 10:** the adapters run only in a VM workspace ([In the VM](#in-the-vm-p1-phase-3)). The host path is gone, and so are its decisions here: `Probe` (supervise offers a harness by its stored login), the OS sandboxes and their allow-lists, the owner's config dirs and login links, and `make smoke-codex`/`smoke-claude`. The decisions below are AGT-S1-T03/T04's, kept for their flags, stream mapping, MCP wiring and hook.

## Interface

```go
type Adapter interface {
    Name() string
    Capabilities() v1alpha1.AdapterCapabilities                     // static: effort mapping, features
    Prepare(ctx context.Context, ws Workspace, t v1alpha1.Task) error
    Run(ctx context.Context, ws Workspace, t v1alpha1.Task, prompt string, sink EventSink) (Raw, error)
    Repair(ctx context.Context, ws Workspace, problems []string, sink EventSink) (Raw, error)
}
type EventSink interface { Progress(ProgressEvent); Usage(UsageDelta); RateLimited(until time.Time) }
```

`ProgressEvent{Kind: turn|tool|message|error, Summary string, At time.Time}` — summaries only (≤ 500 chars); full detail goes to the transcript.

## Codex

| Concern | Mapping |
|---|---|
| Binary | `codex` in the workspace image ([workspace-host.md](workspace-host.md)) |
| Auth | the owner's ChatGPT login, stored by `factory-agent login codex`; the VM gets a placeholder `auth.json` with the account's identifiers, and the gateway injects the real token |
| Isolation | the VM. Per run: `CODEX_HOME` = scratch dir with the placeholder `auth.json`, a generated `config.toml`, `AGENTS.md`, and `skills/` |
| Invocation | `codex exec --json -m <model> -c model_reasoning_effort=<effort> -c approval_policy=never --sandbox danger-full-access --cd <workdir> -` (prompt on stdin) — **verify** (2026 renamed sandbox options exist in the action: `permission-profile`) |
| Skills | copy `SKILL.md` folders to `$CODEX_HOME/skills/<name>/` (**verify** path; fallback: repo-local `.agents/skills/`) |
| MCP (Effector) | `config.toml`: `[mcp_servers.factory] command = "factory-agent" args = ["mcp"]` with env passthrough |
| System prompt | `AGENTS.md` in `CODEX_HOME` holds the headless preamble; task prompt on stdin |
| Effort mapping | `minimal→minimal, low→low, medium→medium, high→high, max→high` (**verify** `xhigh` availability) |
| Stream | JSONL events on stdout → map `turn.*`, `item.*` (tool calls, messages), `token_count`/usage events → `Usage`; errors containing rate/usage-limit wording or HTTP 429 → `RateLimited(reset)` (parse reset if present, else +15 min) |
| Result | `--output-schema` writes structured final message → `out/result.json`; else last message JSON |
| Limits | turns counted from events; runner enforces wall-clock |

## Claude Code

| Concern | Mapping |
|---|---|
| Binary | `claude` in the workspace image ([workspace-host.md](workspace-host.md)) |
| Auth | the owner's subscription token, stored by `factory-agent login claude`; the VM gets a placeholder `CLAUDE_CODE_OAUTH_TOKEN` (or `ANTHROPIC_API_KEY`), and the gateway injects the real one. `CLAUDE_CONFIG_DIR` is a scratch dir of the run's `HOME` |
| Isolation | the VM. `--settings <generated.json>` (hooks, `enabledPlugins: {}`, no sandbox or permissions), no project `.claude/` from the target repo (`--setting-sources` limited, **verify**), skills only via `--plugin-dir` |
| Invocation | `claude -p --output-format stream-json --verbose --model <model> --append-system-prompt <preamble> --plugin-dir skills/<plugin> --mcp-config mcp.json --json-schema <schema> --max-turns <n> --permission-mode <mode>` (prompt on stdin) — **verify** each |
| Read-only workspace | `--permission-mode` + `--disallowedTools Write,Edit,NotebookEdit` + a `PreToolUse` hook rejecting writes outside `out/` and Bash commands matching `git push\|gh (pr\|issue\|api)` |
| Effort mapping | to the CLI's effort/thinking setting (**verify** flag or settings key, e.g. `effortLevel`); `max` → highest available |
| Stream | stream-json: `system/init` (confirms plugins loaded; fail if `plugin_errors`), `assistant`/`user` (tool_use/tool_result) → progress, `result` (usage, `num_turns`, `total_cost_usd` → only meaningful for API keys) ; rate-limit errors → `RateLimited` |
| Result | `--json-schema` structured output → `out/result.json` |

## In the VM (P1 phase 3)

Both adapters run only in `factory-agent run` inside a VM workspace, where the VM is the boundary: claude gets no `sandbox` or `permissions` settings, a scratch `CLAUDE_CONFIG_DIR` and a placeholder `CLAUDE_CODE_OAUTH_TOKEN` (or `ANTHROPIC_API_KEY`), and keeps its PreToolUse hook; codex runs `--sandbox danger-full-access` with no `[sandbox_workspace_write]` or `workDirFor`, and a placeholder `auth.json` built from the account identifiers in the run bundle, in Phase 0's shape. See [workspace-run.md](workspace-run.md#harnesses-in-the-vm).

## Fake adapter

Scriptable (`fake.Script{Events, Result, Err, RateLimitAfter}`) for pipeline and worker tests.

## Tests

- Golden parsing of recorded streams (`testdata/codex-*.jsonl`, `testdata/claude-*.jsonl`) → progress/usage/rate-limit events.
- Command-line construction tables per TaskSpec variant (read-only vs rw, effort, schema).
- The pipeline end to end with each real adapter on its fake binary, as a VM workspace runs it (`harnesstest.E2E.Pipeline`: gateway mode through `internal/fakegateway`, from the run bundle). The real harnesses run only in a VM workspace; there is no host smoke target.
- The fake binaries are wrapper scripts the test writes and then execs. Every script a `factory-agent` test execs (these, the pipeline's and effector's git wrappers, the provisioning stubs) is written through `internal/agent/internal/exectest` (test-only), a wrapper over `execfile`, which holds `syscall.ForkLock` around the write as production code does ([run-pipeline.md](run-pipeline.md#decisions-taken-in-agt-s1-t02)): a child that parallel tests fork meanwhile would otherwise inherit the file open for writing, and exec'ing it then fails with ETXTBSY ("text file busy").

## Decisions taken in AGT-S1-T03

- **Nothing was run against a real codex.** The owner decided (2026-10-19) that codex isn't run while the adapter is written, not even `--help`. So every part of codex's interface the adapter relies on is a constant in `internal/agent/harness/codex/flags.go`, each marked `UNVERIFIED` (now "owner to verify on a VM workspace run"), to be fixed there in one place. The unverified list:
  - `auth.json`, `config.toml`, `AGENTS.md` and `skills/<name>/SKILL.md` in `CODEX_HOME`;
  - `exec`, `--json`, `--model`, `-c key=<TOML>`, `--sandbox danger-full-access`, `--cd`, `--skip-git-repo-check`, `--output-schema`, `--output-last-message`, the prompt as `-` on stdin, and `exec [flags] resume <thread id> -` with the exec flags before `resume`;
  - `-c approval_policy="never"`, which replaces the LLD's `--ask-for-approval never`: as far as known that is a top-level flag `exec` doesn't take;
  - `-c model_reasoning_effort=…` and its values: `minimal`, `low`, `medium`, `high`, and `xhigh` for the `*-codex-max*` models, which `max` maps to there; `minimal` maps to `low` on the `*-codex*` models;
  - the `config.toml` keys: `check_for_update_on_startup`, `[shell_environment_policy] inherit` and `ignore_default_excludes`, and `[mcp_servers.<name>] command`, `args`, `env_vars` and `default_tools_approval_mode = "approve"`;
  - the placeholder `auth.json` keys (Phase 0's shape, exactly: `OPENAI_API_KEY` null, `last_refresh`, `tokens.{id_token, access_token, refresh_token, account_id}`, and the id_token's claims `email`, `exp` and `https://api.openai.com/auth` with `chatgpt_account_id`, `chatgpt_user_id` and `chatgpt_plan_type`), the id_token's non-empty dummy signature segment (`x`, as Phase 0's), and that a fresh `last_refresh` keeps codex from refreshing;
  - the JSONL event and item names below, `turn.completed`'s usage being per turn rather than cumulative, and the rate-limit wording;
  - `useOutputSchema` (true), i.e. that codex accepts the Result schemas for `--output-schema`.
- **Package.** `internal/agent/harness/codex`; vmrun's `taskAdapters` builds it for a codex task, with the run bundle's account. Every codex invocation goes through `Options.Binary`, so tests run a fake codex.
- **Prepare.** The scratch `CODEX_HOME` is `<workspace>/home/.codex`, inside the pipeline's scratch `HOME`, and holds:
  - `auth.json`, the placeholder login (`placeholderAuth`, [workspace-run.md](workspace-run.md#codex)): a new 0600 file, never written over an existing file or link;
  - a generated `config.toml`: no update check; commands given the whole run environment (codex's default drops `*TOKEN*` names, which would drop `FACTORY_TOKEN` and `GH_TOKEN`); and `[mcp_servers.factory]`, with `command` the workspace's `bin/factory` and `args = ["mcp"]`. The orchestrator's brief names that `factory-agent fx mcp`; effector.md and worker.md say `factory-agent mcp`, so AGT-S1-T05 settles it (it is `clirun.EffectorArgs` and `clirun.FactoryCLI` since AGT-S1-T04, shared by both adapters). The MCP server's variables go through `env_vars` **by name**: the names of every variable in the run environment, never their values, so no token is written to the file. The server's tools get `default_tools_approval_mode = "approve"` (see item 3 of the gaps below);
  - `AGENTS.md`, holding the headless preamble and, for a read-only primary checkout, a line saying not to change it (nothing in the VM stops the write; the gateway refuses the push);
  - `skills/<name>/`, copied from the pipeline's plugin layout (the skill folder only). When two plugins have a skill of the same name, both get the folder name `<plugin>-<name>`. Symlinks are skipped, and scripts keep their executable bit. The plugin's root files (`bin/`, `rules/`, …) stay in the pipeline's `skills/<plugin>/`, in the workspace: its `bin/` is on `PATH` (the pipeline puts it there), and a plugin CLI finds the rest from its own location (`ccfr` resolves `bin/../lib` and the plugin root from its real path). Each plugin's `bin/` is also linked at `skills/<plugin>/bin` (unless a skill folder of that name has its own `bin/`), because a skill may name another plugin's CLI beside its own folder: lisa's self-review runs `<lisa skill dir>/../ccf-review/bin/ccfr`. A link, not a copy, so the CLI's real path is still its plugin's.

  The LLD's fallback, a repo-local `.agents/skills/`, isn't used: it would put files in the checkout.
- **Run.** The invocation is `codex exec --json --model <m> -c model_reasoning_effort="<e>" -c approval_policy="never" --sandbox danger-full-access --cd <workdir> --skip-git-repo-check --output-schema <ws.Schema> --output-last-message <CODEX_HOME>/factory-last-message.json -`, with the prompt on stdin:
  - The model is the resolved one, else the config's default; with neither, there is no `--model`.
  - `--skip-git-repo-check` is there because a task without a checkout works in the workspace root.
  - There is no codex sandbox (`danger-full-access`): the VM is the boundary. The workdir (`--cd`, and the process's directory) is the pipeline's `WorkDir`, else the workspace root.
  - The environment is the pipeline's `Workspace.Env` plus `CODEX_HOME`.
  - codex runs in its own process group. Cancelling the context sends SIGTERM to the whole group, codex is SIGKILLed after `KillGrace` (5 s, as `exec.Cmd.WaitDelay`), and the rest of the group gets SIGKILL once codex has exited. That always happens, so commands it left running don't outlive the run.
  - The transcript is the stdout lines as they came, plus `factory.codex-exec` (the args), `factory.codex-stderr` (the last 64 KB) and, past 64 MB, `factory.codex-transcript-truncated` lines.
- **Stream mapping.**
  - Each `turn.started` is one `turn` event.
  - Each `command_execution`, `file_change`, `mcp_tool_call` and `web_search` item is one `tool` event, at `item.started`, or at `item.completed` when its start wasn't streamed; `item.updated` is ignored.
  - A completed `agent_message` is a `message` event and becomes the final message. A `todo_list` is a `plan: n/m done` message. `reasoning` goes to the transcript only.
  - `turn.completed.usage` (`input_tokens` including the cached ones, and `output_tokens`) is a usage delta. The older `token_count` events count only when no `turn.completed` has carried usage.
  - `turn.failed`, `error` and `error` items are `error` events. Only `turn.failed`, or a non-zero exit, fails the run, because codex also reports retries as `error` events.
- **Rate and plan limits.** An error whose text contains `usage limit`, `rate limit`, `too many requests`, `429` or `quota`, in the stream or in the last stderr line after a failed exit, reports `RateLimited` once. The reset is taken from the first of these that is present: a `resets_in_seconds`, `retry_after_seconds` or `resets_at` field; `try again in 2 days 3 hours 5 minutes` (or `1h20m`); `try again at 3:05 PM` (the next such local time) or at a date; or RFC 3339. Otherwise it is now + 15 min. Resets in the past or more than 8 days ahead are ignored. A 429 that codex only retries also throttles the agent.
- **Result.** `$FACTORY_RESULT` still wins (that is the pipeline's rule). `Raw.Result` is the `--output-last-message` file, else the stream's last agent message, with any Markdown code fence around it stripped. The agent can write `$FACTORY_RESULT` in every workspace, a read-only one included.
- **Repair** resumes the session: `codex exec … resume <thread_id> -` with the validation problems, asking for the corrected Result only, both to `$FACTORY_RESULT` and as the final message. The thread id and the run's model, effort, sandbox and workdir are kept in `CODEX_HOME/factory-session.json` (so a repair runs where the run ran), and the prompt in `factory-prompt.md`, so the Adapter keeps no per-run state and one Adapter serves concurrent runs. Without a thread id, a new exec gets the original prompt followed by the problems.
- **LLD gaps and risks for the owner's first VM runs.**
  1. **`maxTurns` and Codex turns.** `maxTurns` counts codex's own turns, and `exec` and each `resume` is one turn. So for Codex `maxTurns` barely bites and `maxWallClock` is the real bound. Counting tool calls as turns instead is a one-line change in `stream.go`.
  2. **`--output-schema` is strict.** OpenAI's structured outputs take a strict subset of JSON Schema, with no remote `$ref` and every property required, and the Result and spec schemas may not fit it. If codex refuses them, set `useOutputSchema` to false: the Result still comes from `$FACTORY_RESULT` or the final message.
  3. **MCP tool approval.** Read from codex's source (openai/codex at `d138398`, 2026-10-21, and the same in `rust-v0.163.0`), not a run: `codex-rs/core/src/mcp_tool_call.rs` `maybe_request_mcp_tool_approval` skips approval only when `mcp_permission_prompt_is_auto_approved` (`codex-rs/codex-mcp/src/mcp/mod.rs`: the tool's approval mode is `approve`, or the policy is `never` with full disk write access) or when `requires_mcp_tool_approval_for_mode` says none is needed (the default mode, `auto`, asks unless the tool is annotated `readOnlyHint`, or `destructiveHint` and `openWorldHint` false; the Effector's tools carry no annotations). Otherwise `request_mcp_tool_user_approval` denies the call under `approval_policy = "never"` ("MCP tool call requires approval, but approval policy is never"), as a host plan-epic run (2026-10-21) showed. The mode comes from `[mcp_servers.<name>] default_tools_approval_mode`, overridden per tool by `tools.<tool>.approval_mode` (`codex-rs/config/src/mcp_types.rs` `AppToolApproval`: `auto`, `prompt`, `writes`, `approve`; `codex-rs/codex-mcp/src/server.rs` `McpServerMetadata::tool_approval_mode`). So `[mcp_servers.factory]` sets `default_tools_approval_mode = "approve"`, for that server only: the Effector checks every op against the task's allowed effects itself. `danger-full-access` may make the key unneeded; no VM run has shown it, so it stays.
  4. **Usage may be cumulative.** If `turn.completed.usage` is cumulative per thread, a resumed repair double-counts it.
- **Tests** never run codex. The test binary is the fake codex, behind a wrapper script: it replays a recorded JSONL file, writes `$FACTORY_RESULT` or the last-message file, records its arguments, environment, stdin and `CODEX_HOME` listing, and can start a child that ignores SIGTERM. The tests cover:
  - golden parsing of `testdata/codex-*.jsonl` (success, multi-turn, a usage limit with its reset, a 429, `resets_in_seconds`, a failed turn with retries, the older `token_count`), with `-update`, plus the semantics behind the goldens;
  - reset parsing;
  - flag assembly for every effort and model family, a writable checkout and a read-only one, resume;
  - Prepare's layout: the placeholder `auth.json` (and its refusal to replace an existing one), the flattened skills with a name clash, `config.toml` with no token in it and the factory server's tools approved, and a read-only checkout's `AGENTS.md`;
  - Run's failures, and stderr-only limits;
  - Repair by resume, from the run's stored workdir, and without a thread;
  - cancel killing a process tree that ignores SIGTERM, and a rate limit stopping a blocked run;
  - the pipeline end to end with the repair turn and a throttled run.

## Decisions taken in AGT-S1-T04

- **Nothing was run against a real claude.** As for codex, the owner decided (2026-10-19) that claude isn't run while the adapter is written, not even `--help`, and that no file of a claude login is read. Every part of claude's interface the adapter relies on is a constant in `internal/agent/harness/claudecode/flags.go` (plus the hook's input type in `hook.go` and the reset formats in `ratelimit.go`), each marked `UNVERIFIED (owner to verify on a VM workspace run)`. The unverified list:
  - `CLAUDE_CONFIG_DIR`; `ANTHROPIC_API_KEY` taking precedence over a subscription login; `CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token`);
  - `DISABLE_AUTOUPDATER=1` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`;
  - `IS_SANDBOX=1` letting a root run use `bypassPermissions`;
  - `--print` reading the prompt from stdin, `--output-format stream-json` with `--verbose`, `--model`, `--append-system-prompt`, `--plugin-dir` (once per plugin), `--settings <file>`, `--setting-sources user`, `--mcp-config <file>` with `--strict-mcp-config`, `--json-schema` taking the schema **inline** (not a path), `--max-turns`, `--max-budget-usd`, `--permission-mode bypassPermissions`, `--disallowedTools` as one comma-separated list, `--add-dir`, `--resume <session id>` from the same working directory;
  - `useJSONSchema` (true), i.e. that claude accepts the Result schemas (local `$defs`/`$ref`, `oneOf`, `additionalProperties: false`);
  - the settings keys `enabledPlugins`, `effortLevel` (the LLD's example) and `hooks.PreToolUse[].{matcher, hooks[].{type: command, command, timeout}}`, the matcher being a regular expression over tool names;
  - the hook protocol: `{tool_name, tool_input, cwd}` on stdin, exit 0 allows, exit 2 blocks and hands stderr to the model; the hook runs under `bypassPermissions` too;
  - the tool names `Bash`, `Write`, `Edit`, `MultiEdit`, `NotebookEdit`, `Read`, `Grep`, `Glob`, `WebSearch`, `WebFetch`, `Task`, `Skill`, `TodoWrite`, `StructuredOutput` and `mcp__<server>__<tool>`, and their input fields (`command`, `file_path`, `notebook_path`, `pattern`, `query`, `url`, `description`, `skill`, `todos[].status`);
  - the MCP config shape `{"mcpServers": {<name>: {type: stdio, command, args, env}}}` with `${NAME}` expanded from claude's environment;
  - the effort mapping `minimal→low, low→low, medium→medium, high→high, max→high`;
  - the stream-json events and fields below, and the rate-limit wording and reset formats.
- **Shared code.** What both adapters need moved out of `codex` into two new packages, and codex now uses them with no change in behaviour: `internal/agent/harness/clirun` (the process-group run and cancel, line splitting, the bounded transcript and stderr tail, reset-time parsing, version comparison, text helpers, the repair prompt, the Effector's MCP server name, arguments and `factory` CLI path, `EnvNames`) and `internal/agent/harness/harnesstest`, imported by tests only (the recording `EventSink`, and the end-to-end rig: git http-backend server, dispatcher, pipeline, job, transcript). `clirun.ParseAt` also takes `3pm`, `3:05pm` and a date without a year now.
- **Package.** `internal/agent/harness/claudecode`; vmrun's `taskAdapters` builds it for a claude-code task.
- **Prepare** writes `<workspace>/home/.factory-claude/settings.json` (`enabledPlugins: {}`, the mapped `effortLevel`, the PreToolUse hook) and `mcp.json` (`factory` = the workspace's `bin/factory` with `["mcp"]`, as codex, and every run variable passed as `${NAME}`, so no value reaches the file). The plugins are the pipeline's `skills/<plugin>` directories as they are (the plugin's whole tree with its selected skills, so `<SKILL.md dir>/../../bin` is the plugin's `bin/`, also on `PATH`). The pipeline leaves the plugins' Claude-only components out ([run-pipeline.md](run-pipeline.md) §Skills): `hooks/`, `hooks.json`, `commands/`, `agents/`, `.mcp.json`, `.lsp.json`, `output-styles/`, `monitors/`, `workflows/`, `themes/` and `settings.json`, and reduces each `plugin.json` to its identity fields (no component key, no `skills` scan paths), so `--plugin-dir` loads skills only, and the run's only hook and MCP server are the adapter's (the PreToolUse hook and the Effector); they are in the workspace. Prepare checks each has `.claude-plugin/plugin.json` and refuses a skill outside a plugin.
- **The PreToolUse hook** is `factory-agent fx claude-hook pre-tool-use --write <dir>…` (`claudecode.HookMain`, dispatched from `fx.go`), run through the workspace's `factory` shim. Its policy is on its command line, not in a file. It blocks, with exit 2 and the reason on stderr:
  - a `Write`, `Edit`, `MultiEdit` or `NotebookEdit` outside `out/`, `tmp/` and the checkouts the harness may write (relative paths against the call's `cwd`, symlinks resolved, `..` cleaned). The adapter's own files in `home/` are outside, so the model can't rewrite its hook;
  - `git push` (with global options, a path, inside `bash -c "…"`), `gh pr|issue <write>`, and `gh api` with a non-GET `-X/--method`, or with fields and no method (gh then POSTs), or a GraphQL `mutation`;
  - unreadable input or arguments (fail closed).

  **Change from the LLD:** the Bash rules apply in every workspace, not only read-only ones: the headless preamble and effector.md send every GitHub change through `factory` (`factory push`). And they block gh *writes* only: the LLD's `gh (pr|issue|api)` would also block the `gh pr view`/`gh api` reads review tasks need. The hook guards against mistakes and injected instructions; it can't see what a Bash command writes, which the VM bounds, and the token scopes stay the boundary.
- **No OS sandbox.** The settings carry no `sandbox` block and no `permissions` rules: the VM is the boundary ([In the VM](#in-the-vm-p1-phase-3)). The PreToolUse hook stays, as a second layer under it (and the VM one under the scoped tokens).
- **Read-only workspace** (no writable primary checkout): `--disallowedTools Write,Edit,MultiEdit,NotebookEdit` (the LLD's list plus `MultiEdit`) and the hook. The permission mode is `bypassPermissions` in both cases: nobody can answer a prompt, and `acceptEdits` would still deny every Bash command. The Result then comes from the structured output (or a Bash write to `$FACTORY_RESULT`).
- **Run.** `claude --print --output-format stream-json --verbose [--model m] [--resume id] --append-system-prompt <preamble> --plugin-dir skills/<p>… --settings … --setting-sources user --mcp-config … --strict-mcp-config --json-schema <schema JSON> [--max-turns n] [--max-budget-usd x] --permission-mode bypassPermissions [--disallowedTools …] [--add-dir out/ and the other checkouts]`, prompt on stdin, in the workspace's working directory:
  - `--json-schema` is the workspace's Result schema (the TaskSpec's, bundled, else the v1alpha1 Result schema), inline on one line and rewritten twice (`compactSchema`, `schema.go`). **Draft-07** (`toDraft7`): claude validates it as draft-07 and refused draft 2020-12's `$schema`, so `$schema`/`$id` go, `$defs` become `definitions`, a `$ref` with siblings moves under `allOf`, and a draft-2019/2020-only keyword is refused. **A plain object root** (`objectRoot`): claude sends the schema to the Messages API as its `StructuredOutput` tool's `input_schema`, which must have `"type": "object"` at the root (the first live docs-review run failed with `API Error: 400 tools.7.custom.input_schema.type: Field required`) and no top-level `allOf`, `anyOf` or `oneOf` (`input_schema does not support oneOf, allOf, or anyOf at the top level`). Every catalog Result schema is `{"allOf": [{"$ref": <base Result>}, {if/then…}], "properties": {outcome, data}}`, so the root becomes the bundled base Result's `type`, `properties`, `required`, `additionalProperties` and `title`, with the TaskSpec's top-level `properties` merged in (a name both have is `{"allOf": [base's, TaskSpec's]}`; nested combinators are fine), `required` the union, and each other top-level `allOf` member folded when it is a plain object schema. What can't be at the root (`if`/`then`/`else`, `anyOf`, `oneOf`, `not`, any other `allOf` member) is dropped; `definitions` stay, so nested `$ref`s resolve. The rewrite only loosens: every Result the full schema accepts, claude's accepts, and the dropped rules (the TaskSpecs' "a done Result carries data…") are still checked by the pipeline against the full schema, with its repair turn, and by the dispatcher on `complete`. A root whose `type` isn't `object`, a schema that isn't a JSON object, and a `$ref` into the root's own keywords (other than `#/definitions/…`) are refused.
  - The model is the resolved one, else the config's default; with neither, claude's default. The auth is the resolved one: a subscription, or an API key.
  - `--max-turns` is `limits.maxTurns`; `--max-budget-usd` is `limits.maxCostUSD` on API keys only.
  - The environment is the pipeline's plus the scratch `CLAUDE_CONFIG_DIR` (`home/.claude`), the two variables above, `IS_SANDBOX=1` for a root run, and exactly one credential variable, `harness.Placeholder`, which the gateway replaces: `CLAUDE_CODE_OAUTH_TOKEN` on a subscription run, `ANTHROPIC_API_KEY` on an API-key run (the other is dropped). Nothing comes from the worker's environment.
  - The process group is handled as for codex (`clirun.Run`); a plugin error in `system/init` stops the run at once.
  - The transcript is the stdout lines, plus `factory.claude-exec` (the args), `factory.claude-stderr` and `factory.claude-transcript-truncated`.
- **Stream mapping.**
  - `system/init` records the `session_id`. A non-empty `plugin_errors` is fatal (the LLD), and an MCP server that isn't `connected` is an `error` event only (the Effector's `mcp` op is AGT-S1-T05's).
  - Each new top-level `assistant` message id is one `turn` (subagents' messages, with a `parent_tool_use_id`, aren't). Its text is a `message` event; each `tool_use` (subagents' too) is one `tool` event (`$ cmd`, `edit <path>`, `read`, `search`, `fetch`, `agent`, `skill`, `mcp server.tool`, `result`); `TodoWrite` is a `plan: n/m done` message; thinking goes to the transcript only.
  - Usage is per message, with `input_tokens` plus both cache counts as input (as codex counts cached input), and a message's usage counted once although its events repeat it; the `result`'s totals top up anything the stream missed. `total_cost_usd` is the cost on API-key runs only.
  - The `result` ends the run: `error_max_turns` and `error_max_budget_usd` are `LimitError`s (`maxTurns`, `maxCostUSD`); `error_max_structured_output_retries` is an error event only, and the exit status 1 claude ends with then isn't a harness error: the Raw goes to the pipeline, which checks `$FACTORY_RESULT` or the last attempt, and repairs (a docs-review run had done its work, then sent `"data": "{}"`, a string, five times); any other error subtype, or `is_error`, fails the run. `permission_denials` are an error event.
- **Rate and plan limits** report `RateLimited` once, from the first of: a `rate_limit_event` with status `rejected` (its `resetsAt`; `allowed_warning` is a message); an `assistant` message with `error: "rate_limit"`; an error `result`, or the last stderr line after a failed exit, with `usage limit`, `rate limit`, `rate_limit`, `too many requests`, `429`, `quota`, `limit reached` or `hit your limit`. The reset is the Unix time in `limit reached|<seconds>`, else `resets 3pm`/`resets Oct 21, 9am` with an optional `(IANA zone)`, else what `clirun.ResetTime` finds, else now + 15 min.
- **Result.** `$FACTORY_RESULT` still wins (the pipeline's rule). `Raw.Result` is the `result`'s `structured_output`, else the `StructuredOutput` tool call's input, else the final text without a code fence. When claude refused every `StructuredOutput` attempt, the last one is the candidate, with each top-level field whose value is a string holding a JSON object or array decoded (`decodeStringFields`; models sometimes stringify a nested object in a tool's input): it then often passes without a repair turn. Nothing else is rewritten, and an attempt claude accepted is left as it is. Repair runs with `--json-schema` too, so its output gets the same treatment. The adapter doesn't write `out/result.json` itself (the LLD's arrow), so a file the agent wrote isn't overwritten.
- **Repair** resumes the session, `--resume <session_id>` with every other flag again and `--max-turns 5`, asking for the corrected Result only. The session id and the run's model, auth, workdir, read-only flag and budget are kept in `factory-session.json` and the prompt in `factory-prompt.md` in the scratch dir, so one Adapter serves concurrent runs. Without a session id, a new run gets the original prompt followed by the problems.
- **LLD gaps and risks for the owner's first VM runs.**
  1. **`--json-schema` and the Result schema.** If claude refuses the generated schema, or the model can't satisfy `task`/`question` being required, set `useJSONSchema` to false. The smoke run passed with a root-level `allOf` that every real task then failed on: its task has no TaskSpec schema, so it got the bare base Result schema, whose root already is `type: object`. The TaskSpec schemas' shape (`objectRoot` above) is covered by the unit tests over `catalog/schemas/results/` only, and live by a real docs-review run; the Messages API may refuse more of what is nested (claude's API error then names the field).
  2. **`bypassPermissions`** may be refused (as root, or by a managed setting); `--disallowedTools` and the hook must still hold under it.
  3. **The effort key.** If `effortLevel` isn't a setting, the effort is silently ignored; the transcript's `system/init` won't show it, so check the model's behaviour or the docs.
  4. **`--setting-sources user`** may not keep the target repo's `.claude/skills`, agents or commands out, and repo `CLAUDE.md` files load as project memory (codex reads `AGENTS.md` the same way).
  5. **Cost on resume.** If `total_cost_usd` is per session rather than per process, a resumed repair double-counts it.
- **Tests** never run claude. The test binary is the fake claude behind a wrapper script: it replays a recorded stream-json file, writes `$FACTORY_RESULT`, records its arguments, environment, stdin, settings and MCP config, puts scripted tool calls to the settings' PreToolUse hooks through `sh -c` (the real `fx claude-hook` path), and can start a child that ignores SIGTERM. The tests cover:
  - golden parsing of `testdata/claude-*.jsonl` (success with structured output, multi-turn with a subagent, a todo list, a failed MCP server and a denial, a usage limit with its `|<reset>`, `resets 3pm (Europe/Lisbon)`, an API 429, a `rate_limit_event`, an error result, max turns, a plugin error, StructuredOutput retries exhausted with a stringified `data`, and with one that isn't JSON), with `-update`, plus the semantics behind the goldens;
  - reset parsing; flag assembly for read-only and read-write, resume, budget; the effort mapping;
  - Prepare's layout (settings with no sandbox or permission rules; the hook command and its writable roots per workspace; MCP config with no token) and refusals;
  - the hook's allow and block table, and `fx claude-hook` from `factory-agent`;
  - Run's arguments, environment, recorded settings and hook answers per auth and workspace, its failures (limits on the stream and on stderr, an error result, max turns, a plugin error stopping a blocked run, an unknown auth);
  - Repair by resume and without a session;
  - cancel killing a process tree that ignores SIGTERM, and a rate limit stopping a blocked run;
  - the pipeline end to end with the hook, the repair turn and a throttled run.


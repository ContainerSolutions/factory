# LLD — Harness adapters (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: AGT-S1-T03 (Codex), AGT-S1-T04 (Claude Code) · Package: `internal/agent/harness/{codex,claudecode,fake}`
> Design: [05 §Harness adapter contract](../../../05-agent-runtime.md#harness-adapter-contract)

> **Rule:** every flag marked **verify** must be checked against the installed CLI (`--help`, docs) at the start of the task; the PR body lists each with ✅/changed. Adapters pin a **minimum harness version** and refuse older ones in `Probe`.

## Interface

```go
type Adapter interface {
    Name() string
    Capabilities() v1alpha1.AdapterCapabilities                     // static: effort mapping, features
    Probe(ctx context.Context, cfg config.Harness) (v1alpha1.HarnessOffer, error)
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
| Binary / min version | `codex` ≥ the version that has `exec --json` and skills (**verify**) |
| Auth | owner signed in once: `CODEX_HOME=<cfg.home> codex login --device-auth`; Probe checks `codex login status` (**verify** command) |
| Isolation | per run: `CODEX_HOME` = scratch copy containing **only** the auth file (symlink to `<cfg.home>/auth.json`, never copied elsewhere), a generated `config.toml`, `AGENTS.md`, and `skills/` |
| Invocation | `codex exec --json -m <model> -c model_reasoning_effort=<effort> --sandbox <read-only\|workspace-write> --ask-for-approval never --output-schema <schema> -C repo/ -` (prompt on stdin) — **verify** `--sandbox`/`--ask-for-approval`/`--output-schema`/`-C` (2026 renamed sandbox options exist in the action: `permission-profile`) |
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
| Binary / min version | `claude` ≥ version with `--json-schema`, `--plugin-dir` (**verify**) |
| Auth | PoC: owner's subscription login in dedicated `CLAUDE_CONFIG_DIR=<cfg.configDir>` (**verify** env var); later: `ANTHROPIC_API_KEY` from the worker environment. Probe: `claude auth status` or a 1-turn dry call (**verify**) |
| Isolation | `--settings <generated.json>` (permissions, hooks, `enabledPlugins: {}`), no project `.claude/` from the target repo (`--setting-sources` limited, **verify**), skills only via `--plugin-dir` |
| Invocation | `claude -p --output-format stream-json --verbose --model <model> --append-system-prompt <preamble> --plugin-dir skills/<plugin> --mcp-config mcp.json --json-schema <schema> --max-turns <n> --permission-mode <mode>` (prompt on stdin) — **verify** each |
| Read-only workspace | `--permission-mode` + `--disallowedTools Write,Edit,NotebookEdit` + a `PreToolUse` hook rejecting writes outside `out/` and Bash commands matching `git push\|gh (pr\|issue\|api)` |
| Effort mapping | to the CLI's effort/thinking setting (**verify** flag or settings key, e.g. `effortLevel`); `max` → highest available |
| Stream | stream-json: `system/init` (confirms plugins loaded; fail if `plugin_errors`), `assistant`/`user` (tool_use/tool_result) → progress, `result` (usage, `num_turns`, `total_cost_usd` → only meaningful for API keys) ; rate-limit errors → `RateLimited` |
| Result | `--json-schema` structured output → `out/result.json` |

## Fake adapter

Scriptable (`fake.Script{Events, Result, Err, RateLimitAfter}`) for pipeline and worker tests.

## Tests

- Golden parsing of recorded streams (`testdata/codex-*.jsonl`, `testdata/claude-*.jsonl`) → progress/usage/rate-limit events.
- Command-line construction tables per TaskSpec variant (read-only vs rw, effort, schema).
- `make smoke-codex` / `make smoke-claude`: trivial task (write a result JSON) on a real machine; not in CI.

## Decisions taken in AGT-S1-T03

- **Nothing was run against a real codex.** The owner decided (2026-10-19) that codex isn't run while the adapter is written, not even `--help`. So every part of codex's interface the adapter relies on is a constant in `internal/agent/harness/codex/flags.go`, each marked `UNVERIFIED (owner to verify with make smoke-codex)`, to be fixed there in one place. The unverified list:
  - the minimum version `0.72.0` and `codex --version`'s `codex-cli x.y.z`;
  - `codex login status`, its exit status, and its `ChatGPT` / `API key` wording;
  - `codex login --device-auth` (only quoted in errors);
  - `auth.json`, `config.toml`, `AGENTS.md` and `skills/<name>/SKILL.md` in `CODEX_HOME`, and codex writing a refreshed token through the `auth.json` symlink (open and truncate) rather than renaming a new file over it;
  - `exec`, `--json`, `--model`, `-c key=<TOML>`, `--sandbox read-only|workspace-write`, `--cd`, `--skip-git-repo-check`, `--output-schema`, `--output-last-message`, the prompt as `-` on stdin, and `exec [flags] resume <thread id> -` with the exec flags before `resume`;
  - `-c approval_policy="never"`, which replaces the LLD's `--ask-for-approval never`: as far as known that is a top-level flag `exec` doesn't take;
  - `-c model_reasoning_effort=…` and its values: `minimal`, `low`, `medium`, `high`, and `xhigh` for the `*-codex-max*` models, which `max` maps to there; `minimal` maps to `low` on the `*-codex*` models;
  - the `config.toml` keys: `check_for_update_on_startup`, `[sandbox_workspace_write] network_access` and `writable_roots`, `[shell_environment_policy] inherit` and `ignore_default_excludes`, and `[mcp_servers.<name>] command`, `args` and `env_vars`;
  - the JSONL event and item names below, `turn.completed`'s usage being per turn rather than cumulative, and the rate-limit wording;
  - `useOutputSchema` (true), i.e. that codex accepts the Result schemas for `--output-schema`.
- **Package and registration.** `internal/agent/harness/codex`, registered with one line in `cmd/factory-agent/worker.go`'s `harnessAdapters`. `probeOptions` now probes each configured harness with its registered adapter's `Probe`, falling back to `probe.Configured` for one without an adapter. Every codex invocation goes through `Options.Binary`, and `Probe`'s short commands also go through `Options.Commands`, so tests run a fake codex.
- **Probe.** `codex --version` must be at least `MinVersion`. The login is `subscription` or `apiKey`, from `codex login status`'s wording. That runs with only `PATH`, `HOME`, `LANG`, `TERM` and `CODEX_HOME=<cfg.home>`, so an `OPENAI_API_KEY` in the owner's environment can't change the answer. The probe also requires `<cfg.home>/auth.json` to exist (`os.Stat` only), because `Prepare` links to it, so a keyring-only login is refused. The login file is never opened, and the command's output is neither logged nor repeated in errors. Models and the default come from the owner's config. Efforts are the configured ones, or all five when none is set. `costUSD` stays null: the stream carries no price.
- **Prepare.** The scratch `CODEX_HOME` is `<workspace>/home/.codex`, inside the pipeline's scratch `HOME`, and holds:
  - `auth.json`, a symlink to `<cfg.home>/auth.json`. Removing the workspace removes the link, not the login;
  - a generated `config.toml`: no update check; `workspace-write` with the network on and `out/`, `tmp/` and `home/.cache` writable besides the cwd; commands given the whole run environment (codex's default drops `*TOKEN*` names, which would drop `FACTORY_TOKEN` and `GH_TOKEN`); and `[mcp_servers.factory]`, with `command` the workspace's `bin/factory` and `args = ["mcp"]`. The orchestrator's brief names that `factory-agent fx mcp`; effector.md and worker.md say `factory-agent mcp`, so AGT-S1-T05 settles it (it is `clirun.EffectorArgs` and `clirun.FactoryCLI` since AGT-S1-T04, shared by both adapters). The MCP server's variables go through `env_vars` **by name**: the names of every variable in the run environment, never their values, so no token is written to the file;
  - `AGENTS.md`, holding the headless preamble;
  - `skills/<name>/`, copied from the pipeline's plugin layout (the skill folder only). When two plugins have a skill of the same name, both get the folder name `<plugin>-<name>`. Symlinks are skipped, and scripts keep their executable bit. The plugin's root files (`bin/`, `rules/`, …) stay in the pipeline's `skills/<plugin>/`, inside the workspace codex's sandbox reads: its `bin/` is on `PATH` (the pipeline puts it there), and a plugin CLI finds the rest from its own location (`ccfr` resolves `bin/../lib` and the plugin root from its real path).

  The LLD's fallback, a repo-local `.agents/skills/`, isn't used: it would put files in the checkout.
- **Run.** The invocation is `codex exec --json --model <m> -c model_reasoning_effort="<e>" -c approval_policy="never" --sandbox <mode> --cd <workdir> --skip-git-repo-check --output-schema <ws.Schema> --output-last-message <CODEX_HOME>/factory-last-message.json -`, with the prompt on stdin:
  - The model is the resolved one, else the config's default; with neither, there is no `--model`.
  - `--skip-git-repo-check` is there because a task without a checkout works in the workspace root.
  - The sandbox is `workspace-write` when the primary checkout is writable, else `read-only`.
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
- **Result.** `$FACTORY_RESULT` still wins (that is the pipeline's rule). `Raw.Result` is the `--output-last-message` file, else the stream's last agent message, with any Markdown code fence around it stripped. In a `read-only` sandbox the agent can't write `$FACTORY_RESULT`, so the final message, constrained by `--output-schema`, is the Result.
- **Repair** resumes the session: `codex exec … resume <thread_id> -` with the validation problems, asking for the corrected Result only, both to `$FACTORY_RESULT` and as the final message. The thread id and the run's model, effort, sandbox and workdir are kept in `CODEX_HOME/factory-session.json`, and the prompt in `factory-prompt.md`, so the Adapter keeps no per-run state and one Adapter serves concurrent runs. Without a thread id, a new exec gets the original prompt followed by the problems.
- **LLD gaps and risks for the owner's smoke run.**
  1. **`maxTurns` and Codex turns.** `maxTurns` counts codex's own turns, and `exec` and each `resume` is one turn. So for Codex `maxTurns` barely bites and `maxWallClock` is the real bound. Counting tool calls as turns instead is a one-line change in `stream.go`.
  2. **`--output-schema` is strict.** OpenAI's structured outputs take a strict subset of JSON Schema, with no remote `$ref` and every property required, and the Result and spec schemas may not fit it. If codex refuses them, set `useOutputSchema` to false: the Result still comes from `$FACTORY_RESULT` or the final message.
  3. **`read-only` blocks commands.** It denies commands both the network and writes, so `gh`/`ccfr` reads in review tasks fail. The Effector works only through MCP, whose server codex doesn't sandbox. An alternative is `workspace-write` with `--cd out/` for read-only checkouts.
  4. **`.git` may be read-only.** codex's `workspace-write` is known to keep `.git` read-only inside writable roots, which would stop commits in `repo/`. The smoke task doesn't commit, so verify this on an `implement` run.
  5. **Usage may be cumulative.** If `turn.completed.usage` is cumulative per thread, a resumed repair double-counts it.
- **`make smoke-codex`** runs `TestSmokeCodex` (`FACTORY_SMOKE_CODEX=1`, refused when `CI` is set). It probes the real codex, then runs the pipeline end to end against an in-process dispatcher and git server on a trivial task: `ls`, then write a fixed Result. It logs the exec arguments, stderr and progress so the flags above can be checked. Settings: `FACTORY_SMOKE_CODEX_HOME` (default `~/.factory-agent/codex-home`), `_MODEL` (`gpt-5-codex`), `_EFFORT` (`low`), `_BIN` (`codex`). It is skipped everywhere else, `make test` included.
- **Tests** never run codex. The test binary is the fake codex, behind a wrapper script: it replays a recorded JSONL file, writes `$FACTORY_RESULT` or the last-message file, records its arguments, environment, stdin and `CODEX_HOME` listing, and can start a child that ignores SIGTERM. The tests cover:
  - golden parsing of `testdata/codex-*.jsonl` (success, multi-turn, a usage limit with its reset, a 429, `resets_in_seconds`, a failed turn with retries, the older `token_count`), with `-update`, plus the semantics behind the goldens;
  - reset parsing;
  - flag assembly for every effort and model family, read-only and read-write, resume;
  - Prepare's layout: the symlink, the flattened skills with a name clash, `config.toml` with no token in it, and the owner's login surviving the workspace's removal;
  - Probe: subscription, API key, too old, not signed in, failing login status, the clean environment;
  - Run's failures, and stderr-only limits;
  - Repair by resume and without a thread;
  - cancel killing a process tree that ignores SIGTERM, and a rate limit stopping a blocked run;
  - the pipeline end to end with the repair turn and a throttled run.

## Decisions taken in AGT-S1-T04

- **Nothing was run against a real claude.** As for codex, the owner decided (2026-10-19) that claude isn't run while the adapter is written, not even `--help`, and that no file of a claude login (`~/.claude`, `~/.claude.json`, a config dir's credentials, the Keychain) is read. Every part of claude's interface the adapter relies on is a constant in `internal/agent/harness/claudecode/flags.go` (plus the hook's input type in `hook.go` and the reset formats in `ratelimit.go`), each marked `UNVERIFIED (owner to verify with make smoke-claude)`. The unverified list:
  - the minimum version `2.1.0` and `claude --version`'s `x.y.z (Claude Code)`;
  - `CLAUDE_CONFIG_DIR`; `ANTHROPIC_API_KEY` taking precedence over a subscription login; `CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token`); the login in `<configDir>/.credentials.json` outside macOS and in the Keychain on macOS, with `<configDir>/.claude.json` present once the dir is set up; the login hint (`CLAUDE_CONFIG_DIR=<dir> claude`, then `/login`);
  - `DISABLE_AUTOUPDATER=1` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`;
  - `--print` reading the prompt from stdin, `--output-format stream-json` with `--verbose`, `--model`, `--append-system-prompt`, `--plugin-dir` (once per plugin), `--settings <file>`, `--setting-sources user`, `--mcp-config <file>` with `--strict-mcp-config`, `--json-schema` taking the schema **inline** (not a path), `--max-turns`, `--max-budget-usd`, `--permission-mode bypassPermissions`, `--disallowedTools` as one comma-separated list, `--add-dir`, `--resume <session id>` from the same working directory;
  - `useJSONSchema` (true), i.e. that claude accepts the Result schemas (local `$defs`/`$ref`, `oneOf`, `additionalProperties: false`);
  - the settings keys `enabledPlugins`, `effortLevel` (the LLD's example) and `hooks.PreToolUse[].{matcher, hooks[].{type: command, command, timeout}}`, the matcher being a regular expression over tool names;
  - the OS sandbox: `sandbox.{enabled, allowUnsandboxedCommands, autoAllowBashIfSandboxed, excludedCommands, enableWeakerNestedSandbox, network.{allowUnixSockets, allowLocalBinding}}`; the sandbox taking its writable paths from `permissions.allow` `Edit(//<abs>/**)` rules, its unreadable paths from `permissions.deny` `Read(//<abs>)`/`Read(//<abs>/**)` rules and its network allow-list from `WebFetch(domain:<host>)` rules (`*.` wildcards included), with deny winning; `//` marking an absolute path; the sandbox's tools (`sandbox-exec` on macOS, `bwrap` and `socat` on Linux) and its platforms (none but those two); `api.anthropic.com` being all the harness needs from inside it; a `WebFetch(domain:localhost)` rule (the pipeline adds the local stack's host to `Workspace.Hosts` when a stack is up, AGT-S1-T06) being enough for sandboxed commands to reach `localhost:8000` and `localhost:8080`, with no port setting and with `allowLocalBinding: false` (which is about listening, not connecting): localhost access is to be confirmed by `make smoke-claude` with a stack up;
  - the hook protocol: `{tool_name, tool_input, cwd}` on stdin, exit 0 allows, exit 2 blocks and hands stderr to the model; the hook runs under `bypassPermissions` too;
  - the tool names `Bash`, `Write`, `Edit`, `MultiEdit`, `NotebookEdit`, `Read`, `Grep`, `Glob`, `WebSearch`, `WebFetch`, `Task`, `Skill`, `TodoWrite`, `StructuredOutput` and `mcp__<server>__<tool>`, and their input fields (`command`, `file_path`, `notebook_path`, `pattern`, `query`, `url`, `description`, `skill`, `todos[].status`);
  - the MCP config shape `{"mcpServers": {<name>: {type: stdio, command, args, env}}}` with `${NAME}` expanded from claude's environment;
  - the effort mapping `minimal→low, low→low, medium→medium, high→high, max→high`;
  - the stream-json events and fields below, and the rate-limit wording and reset formats.
- **Shared code.** What both adapters need moved out of `codex` into two new packages, and codex now uses them with no change in behaviour: `internal/agent/harness/clirun` (the process-group run and cancel, line splitting, the bounded transcript and stderr tail, reset-time parsing, version comparison, text helpers, the repair prompt, the Effector's MCP server name, arguments and `factory` CLI path, `EnvNames`) and `internal/agent/harness/harnesstest`, imported by tests only (the recording `EventSink`, and the end-to-end rig: git http-backend server, dispatcher, pipeline, job, transcript). `clirun.ParseAt` also takes `3pm`, `3:05pm` and a date without a year now.
- **Package and registration.** `internal/agent/harness/claudecode`, registered with one line in `harnessAdapters`. Every config name now has an adapter, so `TestWorkerCommandRefusals` unregisters claude-code for its "no harness adapter" case (the config refuses any other name), and `newRunner`'s own test uses `aider`.
- **Probe.** Claude Code's OS sandbox must be supported (below), else the harness isn't offered and the worker takes no claude-code task. `claude --version` must be at least `MinVersion`; it runs with only `PATH`, `HOME`, `LANG`, `TERM` and `CLAUDE_CONFIG_DIR`. The config dir must be an existing absolute directory. The auth mode comes from variable presence and `os.Stat` only, never a file's contents: `apiKey` when `ANTHROPIC_API_KEY` is set in the worker's environment; else `subscription` when `CLAUDE_CODE_OAUTH_TOKEN` is set, or `<configDir>/.credentials.json` exists, or (macOS only, where the login is in the Keychain) `<configDir>/.claude.json` exists; else "not signed in" with the login hint. `claude auth status` and a one-turn dry call (the LLD's options) aren't used: the first prints the account, the second spends a turn at every start. On macOS this means a set-up but signed-out config dir passes Probe and fails at its first run. Models and default are the owner's config; efforts are the configured ones or all five; `costUSD` counts only for API keys.
- **The config dir is the owner's, not a scratch copy.** Unlike codex's scratch `CODEX_HOME`, `CLAUDE_CONFIG_DIR` is the dedicated `cfg.configDir` itself: on macOS the login is a Keychain entry keyed by that path, so it can't be linked into a scratch dir. Isolation comes from the command line instead: `--setting-sources user` (never the target repo's `.claude/settings*.json`), `--settings` with `enabledPlugins: {}`, `--strict-mcp-config`, and skills only through `--plugin-dir`. claude keeps its sessions (the transcripts `--resume` needs) in the dedicated dir, so they accumulate there.
- **Prepare** writes `<workspace>/home/.factory-claude/settings.json` (`enabledPlugins: {}`, the mapped `effortLevel`, the PreToolUse hook) and `mcp.json` (`factory` = the workspace's `bin/factory` with `["mcp"]`, as codex, and every run variable passed as `${NAME}`, so no value reaches the file). The plugins are the pipeline's `skills/<plugin>` directories as they are (the plugin's whole tree with its selected skills, so `<SKILL.md dir>/../../bin` is the plugin's `bin/`, also on `PATH`). The pipeline leaves the plugins' Claude-only components out ([run-pipeline.md](run-pipeline.md) §Skills): `hooks/`, `hooks.json`, `commands/`, `agents/`, `.mcp.json`, `.lsp.json`, `output-styles/`, `monitors/`, `workflows/`, `themes/` and `settings.json`, and reduces each `plugin.json` to its identity fields (no component key, no `skills` scan paths), so `--plugin-dir` loads skills only, and the run's only hook and MCP server are the adapter's (the PreToolUse hook and the Effector); they are in the workspace, which no deny rule covers, so the sandbox reads them. Prepare checks each has `.claude-plugin/plugin.json` and refuses a skill outside a plugin.
- **The PreToolUse hook** is `factory-agent fx claude-hook pre-tool-use --write <dir>…` (`claudecode.HookMain`, dispatched from `fx.go`), run through the workspace's `factory` shim. Its policy is on its command line, not in a file. It blocks, with exit 2 and the reason on stderr:
  - a `Write`, `Edit`, `MultiEdit` or `NotebookEdit` outside `out/`, `tmp/` and the checkouts the harness may write (relative paths against the call's `cwd`, symlinks resolved, `..` cleaned). The adapter's own files in `home/` are outside, so the model can't rewrite its hook;
  - `git push` (with global options, a path, inside `bash -c "…"`), `gh pr|issue <write>`, and `gh api` with a non-GET `-X/--method`, or with fields and no method (gh then POSTs), or a GraphQL `mutation`;
  - unreadable input or arguments (fail closed).

  **Change from the LLD:** the Bash rules apply in every workspace, not only read-only ones: the headless preamble and effector.md send every GitHub change through `factory` (`factory push`). And they block gh *writes* only: the LLD's `gh (pr|issue|api)` would also block the `gh pr view`/`gh api` reads review tasks need. The hook guards against mistakes and injected instructions; it can't see what a Bash command writes, which the OS sandbox below bounds, and the token scopes stay the boundary.
- **The OS sandbox is always on** (owner decision, 2026-10-19). `bypassPermissions` without it would let prompt-injected Bash read the maintainer's machine (other credentials, `~/.ssh`, the gh login), outside [07](../../../07-security-and-governance.md)'s blast-radius model; codex runs inside its `--sandbox`, and claude must too. The generated settings carry:
  - `sandbox`: `enabled: true`, `allowUnsandboxedCommands: false` (no `dangerouslyDisableSandbox` escape), `excludedCommands: []`, `enableWeakerNestedSandbox: false`, `autoAllowBashIfSandboxed: true`, `network.allowUnixSockets` = the git credential helper's socket only, `network.allowLocalBinding: false`;
  - `permissions.allow`: `Edit` on the writable roots only (the rw checkouts, `out/`, `tmp/`, the scratch `HOME`; a read-only workspace gets no checkout), and `WebFetch(domain:…)` for `Workspace.Hosts` (the git host and the dispatcher's, a new Workspace field the pipeline fills), `api.github.com` and `*.githubusercontent.com` with `github.com` (a GHE host serves its API itself), and `api.anthropic.com`;
  - `permissions.deny`: `Edit` on the adapter's scratch dir (its settings and hook), and `Read` on the dedicated config dir and on the worker's `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.azure`, `~/.config/{gcloud,gh,factory-agent}`, `~/.netrc`, `~/.git-credentials`, `~/.docker`, `~/.kube`, `~/.npmrc`, `~/.pypirc`, `~/.claude`, `~/.claude.json`, `~/.codex`, `~/.factory-agent/{codex-home,claude}` and `~/Library/Keychains`, leaving out any that holds the workspace.

  Where the sandbox can't run (a platform other than macOS and Linux, or Linux without `bwrap` and `socat`), Probe drops the harness and Prepare refuses the run: there is no unsandboxed fallback. The PreToolUse hook stays, as a second layer under the sandbox (and the sandbox one under the scoped tokens).
- **Read-only workspace** (no writable primary checkout): `--disallowedTools Write,Edit,MultiEdit,NotebookEdit` (the LLD's list plus `MultiEdit`) and the hook. The permission mode is `bypassPermissions` in both cases: nobody can answer a prompt, and `acceptEdits` would still deny every Bash command. The Result then comes from the structured output (or a Bash write to `$FACTORY_RESULT`, which the disk allows).
- **Run.** `claude --print --output-format stream-json --verbose [--model m] [--resume id] --append-system-prompt <preamble> --plugin-dir skills/<p>… --settings … --setting-sources user --mcp-config … --strict-mcp-config --json-schema <schema JSON> [--max-turns n] [--max-budget-usd x] --permission-mode bypassPermissions [--disallowedTools …] [--add-dir out/ and the other checkouts]`, prompt on stdin, in the workspace's working directory:
  - The model is the resolved one, else the config's default; with neither, claude's default. The auth is the resolved one, else Probe's rule.
  - `--max-turns` is `limits.maxTurns`; `--max-budget-usd` is `limits.maxCostUSD` on API keys only.
  - The environment is the pipeline's plus `CLAUDE_CONFIG_DIR`, the two variables above, `USER` (claude looks its Keychain entry up by it) and exactly one credential variable from the worker's environment: `ANTHROPIC_API_KEY` on an API-key run (the run fails if it isn't set), or `CLAUDE_CODE_OAUTH_TOKEN` on a subscription run when the owner set one. A subscription run never gets `ANTHROPIC_API_KEY`, so a key in the owner's shell can't take it over.
  - The process group is handled as for codex (`clirun.Run`); a plugin error in `system/init` stops the run at once.
  - The transcript is the stdout lines, plus `factory.claude-exec` (the args), `factory.claude-stderr` and `factory.claude-transcript-truncated`.
- **Stream mapping.**
  - `system/init` records the `session_id`. A non-empty `plugin_errors` is fatal (the LLD), and an MCP server that isn't `connected` is an `error` event only (the Effector's `mcp` op is AGT-S1-T05's).
  - Each new top-level `assistant` message id is one `turn` (subagents' messages, with a `parent_tool_use_id`, aren't). Its text is a `message` event; each `tool_use` (subagents' too) is one `tool` event (`$ cmd`, `edit <path>`, `read`, `search`, `fetch`, `agent`, `skill`, `mcp server.tool`, `result`); `TodoWrite` is a `plan: n/m done` message; thinking goes to the transcript only.
  - Usage is per message, with `input_tokens` plus both cache counts as input (as codex counts cached input), and a message's usage counted once although its events repeat it; the `result`'s totals top up anything the stream missed. `total_cost_usd` is the cost on API-key runs only.
  - The `result` ends the run: `error_max_turns` and `error_max_budget_usd` are `LimitError`s (`maxTurns`, `maxCostUSD`); `error_max_structured_output_retries` is an error event only (the pipeline then checks `$FACTORY_RESULT` or the text, and repairs); any other error subtype, or `is_error`, fails the run. `permission_denials` are an error event.
- **Rate and plan limits** report `RateLimited` once, from the first of: a `rate_limit_event` with status `rejected` (its `resetsAt`; `allowed_warning` is a message); an `assistant` message with `error: "rate_limit"`; an error `result`, or the last stderr line after a failed exit, with `usage limit`, `rate limit`, `rate_limit`, `too many requests`, `429`, `quota`, `limit reached` or `hit your limit`. The reset is the Unix time in `limit reached|<seconds>`, else `resets 3pm`/`resets Oct 21, 9am` with an optional `(IANA zone)`, else what `clirun.ResetTime` finds, else now + 15 min.
- **Result.** `$FACTORY_RESULT` still wins (the pipeline's rule). `Raw.Result` is the `result`'s `structured_output`, else the `StructuredOutput` tool call's input, else the final text without a code fence. The adapter doesn't write `out/result.json` itself (the LLD's arrow), so a file the agent wrote isn't overwritten.
- **Repair** resumes the session, `--resume <session_id>` with every other flag again and `--max-turns 5`, asking for the corrected Result only. The session id and the run's model, auth, workdir, read-only flag and budget are kept in `factory-session.json` and the prompt in `factory-prompt.md` in the scratch dir, so one Adapter serves concurrent runs. Without a session id, a new run gets the original prompt followed by the problems.
- **LLD gaps and risks for the owner's smoke run.**
  1. **The Keychain with a scratch `HOME`.** The run's `HOME` is the workspace's. If claude (or macOS's `security`) finds the login Keychain through `$HOME`, a subscription run won't find its login. The fallback is `claude setup-token` and `CLAUDE_CODE_OAUTH_TOKEN` in the worker's environment, which the adapter already passes.
  2. **`--json-schema` and the Result schema.** If claude refuses the generated schema, or the model can't satisfy `task`/`question` being required, set `useJSONSchema` to false.
  3. **`bypassPermissions`** may be refused (as root, or by a managed setting); `--disallowedTools` and the hook must still hold under it, which the smoke's `git push --dry-run` checks.
  9. **The sandbox keys and rules.** Check in the transcript and claude's docs that every `sandbox` key exists, that the rules' `//` absolute form and the `WebFetch(domain:…)` allow-list (with `*.` wildcards) are what the sandbox reads, and that a write outside the roots and a read of a denied path fail. Check that the allow-list doesn't break git over HTTPS (clone, fetch, the credential helper's Unix socket), gh, the `factory` CLI to the dispatcher (a loopback dispatcher in development), or the Effector. Tasks that download dependencies (Go modules, npm) need their registries added to `hostDomains`/`harnessDomains`.
  4. **The effort key.** If `effortLevel` isn't a setting, the effort is silently ignored; the transcript's `system/init` won't show it, so check the model's behaviour or the docs.
  5. **`--setting-sources user`** may not keep the target repo's `.claude/skills`, agents or commands out, and repo `CLAUDE.md` files load as project memory (codex reads `AGENTS.md` the same way).
  6. **The Effector over MCP fails today**: `factory-agent fx mcp` is AGT-S1-T05's, so `system/init` reports the server `failed` (an error event, not a failure).
  7. **An `ANTHROPIC_API_KEY` in the owner's environment switches the agent to API-key auth** (Probe reports `apiKey`); in the PoC the worker must run without it.
  8. **Cost on resume.** If `total_cost_usd` is per session rather than per process, a resumed repair double-counts it.
- **`make smoke-claude`** runs `TestSmokeClaude` (`FACTORY_SMOKE_CLAUDE=1`; the target and the test both refuse when `CI` is set). It probes the real claude, then runs the pipeline end to end on a trivial task: `ls`, a `git push --dry-run` the hook must refuse, then a fixed Result. It logs the exec arguments, stderr, `system/init`, the `result` and the progress, and says whether the hook refused the push. Settings: `FACTORY_SMOKE_CLAUDE_CONFIG_DIR` (default `~/.factory-agent/claude`), `_MODEL` (`claude-sonnet-5-5`), `_EFFORT` (`low`), `_BIN` (`claude`).
- **Tests** never run claude. The test binary is the fake claude behind a wrapper script: it replays a recorded stream-json file, writes `$FACTORY_RESULT`, records its arguments, environment, stdin, settings and MCP config, puts scripted tool calls to the settings' PreToolUse hooks through `sh -c` (the real `fx claude-hook` path), and can start a child that ignores SIGTERM. The tests cover:
  - golden parsing of `testdata/claude-*.jsonl` (success with structured output, multi-turn with a subagent, a todo list, a failed MCP server and a denial, a usage limit with its `|<reset>`, `resets 3pm (Europe/Lisbon)`, an API 429, a `rate_limit_event`, an error result, max turns, a plugin error), with `-update`, plus the semantics behind the goldens;
  - reset parsing; flag assembly for read-only and read-write, resume, budget; the effort mapping;
  - Prepare's layout (settings with the sandbox on, its writable roots per workspace, network allow-list and denied reads; hook command; MCP config with no token; the owner's config dir untouched) and refusals, the missing sandbox among them;
  - sandbox support per platform and tools, and Probe dropping the harness without it;
  - Probe: every auth source, macOS, too old, not signed in, the clean environment;
  - the hook's allow and block table, and `fx claude-hook` from `factory-agent`;
  - Run's arguments, environment, recorded sandbox settings and hook answers per auth and workspace, its failures (limits on the stream and on stderr, an error result, max turns, a plugin error stopping a blocked run, an API-key run without the key);
  - Repair by resume and without a session;
  - cancel killing a process tree that ignores SIGTERM, and a rate limit stopping a blocked run;
  - the pipeline end to end with the hook, the repair turn and a throttled run.


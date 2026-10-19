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
  - a generated `config.toml`: no update check; `workspace-write` with the network on and `out/`, `tmp/` and `home/.cache` writable besides the cwd; commands given the whole run environment (codex's default drops `*TOKEN*` names, which would drop `FACTORY_TOKEN` and `GH_TOKEN`); and `[mcp_servers.factory]`, with `command` the workspace's `bin/factory` and `args = ["mcp"]`. The orchestrator's brief names that `factory-agent fx mcp`; effector.md and worker.md say `factory-agent mcp`, so AGT-S1-T05 settles it (it is `mcpArgs` and the command in `prepare.go`). The MCP server's variables go through `env_vars` **by name**: the names of every variable in the run environment, never their values, so no token is written to the file;
  - `AGENTS.md`, holding the headless preamble;
  - `skills/<name>/`, copied from the pipeline's plugin layout. When two plugins have a skill of the same name, both get the folder name `<plugin>-<name>`. Symlinks are skipped, and scripts keep their executable bit.

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

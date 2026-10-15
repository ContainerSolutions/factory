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

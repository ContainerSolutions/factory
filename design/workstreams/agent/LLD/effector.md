# LLD — Effector CLI and MCP server (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Task: AGT-S1-T05 · Package: `internal/agent/effector`
> Ops: [contracts.md §Effect ops](../../foundation/LLD/contracts.md#effect-ops) · Server side: [api-server.md §Effector API](../../dispatcher-runtime/LLD/api-server.md#effector-api-post-tasksideffects)

## Shape

One Go implementation, two front-ends:

```go
type Effector struct { API apiclient.Client; TaskID string; Allow []string; LabelGlobs []string; Git GitOps; seq atomic.Int64 }
func (e *Effector) Do(ctx context.Context, op string, args map[string]any) (Result, error)
```

- **CLI** — `factory <op> [flags]` (the `factory` name is a symlink to `factory-agent fx` placed on the run's `PATH`).
- **MCP** — `factory-agent mcp` (stdio, `modelcontextprotocol/go-sdk`), one tool per op with a JSON Schema for its args; tool list generated from the same op table, so CLI and MCP never drift.

## Ops

| CLI | Args | Side |
|---|---|---|
| `factory comment --body-file f` | target defaults to the task's primary object | API |
| `factory pr comment --pr N --body-file f` | | API |
| `factory label add\|remove --name L [--target ref]` | | API |
| `factory issue create --title T --body-file f [--labels …]` | returns ref | API |
| `factory issue sub-add --parent N --child N` | | API |
| `factory issue close [--target ref] [--reason]` | | API |
| `factory pr open --head B --base B --title T --body-file f` | returns PR ref | API |
| `factory pr update --pr N [--title] [--body-file]` | | API |
| `factory pr retarget --pr N --base B` | | API |
| `factory pr merge --pr N` | | API |
| `factory review --pr N --event approve\|request-changes\|comment --body-file f [--comments-file c.json]` | inline comments `[{path, line, body}]` | API |
| `factory thread reply --thread ID --body-file f` / `factory thread resolve --thread ID` | | API |
| `factory push [--branch B]` | `git push` with the scoped token; records `push-branch` | git |
| `factory stack push` / `factory stack rebase` | `gh stack push/rebase` (or git fallback), records op | git |
| `factory result --file out/result.json` | validates locally, copies to `$FACTORY_RESULT` | local |
| `factory git-credential get` | credential helper protocol (internal) | local |

## Behaviour

1. **Local pre-check** (fast feedback, not security): op ∈ `Allow`, labels match globs → else exit 3 with a clear message (the server re-checks; it's authoritative).
2. **Idempotency key** = `<taskID>/<op>/<seq>`. `seq` increments per CLI/MCP call within the run and is persisted in `out/.effector-seq`, so keys stay unique across adapter restarts. On **network errors** the CLI retries the same call with the **same** key (up to 3 times), so the server applies it once. A *new* call is always a new effect, so skills must check before repeating an effect (e.g. `factory pr open` first looks up an open PR for the head branch server-side, and comments carry a skill-chosen `--key` when they must be unique per logical step: `factory comment --key plan-summary` → key `<taskID>/comment/plan-summary`).
3. API ops → `POST /tasks/{id}/effects`; print `{"effectId", "url"}` as JSON on stdout; non-2xx → exit 4 with problem detail.
4. Git ops run in `repo/` using the credential helper; after success they `POST` a record (`op`, `ref`, `sha`).
5. `lisa` compatibility: when `FACTORY_API` is set, `lib/lisa` subcommands that write to GitHub delegate to `factory …` (SKL-S1-T02 implements the delegation on the lisa side).

## Exit codes

`0` ok · `2` usage · `3` not allowed (local) · `4` server rejected / failed · `5` git failure.

## Tests

- Each op against `factory-mock` (request body golden).
- MCP: tool list equals CLI op table; a call round-trips through the same `Do`.
- Credential helper: answers only for the task's repos; nothing for others (public reads anonymous).
- Retry with same key on simulated network error.

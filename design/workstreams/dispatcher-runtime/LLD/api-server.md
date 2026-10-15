# LLD — HTTP API server and `factory serve` (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Tasks: RT-S2-T03, RT-S2-T04 · Packages: `internal/server`, `internal/auth`, `cmd/factory`
> Contract: [contracts.md §HTTP API](../../foundation/LLD/contracts.md#http-api)

## Server

- Std `net/http` + `ServeMux`; handlers implement the oapi-codegen **strict** server interface (`internal/apiserver/gen`).
- Middleware chain: request ID → structured access log (no bodies) → recover → auth (per route group) → body limit (1 MB; transcripts 20 MB) → handler.
- Routes outside the contract: `/hooks/{provider}` (provider `Handler()`), `/` (embedded UI via `internal/webui.Handler()`), `/healthz`, `/readyz` (catalog loaded + rebuild done), `/metrics` (Prometheus text).

## Auth

| Caller | Mechanism | Checks |
|---|---|---|
| Agent | `Authorization: Bearer <agentToken>` | token hash lookup → Agent; lease/task routes require the lease to belong to the agent |
| UI | GitHub OAuth web flow → server-side session cookie (`HttpOnly`, `Secure`, `SameSite=Lax`, 12 h) | `GET /user`; **commands** require membership of the catalog Group `maintainers` (resolved via `Conn.ResolveGroup`, cached 10 min — the CCF org admins in the PoC); any org member may read |
| CLI | `Authorization: Bearer <cliToken>` (static token from env, PoC) | full admin; for `factory dispatch` |
| Webhooks | provider HMAC (in `Conn.Handler()`) | — |

OAuth app config: `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET` (a GitHub OAuth App, or the author GitHub App's user-to-server flow). CSRF: `state` param bound to a short-lived cookie.

## Effector API (`POST /tasks/{id}/effects`)

```text
1. auth: agent owns the active lease of task {id}
2. op ∈ task.spec.effects.allow                         else 403 effect-not-allowed
3. label ops: every label matches task.spec.effects.labels globs   else 403
4. identity := TaskSpec.identity; target must be the task target repo (or its linked objects) else 403
5. req.idempotencyKey must be present and non-empty (≤ 200 chars)  else 400 idempotency-key-required
   idem := hash(task.id, req.idempotencyKey); if Effect(idem) exists → return it (200, replay)
6. rate limits: ≤ 20 comments/run, ≤ 60 effects/run, and ≤ 30 comments per work item per rolling hour (all runs)   else 429
7. result := Conn.Apply(EffectRequest{op, target, args, identity, idem})
8. store Effect; append to Run.progress; broadcast SSE; return {effectId, url}
```

Git-side ops (`push-branch`, `stack.*`) are **recorded** (step 5–8 without `Conn.Apply`).

## Commands (`POST /commands`)

Body: `{type, target, args}`.

| `type` | `target` / `args` | Effect |
|---|---|---|
| `pause` / `resume` | `target: {scope: global\|provider\|queue\|workitem, name}` | sets kill-switch flags in the store (rails read them); emits `factory.command.pause/resume` |
| `cancel` | `target: {task}` | Task → `Cancelling` |
| `redispatch` | `target: {task}`, `args: {harness?}` | clones the Task with an optional harness override; new Task `Pending` |
| `dispatch` | `args: {event}` | validates + stores an injected Event (source must be a configured provider or `factory`), used by `factory dispatch` and the UI dry-run "run for real" |
| `resync-catalog` | — | triggers catalog-sync |

## SSE (`GET /stream?topics=…`)

- Hub with per-topic subscribers; topics: `runs/<id>`, `workitems`, `workitems/<id>`, `tasks`, `agents`.
- Events: `progress`, `workitem.updated`, `task.updated`, `agent.updated` (JSON); `id:` = store version for resume via `Last-Event-ID` (backfill from the run's progress ring buffer).
- Keep-alive comment every 15 s; max 100 subscribers (PoC).

## `factory serve`

```text
factory serve [--addr :8080] [--catalog-repo URL --catalog-ref main | --catalog-dir ./catalog]
              [--retention 2160h] [--public-url https://…] [--log-format json|text]
```

Environment (names only; values from the secret store — see [rollout](../../poc-rollout/LLD/rollout.md#secrets)):

| Variable | Use |
|---|---|
| `FACTORY_SECRET_<NAME>_<KEY>` | resolves a catalog `secretRef{name, key}` (e.g. `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY`) |
| `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET` | UI login |
| `FACTORY_SESSION_KEY` | cookie signing (32 bytes) |
| `FACTORY_CLI_TOKEN` | CLI admin token |
| `FACTORY_POOL_JOIN_TOKEN_<POOL>` | via `secretRef` of the Pool |

Wiring order is in [controllers.md §Ordering](controllers.md#ordering-and-startup). Graceful shutdown: stop accepting leases, wait ≤ 30 s for in-flight requests, cancel controllers.

`factory dispatch --event file.json [--url --token]` posts `{type: dispatch, event}` to `/commands`.

## Tests

- Contract tests: the generated client against `httptest` server for every operation (happy + 401/403/404/409).
- Effector: allow-list, label globs, foreign target, idempotent replay, rate limit.
- OAuth: state mismatch, non-member, member read-only, admin commands.
- SSE: resume with `Last-Event-ID`.
- Integration (RT-S2-T04): `serve` + fake GitHub (`httptest`) + fake agent completes a task end-to-end.

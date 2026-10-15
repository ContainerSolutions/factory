# LLD — Scheduler, registered pools and leases (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Task: RT-S2-T02 · Package: `internal/scheduler`
> Protocol overview: [10 §Agent lifecycle](../../../10-poc.md#agent-lifecycle-and-lease-protocol)

## Task phases

```text
Pending ──lease──▶ Leased ──first heartbeat──▶ Running ──complete──▶ Succeeded | Failed
   ▲   └─(deferred: budget / no-capable-agent / agent-throttled; reason on status)
   └──────── lease expired / abandon / retryable failure (attempts < queue.retry.max+1)
Running ──cancel──▶ Cancelling ──ack or lease expiry──▶ Cancelled
```

## Registration

`POST /api/v1/agents` body: `{pool, joinToken, ownerToken, capabilities, lending, agentVersion}`.

1. `joinToken` matches `Pool.admission.tokenRef` secret (constant-time compare).
2. `ownerToken` (a short-lived user token from the GitHub **device flow** that `factory-agent register` runs against the dispatcher's OAuth client) → `GET /user` → login; **discarded immediately** (never stored or logged). The worker's OS user needs no `gh` login.
3. Owner ∈ `Pool.admission.owners` Group (via `Conn.ResolveGroup`, cached 10 min).
4. Capabilities ⊇ `Pool.require` (e.g. at least one required harness).
5. Issue `agentToken` (random 32 bytes, stored hashed, TTL 24 h, renewed by heartbeats) and create/replace the `Agent` object (`agt_<owner>-<host>`).

## Lease matching

`GET /agents/{id}/lease?wait=30s` parks the request; the scheduler tries to match on every Task/Agent change and on a 1 s tick.

Per candidate `(task, agent)` the matcher checks, in order (first failing reason is recorded for the UI):

| # | Check | Reason if failed |
|---|---|---|
| 1 | Task's queue → pool == agent's pool | — (not a candidate) |
| 2 | queue concurrency: running tasks of the queue < `concurrency` | `queue-full` |
| 3 | budgets (queue, RuleSet, global): runs, tokens, USD (USD only counts `apiKey` runs) | `budget` |
| 4 | agent phase `Ready`, not `Throttled` (until passed), running < `lending.maxConcurrent` (and pool `maxTasksPerAgent`) | `agent-busy` / `agent-throttled` |
| 5 | **lending**: `unlimited` short-circuits; else queue ∈ `lending.queues`, task RuleSet ∈ `lending.ruleSets` (if set), harness ∈ `lending.harnesses` (if set), within `lending.hours`, `today.runs < maxRunsPerDay`, `today.tokens < maxTokensPerDay` | `lending` |
| 6 | `requires.labels` ⊆ agent labels | `no-capable-agent` |
| 7 | harness resolution `patterns.Resolve(TaskSpec.allowed, task.request, agent.offer)` succeeds | `no-capable-agent` |
| 8 | **anti-affinity**: if TaskSpec identity is `reviewer` and the PR's last author Run's agent == this agent, skip unless it's the only Ready agent in the pool for > 10 min (soft in PoC) | `anti-affinity` |
| 9 | `has-local-stack` tasks: agent has no other `has-local-stack` task running | `agent-busy` |

Selection among matching pairs:

- Tasks ordered by queue `priority` desc, then `CreatedAt` asc.
- Agents ordered by **fairness**: least-recently-leased first; agents with `lending.unlimited` are tried **last** when another agent can take the task (`Pool.fairness: prefer-limited`, default) or first (`prefer-unlimited`).

On match:

1. `task.spec.harness.resolved = …`; phase `Leased`; `lease{id, agent, expiresAt: now+ttl}`.
2. Mint the git credential through `Conn.MintGitCredential` (rw repo with TaskSpec permissions, read repos read-only, TTL = min(1h, maxWallClock+10m)).
3. Respond `200 {lease, task, gitCredential}` (credential only in this response; never stored).

## Heartbeats and expiry

- `POST /leases/{id}/heartbeat` every `pool.lease.heartbeat` (30 s); body carries progress events (appended to Run, broadcast on SSE) and token usage deltas (`today.tokens`).
- Response: `{expiresAt, cancel: bool, refreshedGitCredential?}` (credential refreshed when < 10 min left).
- No heartbeat for `ttl` (2 min) → lease expired → Task back to `Pending` (`attempts++`, reason `agent-lost`) if retry policy allows, else `Failed{agent-lost}`.

## Throttling

`PATCH /agents/{id} {phase: Throttled, until}` or `abandon{reason: throttled, until}` → agent `Throttled`; the Task returns to `Pending` **without** consuming an attempt.

## Budgets

Counters live in a dedicated runtime kind, **`BudgetCounter`**, one object per (scope, name, UTC day): name `budget.<queue|ruleset|global>.<name>.<YYYY-MM-DD>`, fields `runs`, `tokens`, `usd`. Queue and RuleSet stay pure config. Updates use store CAS (`Update` with `resourceVersion`, retry on `ErrConflict`): `runs` is incremented when a lease is granted; `tokens`/`usd` from heartbeat deltas and the final Result. The matcher reads today's counters for the task's queue, RuleSet and global scope. Counters are swept after 90 days like other runtime kinds. A Task deferred for budget is retried at the next UTC day boundary (a new counter object) or when the catalog budget changes.

## Tests

- Matching table: one test per row above (pass and fail).
- Fairness: two agents (one unlimited) → limited agent preferred.
- Lending: `maxRunsPerDay` reached → `lending`; hours outside window → `lending`.
- Lease expiry and retry attempts; throttled abandon doesn't consume attempts.
- Anti-affinity soft fallback after 10 min with fake clock.
- Credential is minted per lease and never persisted (store inspected).

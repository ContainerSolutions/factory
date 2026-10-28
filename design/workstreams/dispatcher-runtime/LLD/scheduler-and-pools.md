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

`POST /api/v1/agents` (`registerAgent`): one credential header, an enrollment token or the agent's refresh token ([P1 §6](../../../proposals/P1-agent-workspaces.md#6-enrollment-replaces-the-device-flow)); body `{pool, host, agentVersion, capabilities, lending}`; response `201 {agentId, agentToken, expiresAt, refreshToken}` ([contracts.md §HTTP API](../../foundation/LLD/contracts.md#http-api)). Two credentials are a `401`, and so are the join and owner token headers, which P1 phase 6 removed (with the device flow, the owner-token exchange and the `admission.owners` Group check: the admin who mints the enrollment vouches for its owner).

- `X-Factory-Enrollment-Token` (`enr_…`): the `Enrollment` record (internal kind, `enl_<ulid>`) is found by its token's SHA-256 (a label; the stored hash compared in constant time) and must be unused and unexpired; the request's `pool` and `host` must be its pool and name (`400`, and the token stays unspent). It is **spent** (CAS on the record, so two concurrent uses register once), the owner is the enrollment's (the admin vouched for it), and the response adds `refreshToken`. The agent's `AgentRefreshToken` record (named by the agent id, hashed, with the enrollment's pool, owner and name) is created or replaced.
- `X-Factory-Agent-Refresh-Token` (`arf_…`): the `AgentRefreshToken` found the same way, its pool and name matching the request; it is **rotated** (CAS: the presented token stops working, a concurrent second use is a `401`) and the response carries the next one.

Then:

1. Capabilities ⊇ `Pool.require` (e.g. at least one required harness).
2. Issue `agentToken` (random 32 bytes, stored hashed, TTL 24 h, renewed by heartbeats) and create/replace the `Agent` object (`agt_<owner>-<host>`, `host` from the body).

An admin's `deregisterAgent` deletes the `AgentRefreshToken` before deregistering; the agent's own does not, so a restart can still re-register.

`createEnrollment` takes a registered pool with `admission` (the pools enrollments may be minted for), an owner login (`^[A-Za-z0-9-]{1,39}$`, on the dispatcher's sign-in provider), a name (the host pattern) and a `ttl` (default 7 d, at most 30 d); `listEnrollments` never returns tokens or hashes; `deleteEnrollment` removes the record (`404` unknown).

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
| 8 | **anti-affinity**: if TaskSpec identity is `reviewer`, the PR's last author Run's agent == this agent and its harness == the harness this lease resolves (row 7), skip unless it's the only Ready agent in the pool for > `--anti-affinity-wait` (default 10 min; `0` turns the row off) | `anti-affinity` |

Selection among matching pairs:

- Tasks ordered by queue `priority` desc, then `CreatedAt` asc.
- Agents ordered by **fairness**: least-recently-leased first; agents with `lending.unlimited` are tried **last** when another agent can take the task (`Pool.fairness: prefer-limited`, default) or first (`prefer-unlimited`).

On match:

1. `task.spec.harness.resolved = …`; phase `Leased`; `lease{id, agent, expiresAt: now+ttl}`.
2. Mint the git credentials through `Conn.MintGitCredential` (TTL = min(1h, maxWallClock+10m)): the rw repo with TaskSpec permissions, then one read-only token per read repo unless the read repos are public (`publicReads`).
3. Respond `200 {lease, task, gitCredential, readCredentials}` (credentials only in this response; never stored).

**The `sched` lock** ([P3 §Locks and leases](../../../proposals/P3-dispatcher-state-in-object-storage.md#locks-and-leases)). A match is checked against a snapshot of many objects (the queue's concurrency, the agent's capacity, lending, the budgets), under the in-process `Scheduler.mu`; with several dispatchers, each up to a sync interval behind, two could lease two *different* tasks past a limit. So every commit, on every instance, re-checks and writes under one store lock, `sched` (one lock, not one per pool, since lending lets a queue's tasks run in other pools):

- **The credentials are minted before the lock** (minting is the slow call to GitHub). A credential minted for a candidate that then fails the re-check is left to expire (an hour at most).
- **The lock is taken with a `SchedWait` context (2 s).** A timeout is `errSchedBusy`, which **ends the pass** with no grant (the poll parks until the next wake or tick), and records "busy until now + 2 s": a pass that starts before then ends at once, with no wait and no mint. So a pass waits for the lock at most once and mints at most once, and the other polls, `Refresh` and `Explain` are held up by at most one 2 s wait at a time, even while a dead holder keeps the lock for its TTL.
- **The lock's TTL is 30 s (`SchedTTL`) and the section runs under a 15 s deadline**: taking the lock (which syncs), reloading the snapshot, checking the candidate again (`check`: it must still be pending, its agent still there, and every row still pass) and the commit's writes. A candidate that fails the re-check is skipped, as a task that changed under the snapshot is. A section that hits its deadline fails like a commit error: the task is unleased, or its lease expires.
- Heartbeats, completes and abandons take neither `Scheduler.mu` nor the lock. The scheduler's other in-process state (`waiting`, `heldSince`, `mintBackoff`) is per instance and only affects preference, never a limit.

**Leader-only work.** With `Options.Leader` (serve's leader lock), the lease controller's reconciles and `Refresh` run on the leader only; matching, heartbeats, completes and the tick's wake of parked polls run on every instance.

## Heartbeats and expiry

- `POST /leases/{id}/heartbeat` every `pool.lease.heartbeat` (30 s); body carries progress events (appended to Run, broadcast on SSE) and token usage deltas (`today.tokens`).
- Response: `{expiresAt, cancel: bool, refreshedGitCredential?, refreshedReadCredentials?}` (all of the lease's credentials refreshed together when the first to expire has < 10 min left).
- **Usage accounting.** Heartbeats carry **deltas** since the previous heartbeat (`inputTokens`, `outputTokens`, `costUSD`); the scheduler adds them to the `BudgetCounter`s and the agent's `today` as they arrive. `Result.harness.usage` is the **cumulative total** for the run. On `complete`, the scheduler adds `max(0, total − Σ deltas)` per field, so nothing is counted twice, and logs a warning when `Σ deltas > total`. On lease expiry or abandon, only the deltas already received count.
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
- Anti-affinity soft fallback after 10 min with fake clock; a different harness isn't held; wait 0 is off.
- Credential is minted per lease and never persisted (store inspected).
- The `sched` lock, on two object stores over one bucket (`sched_test.go`): two schedulers with `concurrency: 1`, each behind the other, lease exactly one task; a dead holder: a pass mints once, waits once and ends, passes within 2 s neither wait nor mint, and once its lease ends the task is leased once; `Refresh` runs on the leader only.

## Decisions taken in RT-S2-T02

- **Go API, no HTTP.** `scheduler.New(Deps{Store, Catalog, Conns, Runs, Secrets, Owners}, Options)` with `Run(ctx)` (the lease controller plus the 1 s tick). The API server (RT-S2-T03) calls:
  - `Register`, `Authenticate`, `UpdateAgent` (PATCH: Ready, Throttled{until}, Draining, Offline, lending) and `Deregister`;
  - `Lease(ctx, agent, wait)`, which returns a `*Grant{Lease, Task, GitCredential}` or nil when `wait` (capped at 60 s) runs out;
  - `Heartbeat`, `Complete` and `Abandon`, keyed by `(agent, lease)`. Someone else's lease is `ErrNotLeaseHolder`, and an ended one is `ErrLeaseGone`.

  Errors wrap `ErrUnauthorized`, `ErrForbidden`, `ErrInvalid` (`*ValidationError`), `ErrNotFound` and `ErrLeaseGone`, for 401, 403, 400, 404 and 409. `Explain(task, agent)` returns the first failing reason, for the UI and tests. **The API server must call `Scheduler.Complete`, not `RunAPI.Complete`**, because the usage top-up needs the lease's Σ deltas.
- **Owners** come from the enrollment (`Owner{Provider, Login}`, the provider the dispatcher's sign-in uses); the scheduler verifies no login and resolves no Group at registration (P1 phase 6 removed the `OwnerVerifier` and the owners Group cache).
- **Agent tokens.** The token is `<agentId>.<base64url(32 random bytes)>`. Only `sha256:<hex>` of the secret part is stored, in an internal `AgentToken` record named by the agent id. Registering again replaces the token.

  The 24 h TTL is renewed by **any authenticated call** (at most every 10 min), not only heartbeats, so an idle polling agent stays registered. The tick takes an agent whose token expired Offline.
- **Leases** are an internal `Lease` record (`lse_<hex>`, labels `factory.ccf.dev/task` and `/agent`). It holds the TTL, the resolved auth, the credential's expiry (never the credential) and Σ heartbeat deltas.

  The authoritative expiry is `Task.status.leaseExpiresAt`. Commit order:
  1. mint the credential;
  2. CAS the task to Leased (`spec.harness.resolved`, agent, expiry);
  3. create the Lease;
  4. charge `runs`;
  5. `RunAPI.LeaseIssued`.

  If the lease record fails, the task returns to Pending without an attempt. If a credential can't be minted, the task stays Pending with reason `git-credential` and the next task is tried. It then backs off (`fx/rt-mint-backoff`): no agent is offered it again for 30 s, doubling to 10 min per failure, with one warning per failed try; the in-memory backoff clears on a successful mint, when the task leaves Pending, and when a catalog is swapped in (`resync-catalog` included), so a missing App installation costs one provider call per delay, not one per poll. Each failed try also sets `status.reasonDetail` (group 1): for an App that isn't installed on the repo (`provider.NotInstalledError`), "the reviewer App (ccf-factory-reviewer) isn't installed on owner/name: install it, the next retry is at HH:MM UTC"; for any other failure, the repo, the identity, the error and the next retry. Any other reason, and the lease, clear it. Ended leases are kept for 1 h, so a retried complete or abandon is answered, then deleted.
- **The lease controller** runs on the controller runtime, keyed by task id. It watches Tasks and Leases, and Agents only to wake polls. It reads the leases before the task, so a lease just created is never revoked. It does three things:
  - **Revocation (the hook RT-S2-T01 left):** any open lease whose task is no longer Leased/Running/Cancelling by that agent is ended `revoked`. That covers a run timeout, a cancel without an agent and a re-lease. Heartbeat, Complete and Abandon also check the task and answer `ErrLeaseGone`.
  - **Expiry:** after `leaseExpiresAt`, the task returns to Pending with `attempts + 1` and reason `agent-lost` when `Queue.retry` allows (same rule as the run controller's), else it fails with `agent-lost`. A Cancelling task goes through `RunAPI.AckCancel` to Cancelled. The run controller then closes the run (`lost` or `failed`).
  - The first heartbeat moves Leased to Running.
- **Matching** runs under one scheduler mutex, so in-process checks and charges are atomic (budgets, queue concurrency, capacity). The task CAS still guards against other writers.
  - Row 4: capacity is the smaller of `lending.maxConcurrent` and `pool.maxTasksPerAgent` that are set, or 1 when neither is. `Busy` is derived (Ready ↔ Busy by running tasks). A Throttled agent whose `until` passed counts as Ready and is settled back to Ready. Draining and Offline give `agent-busy`.
  - Row 5: no `lending` lends nothing. Non-unlimited lending needs the queue listed. The harness tested is the request's (or the TaskSpec default's) name.
  - `hours` is `[days] HH:MM-HH:MM [IANA zone]`, UTC by default. An end before the start crosses midnight and belongs to the day it starts on. `time/tzdata` is embedded.
  - Row 7: with no request and no default, the agent's offers are tried in order.
- **Daily caps.** There is no day field on `Agent.status.today`, so the agent's daily usage is a fourth `BudgetCounter` scope, `budget.agent.<agentId>.<UTC day>`, checked against `maxRunsPerDay`/`maxTokensPerDay` and mirrored to `status.today`. Days are UTC, like the other budgets, not the lending zone.
- **Budgets.** `runs` is charged at lease time; `tokens` and `usd` from heartbeat deltas and the complete top-up; all to the queue, RuleSet (from provenance) and agent counters, with CAS create-or-update. USD counts only runs whose resolved auth is `apiKey`. **The catalog has no global budget**, so the global scope isn't checked. Over budget, the task stays Pending (`budget`) until the next UTC day's counter.
- **Fairness.** Each pass walks the pending tasks in order (queue priority desc, `createdAt`, id). It gives each task to the fairest *parked* poll not yet given one (prefer-limited by default, then least-recently-leased, then name). A poll takes the first task it is given. The skipped tasks are left to the other parked agents, which the same change has woken. Last-lease times and parked polls are in memory.
- **Anti-affinity.** The PR's last author run is the newest Run of the work item's tasks whose TaskSpec identity is `author`. The "only Ready agent" test asks whether another serving agent in the pool could take the task at all (lending lists, labels, harness; not hours or daily caps). If none can, the author's agent takes the task once it has waited 10 min (in memory, from the first time it was held back).
- **Anti-affinity per harness, with a configurable wait (`fx/sched-drop-anti-affinity`).** Every run gets a clean VM workspace, a clean harness context and its own skills, and the author and the reviewer are different GitHub Apps, so a review on the agent that wrote the PR only shares something with the author when it runs on the same harness. Row 8 now holds a reviewer task back only then: the author run's harness is the name in its Result (`result.harness.name`), else its task's `spec.harness.resolved.name` (set at lease); the reviewer's is what row 7 resolves for this agent now (that is what the lease records). An unknown author harness holds nothing. `factory serve --anti-affinity-wait` sets the wait (`Options.AntiAffinityWait`, default 10 min); `0` there is `scheduler.AntiAffinityOff` (negative), which turns the row off, since a zero option takes the default. `run.sh` passes `FACTORY_ANTI_AFFINITY_WAIT`.
- **Reasons.** The tick (`Refresh`) writes on each pending task the reason of the agent that got furthest down the table (`no-capable-agent` when the pool has no agent), only when it changed. Heartbeat progress is appended to the open Run (CAS, ring of 200).
- **Read credentials (added later, `fx/gh-s0-read-credentials`).** `mint` returns the task token (its repo only: no read repos, so write access is on one repo) and, unless `credentials.git.publicReads`, a `provider.ReadOnlyRequest` token per `credentials.git.readRepos` entry, all or nothing: a failed read mint is `git-credential` like the task token. `Grant.ReadCredentials` and `HeartbeatResponse.RefreshedReadCredentials` carry them; `Lease.status.credentialExpiresAt` is the first expiry of the set, and a refresh re-mints the whole set. Private vs public is in [contracts.md §Refinements](../../foundation/LLD/contracts.md#refinements-fnd-s1-t03).
- **Not done here.** There's an SSE broadcast of progress (RT-S2-T03), and metrics (RT-S2-T04). `controllers/run.go` still has `TODO(RT-S2-T02)` in `checkTimeout`; revocation is done here, so that comment can go in a controllers PR.

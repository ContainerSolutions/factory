# P3: The dispatcher keeps its state in object storage

> Status: **proposed**. Workstream: RT (dispatcher runtime). Builds on
> [P2](P2-dispatcher-state-file.md) (its journal and snapshot formats) and
> [ADR-0009](../adr/0009-dispatcher-state-is-a-log-in-object-storage.md). One contract change:
> `/readyz`'s description (§Rollout, PR 4). Touches `internal/store`, `cmd/factory serve`,
> `factory state`, `internal/auth`'s OAuth state, the provider controller's intake and
> `factory-agent supervise`'s polling, and removes the work-item rebuild from markers.
> Decisions at the end (Gustavo, 2026-10-27).

## Why

P2's file store keeps the dispatcher's state in a local directory and allows one writer: it
holds an exclusive flock on the dir. The next step (IMPROVEMENTS.md, "a shippable factory") runs
our own instance at `factory.container-solutions.com` on Cloud Run or Lambda:

- **No local disk survives.** An instance's filesystem is gone when it scales to zero.
- **Several instances run at once.** The platform starts a second instance under load, and
  during a deploy the old and new revisions overlap. Both write the same state.
- **Idle costs nothing.** Nothing may need to run between requests just to keep state
  correct.
- **The bill stays at or near zero.**

## Goal

1. **One state, many dispatchers:** `factory serve --state-url gs://bucket/prefix` (or
   `s3://…`) keeps all state in a bucket. Any number of dispatchers can share it, and no write is
   lost or applied twice.
2. **The same store semantics:** the `Store` interface is unchanged and the new backend passes
   `storetest` as is. That covers CAS on `resourceVersion`, the global version order, `List` at
   a version, `Watch` with `SinceVersion`, leased locks, retention and transcripts.
3. **One precondition only:** every object is either created once or not at all ("create if
   absent"). GCS (`ifGenerationMatch=0`) and S3 (`If-None-Match: *`) both support it, and so does
   a local directory (`O_EXCL`).
4. **Cold start in about a second** from a snapshot and a short log.

Non-goals:
- High write throughput (we expect a few writes a second at peak).
- Strict linearizable reads across instances. Writes are always checked against the latest
  state; reads may lag by up to the sync interval (§Reads).
- Moving P2's dir. The local file store stays, for development and for the run kit.

## Design: a shared log of create-only objects, replayed into memory

Every dispatcher keeps a full copy of the state in memory, using the same `store/memory` engine as
the file store. The bucket holds the authoritative history: an **append-only log** with one
object per change, named by its version, and periodic **snapshots**. A change is committed when
its log object is created. Creation is conditional on the name being free, so two dispatchers
can't both take the same version.

This is P2's design with the journal file split into objects, one per line, and fsync replaced
by a conditional create. Delta Lake's transaction log on S3 uses the same pattern.

```
<prefix>/
  log/00000000000000001043.json     one change: P2's journal line, plus the writer's id
  log/00000000000000001044.json
  snapshots/00000000000000001000.json.gz   the whole store at version 1000 (P2's snapshot format, gzip'd)
  transcripts/<run>.<id>.jsonl.gz   one blob per put of a transcript or environment log
```

- **Log entries** are `{"v","at","op","kind","name","obj"?,"w"}`. This is P2's journal line plus
  `w`, the writer's instance id and a per-write nonce. The version `v` is the object's name, and
  versions are contiguous. An entry is about 1–2 KB.
- **Snapshots** are named by the version they hold, and are never overwritten. A snapshot carries
  the newest schema version among the entries it holds, not only its writer's. Otherwise a
  release that reads only N could load N+1 data from a snapshot written by release N.
- Names are zero-padded to 20 digits, so a listing comes back in version order.

### The write path, and a lost write

The file store's commit hook is the seam (`memory.WithCommitHook`, store.md §File store). When a
write is checked and encoded under memory's write lock, the hook creates `log/<v>` if absent,
where `v` is memory's next version. Then:

- **Created:** the write is committed. Memory applies it and tells the watchers, as in the file
  store.
- **Taken:** the hook reads `log/<v>` and compares its nonce with this write's. Both SDKs retry a
  conditional create on their own (GCS treats `ifGenerationMatch=0` as idempotent, and
  aws-sdk-go-v2's standard retryer retries PutObject), so "taken" can be this write's own
  earlier attempt landing. **Its own nonce:** the write is committed, as if the create had
  succeeded. Re-running it would answer `ErrAlreadyExists` or `ErrConflict` for a write that
  happened, and callers such as `update()` would apply their change twice: `scheduler.charge`
  would double-charge a BudgetCounter.
- **Another nonce (a lost write):** another dispatcher committed version `v` first. The hook fails
  the write with an internal `errBehind`, leaving memory untouched. The object store then **syncs**:
  it reads `log/<v>`, `log/<v+1>`, and so on until one is missing, and applies them to memory.
  Their watch events fire. Then it **re-runs the caller's operation** against the fresh state:
  - A `Create` whose key now exists gets `ErrAlreadyExists`.
  - An `Update` whose object changed gets `ErrConflict`, and the caller re-reads and retries, as
    every caller does today.
  - A `Delete` of something now gone gets `ErrNotFound`.
  - Anything else is tried at the next free version.

  A successful create of `log/<v>` proves the writer had applied `1…v-1`. So a CAS checked
  against local memory is a CAS against the whole history, and no two writes can both pass the
  check. After 20 lost races in a row the call fails with an error.
- **A write the local copy refuses** (`ErrNotFound`, `ErrAlreadyExists` or `ErrConflict`, decided
  before any create) syncs once and runs again before it answers. A stale instance would
  otherwise refuse writes that the log allows, and its caller would re-read the same stale copy.
  That costs one GET per refused write. So writes are always checked against the latest state,
  as Goal 2 says.
- **Unknown outcome** (a timeout or a 5xx after the request was sent): the hook reads `log/<v>`
  and goes on as for "taken". If it is missing, the create is retried once, which is safe because
  it is create-if-absent. Then the write fails, so a bucket outage holds memory's write lock for
  at most two creates and a read, about 20 s with the 5 s deadline per request.

The hook holds memory's write lock for one PUT, about 20–60 ms in-region. Reads wait during it,
as they wait for the file store's fsync. That is fine at a few writes a second. If it shows up in
latency, the next step is to check writes in the object store under its own commit lock, outside
memory's read lock; it is not built.

There is no group commit: one change makes one object. A heartbeat makes about six sequential
writes (task, lease, agent, run progress, budget counters), so about 0.3 s.

### Reads and freshness

Reads (`Get`, `List`) are served from memory, so they never list the bucket. To see other
dispatchers' writes, an instance syncs (reads the log from its version + 1 until an entry is
missing):

- **before a read**, when its last sync is older than `--state-sync-interval` (default 2 s);
- **before every write** (implicitly: a lost race syncs);
- **on a `Get` that finds nothing**, once, before returning `ErrNotFound`. The common
  cross-instance case is an agent that leases on instance A and heartbeats on B, or a UI session
  created on A and used on B. That case finds the object without waiting for the interval;
- **in the background**, every sync interval while the process runs. This keeps watches live on
  a long-running `serve`. On Cloud Run the CPU is throttled between requests, so this loop may
  not run; correctness never depends on it.

An up-to-date instance's sync is one GET of a missing object (a 404).

**Stale reads are safe where a CAS write follows them.** A read may be up to one sync interval
old. When the decision ends in a CAS on the object read, a stale read costs a retry, not a wrong
result. Notices, such as "was a cancel requested?" on a heartbeat, arrive one heartbeat later.
Decisions that read many objects and write a different one are not covered by a CAS. Two kinds
need more:
- **The scheduler's lease matching** gets a lock (§Locks and leases).
- **Lookups whose empty answer leads to a create or a drop.** The link index answers "which
  work item links this object?". A stale "none" would make the intake filter drop a linked
  event (§Only events that can matter are stored), or make `workItemFor` create a second work
  item for an object another instance just linked. For example, an agent's effect opens a PR
  and `linkCreated` records the link on the instance that served it, while GitHub's
  `pull_request` webhook follows within a second. So a lookup that finds nothing syncs once and
  asks again before it acts, like a `Get` that finds nothing. The `Store` interface has no sync,
  so the object store implements an optional `store.Syncer` (`Sync(ctx)`), and these callers use
  `store.SyncIfSupported`. Memory and the file store have nothing to sync. When the instance is
  current, an empty lookup costs one GET of a missing log entry.

### Watches

Watches are memory's: changes applied from the log (from a sync or a lost race) fire watch events
exactly like local writes, in version order. A watcher on B sees A's writes in order, one sync
interval late at most while syncs run. `SinceVersion` replays memory's ring, as before. After a
cold start the ring begins at the loaded version, and an older `SinceVersion` is
`ErrWatchExpired` (the informer resyncs), as after a file-store restart.

### Locks and leases

`Lock` is the only part of the `Store` interface that memory keeps per process today (other
per-process state outside the store is in §Several dispatchers). In the
object store a lock is a stored record of an internal kind, `StoreLock`, named after the lock.
It holds `{holder, token, expiresAt}` on the store's clock and is written through the log like
any other object:

- **Lock** creates the record. If the record exists and has expired, Lock takes it with a CAS
  update. While another holder has it, Lock waits until the lock is free or expired, or until
  `ctx` ends. It wakes on a local `Unlock`, on the store's clock reaching the expiry (so
  storetest's fake clock drives it), and on a sync every second for other instances' releases.
- **Renew** is a CAS update of the record, with `ErrLockLost` if the record changed or expired.
- **Unlock** deletes the record only at the version this holder last wrote. Memory gets a
  conditional delete for this: it is internal to the store packages, and the `Store` interface
  doesn't change. So a holder never removes a lock someone else reclaimed after it expired.
  `Unlock()` takes no context and returns no error, so it writes under its own 5 s deadline.
  A failure is logged at warn and the record is left to expire.
- **Dead records** (a holder that crashed, or an Unlock that failed) are deleted by the leader's
  sweeper with the same conditional delete, once expired. So expired locks don't pile up in the
  snapshots.

So the work-item locks (`wi/<id>`, taken by controllers and by the API's work-item admin
actions) and the leader lease hold across dispatchers. A locked section costs two writes. Lock
expiry compares instance clocks; the platforms keep them within milliseconds, and the TTLs are
minutes.

**Task leases need one more lock.** The commit is a CAS on the `Task` it leases (lease.go), so
two dispatchers can't lease the same task. But `tryLease` checks against a snapshot of many
objects, under the in-process `Scheduler.mu` ("checked and charged atomically in this process"):
- the queue's concurrency;
- the agent's capacity;
- lending;
- the budgets.

Two instances, each up to a sync interval behind, could lease two *different* tasks and break a
queue's `concurrency: 1`. So every lease commit, on every instance, serialises on one store lock,
`sched`. It is one lock, not one per pool, because lending lets a queue's tasks run in other
pools. The section is kept short and bounded:
- **The git credentials are minted before the lock.** Minting is the slow call to GitHub. A
  credential minted for a candidate that then fails the re-check is left to expire (an hour at
  most), as today when a commit fails after the mint.
- **The lock is taken with a 2 s context.** A timeout is not `errSkip` (which `tryLease` treats
  as "try the next candidate in this pass"). It is a new sentinel, `errSchedBusy`, that **ends
  the pass**: `tryLease` returns no grant and the poll parks until the next wake or tick. So a
  pass waits for the lock at most once and mints at most once. `Scheduler.mu` is held during
  that wait. So that several parked polls don't each wait in turn, a timeout also records
  "sched busy until now + 2 s" in the scheduler. A pass that starts before then ends at once,
  with no wait and no mint. So an instance's other polls (and `Refresh`, `Explain`) are held up
  by at most one 2 s wait at a time, even while a dead holder keeps the lock for its 30 s TTL.
  Heartbeats, completes and abandons don't take `Scheduler.mu`.
- **The lock's TTL is 30 s, and the section runs under a 15 s deadline (TTL/2).** The section is
  taking the lock (which syncs), reloading the snapshot, checking the candidate again, and the
  commit's writes. The lock can't expire under a running section, so a second instance can't
  take it mid-commit. A section that hits the deadline fails like a commit error today: the
  task is unleased, or its lease expires.
- **Cost:** two writes per commit attempt, a candidate that fails the re-check included. That is
  a few per lease granted, not per poll.

The scheduler's other in-process state stays per instance and only affects preference, never a
limit:
- `waiting`: fairness picks among the polls parked on the same instance;
- `heldSince`: the anti-affinity wait;
- `mintBackoff`: a task whose credentials failed to mint may be tried once more by another
  instance before its delay ends.

### Transcripts

Each blob (a transcript or an environment log) is an object at
`transcripts/<run>.<id>.jsonl.gz`, with P2's escaping for unsafe run names. `<id>` is random and
new on every put, and the blob is written with create-if-absent. The `TranscriptBlob` record
names the id, so a read follows the record to its blob. A drop deletes only the blob its deleted
record named, and a re-put replaces the record and deletes the blob it replaced. So a drop and a
re-put on two instances can't remove each other's data, in either order. The unique names
also make the "unconditional `Put`" unnecessary for transcripts.

The caps and retention need sizes and creation times on the store's clock. Listing can't give
these (S3's listing has no custom metadata), so each blob gets an index record of an internal kind,
`TranscriptBlob`, `{size, putAt}`, written through the log. `putAt` is set on every put, a replace
included, on the store's clock. `createdAt` can't serve, because an Update keeps it, and memory's
transcripts count a replaced blob as new (storetest's `TotalCapEvictsOldest`):
- **The 2 GB total cap and oldest-first eviction** come from these records, read from memory:
  the oldest `putAt` goes first, and on a tie the lower resourceVersion goes first.
- **Retention** sweeps the records by `putAt` and deletes their blobs.
- **A Run's delete** removes its records and blobs, after the Run's own delete commits.

A crash between the blob and its record leaves an orphan blob. The leader's orphan scan lists
`transcripts/` and deletes blobs that have no record and are more than a day old (the blob's own
age, so nothing has to survive a restart). The scan, and the log and snapshot retention, run at
a process's first sweep and then daily. With scale to zero, an instance may never live a day, so
"once a day" alone might never run.

### Two releases on one log

During a deploy, old and new revisions run at once on the same log, and P3 must not lose writes
then either. Two things would:
- **An unknown kind.** The file store's decoder treats a kind it doesn't know as corruption
  (journal.go, snapshot.go). An old revision that meets a new kind's entry couldn't sync, so it
  couldn't commit anything.
- **An unknown field.** An old revision decodes an object without the fields its Go type lacks.
  When it CAS-updates that object, the new fields are silently dropped.

So every log entry and snapshot carries the writer's **schema version**: one number for all
stored types, bumped by any change to a stored kind or its fields. A golden test of the stored
types' shape (every v1alpha1 runtime kind and `internalKinds()`) is pinned to the version: a
change to a stored type fails until both are updated.

**Expand, then write.** Each release knows two numbers: `SchemaVersion`, which it writes, and
`ReadableSchema`, the newest it can read without losing data. A change to stored types ships in
two releases:
1. **Release N adds the new kind or field** to the Go types (additive: new kinds and optional
   fields). It bumps `ReadableSchema` to N+1, but still writes N.
2. **Release N+1 bumps `SchemaVersion`** to N+1.

So during a deploy, and after a rollback from N+1 to N, every instance can read every entry and
keep the fields it rewrites.

**Changes accumulate into the pending version** (Gustavo, 2026-10-27). While `ReadableSchema` is
already N+1 and no release writes it yet, further stored-type changes join N+1. The golden's
`-update` accepts them. One read-ahead step then covers them all, and the release that bumps
`SchemaVersion` to N+1 writes them together. **N+1 freezes once a release that reads it ships**
(the expand release; Gustavo, 2026-10-27). A later change can't join N+1, because the deployed
expand release wouldn't know it and would drop its fields on rewrite. It waits until
`SchemaVersion` N+1 ships, then opens N+2. A test can't know what shipped, so `schema.go` and the
release checklist say this.

**An entry newer than `ReadableSchema`** can only appear when a deploy skipped the expand step.
The instance that meets it stops writing:
- its writes answer `503` with a problem saying why;
- its reads go on from what it has;
- its `/readyz` turns 503.

An instance whose newest snapshot is newer than its `ReadableSchema` refuses to start, with an
error naming both versions. Draining comes from the deploy moving traffic to the new revision.
Cloud Run and Lambda have no readiness probe that drains, and `/readyz` listens on loopback
only.

**`/readyz`** on any instance, leader or not, answers 200 once the catalog and the store are
loaded and the first sync has succeeded. It answers 503 when the store has stopped writing, or
when its last successful sync is older than `MaxLag`.

The tests:
- a newer version from another store: writes stop, with nothing lost;
- a rollback within `ReadableSchema`: the old release keeps writing and keeps the new fields;
- a snapshot past `ReadableSchema`: start refused.

### Snapshots, cold start and log retention

- **Compaction** runs on the leader when the log is 1,000 entries past the last snapshot, and on
  `Close` on any instance. Two instances closing at once may both write a snapshot. Each is
  named by its own version and created only if absent, so that is safe, at one PUT per
  instance stop. It writes `snapshots/<V>.json.gz` from `memory.Snapshot` at version V.
- **Cold start:**
  1. List `snapshots/` and GET the newest snapshot.
  2. List `log/` starting after V, then GET those entries in parallel. There are at most about
     1,000 of them.
  3. Check the versions are contiguous from V + 1, and replay them into memory (`memory.Restore`,
     then apply).

  That is two LISTs and a few hundred small GETs, about a second.

- **Deleting old objects without losing a write.** A log entry is never deleted while a lagging
  writer could still try to create it again. If it were deleted, a stale `create if absent`
  would succeed and rewrite history. Two rules make that impossible:
  1. The leader deletes a log entry only when it is covered by a snapshot **and** is older than
     `SegmentRetention` (7 days). Snapshots are kept while they are the newest two or are less
     than 7 days old.
  2. Before a write, an instance whose last successful sync started more than `MaxLag` (1 hour)
     ago reloads instead of probing. The reload lists from its version, so it sees any gap left
     by deleted entries, and if there is one it starts again from the newest snapshot.

  Together these guarantee a writer has always seen the log within the last hour. An entry it
  hasn't seen is less than an hour old, so it can't have been deleted yet.

- **Retention of stored objects** is unchanged: the leader's sweeper deletes through the log, as
  the file store's deletes go through the journal.
- **No bucket lifecycle rules.** An age-based lifecycle rule could delete log entries that no
  snapshot covers yet, for example after weeks at zero instances. Deletes are explicit, and both
  GCS and S3 charge nothing for them.

### Several dispatchers: who runs what

Every write is a CAS or a create on the log, so any number of instances can serve requests. The
background loops, though, must not run twice. Two rule controllers would both evaluate the same
Event and both post the same notice. So:

- **Every instance** serves the API: webhooks (which store Deliveries), the agents' register,
  lease, heartbeat and complete calls, the UI and the CLI. The scheduler's lease matching runs
  in the agent's request on any instance, its commit under the `sched` lock (§Locks and leases).
  Each instance also loads its own catalog snapshot, and runs the tick that wakes its parked
  polls.
- **Per-process state outside the store** must not break when a client's next request lands on
  another instance:
  - **UI logins.** Pending OAuth logins live in a process map (`internal/auth`), so a callback
    on another instance fails with `ErrStateMismatch`. Instead:
    - **The OAuth `state` becomes self-contained:** signed with the session key under its own
      label (so it can't pass for a session cookie), and carrying a random id, its expiry and
      the return URL.
    - **It stays single-use, as today** (`FinishLogin` deletes the pending state after one use).
      Finishing a login creates a record of an internal kind, `UsedLoginState`, named by the
      hash of the state's id, with create-if-absent. A second use is `ErrAlreadyExists` and
      fails. The sweeper deletes the record after the state's TTL. That is one write per login,
      and no map, so `MaxPendingLogins` goes. `authneg_test.go`'s "a state used twice" keeps
      passing, and `TestBeginLoginBounds` changes with the bound's removal.
    - **Every instance needs the same session key.** Without `FACTORY_SESSION_KEY`, each instance
      makes up a random one (auth.go), so both the state and the session cookies would fail on
      another instance. So `serve --state-url` refuses to start without it.
    - **Rotation:** `FACTORY_SESSION_KEY_PREVIOUS` is also accepted for verifying. Rotating then
      ends no session or login in flight. Drop the previous key after the session lifetime.
  - **`/stream` resume.** A `Last-Event-ID` from a fresher instance is "ahead of the store" on
    a lagging one: `ErrInvalid`, which the stream doesn't fall back from. The stream syncs and
    retries once. If the version is still ahead, it starts from now, as it does for an
    expired version.
  - **The work-item resync's baselines** (the per-link observation digests) are in process.
    The first resync after a restart or a leader change records a baseline and emits nothing,
    so on a scale-to-zero instance `factory.workitem.resynced` would rarely fire. The digests
    move to the store, in a record per work item of an internal kind, `LinkObservations`. It
    is written only when a digest changes, so a new leader compares with the last one seen. It
    is deleted where `forget` clears the digests today: when the item is retired or deleted, or
    a link is removed (the link's entry then goes). The sweeper deletes records whose work item
    no longer exists. It holds digests and link refs only, no provider text.
  - **Caches that only save work** stay per process, for example the work-item controller's
    last-projection digests, its `lastResync` times and the observation cache: a cold one
    costs a provider call, never a wrong result. A new leader's first resync spreads its
    checks over the resync interval, with jitter, rather than observing every open item at
    once. So does the scheduler's preference state (§Locks and leases).
- **The leader only** runs every loop that writes from a watch or a timer: the controllers
  (provider, rule, debounce, work-item, run), the scheduler's lease controller and its
  `Refresh`, the sweeper (lock records included), compaction and the transcript orphan scan.
- **The App installation check runs on every instance** (Gustavo, 2026-10-28). It only reads
  GitHub and keeps its result in process, so each instance shows the catalog API's installation
  warnings, wherever the UI lands.
- **A delivery is deleted only at the version read.** A leader that paused past its lease could
  otherwise delete a Delivery that a redelivery re-created with the same id, before it was
  normalised (Gustavo, 2026-10-28).
- **The provider's delivery recovery** (`pc.Recover`) runs as today on every instance, at its
  start and before its listeners open, with `since` 10 s (GitHub's timeout) before its process
  start. It covers what GitHub sent while the instance was loading, including a delivery that
  timed out while it started. Recovering is the same intake as a webhook (a create of
  each Delivery, deduplicated by id), so any instance may do it. There is no other start-up work:
  the work-item rebuild from markers is removed (§No rebuild from markers).

  **Known limit (Decision 5):** a webhook is safe once its Delivery is stored and GitHub has its
  2xx. GitHub doesn't retry a failed delivery, though, and `Recover` covers only each instance's
  own start-up. So a live delivery that fails is lost unless someone redelivers it from the
  App's delivery log, which GitHub keeps for about 3 days. It can fail through a cold start past
  GitHub's 10 s timeout, a 503 during a deploy, or a bucket error. Running the dispatcher on a
  VM behind a load balancer, with no cold starts, makes this rarer.
- **The leader** is whoever holds the `leader` lock: TTL 3 minutes, renewed every minute. Every
  instance tries to take it at start and on each tick, with a context of a few seconds, because
  `Lock` waits while another holder has it. The holder checks it still holds the lock before
  each reconcile, and stops its loops when Renew fails.
- **When leaders overlap** (an old leader paused past its TTL, for example by Cloud Run
  throttling its CPU), the stored state stays correct, because every write is a CAS or a
  create. **Provider calls** are guarded differently. `applier.send` calls the provider
  (`conn.Apply`) *before* it records the Effect, so the Effect's deterministic id doesn't stop a
  double post. What stops it is the work-item lock, which every reconcile that applies
  effects takes (rule, debounce) and which is now store-wide, together with the provider's
  own idempotency markers.

  The `serve` PR audits every store write **and every provider call** a leader-only loop or the
  recovery makes. Each must be one of these, and it fixes any that isn't.
- **Scale to zero.** With no instance running nothing runs, and nothing needs to. A webhook or an
  agent call starts an instance, which takes the leader lock if it is free. Timed work
  (debounces, lease expiry, schedules) needs a request to happen; IMPROVEMENTS item 9 adds a tick endpoint for
  that (Cloud Scheduler or EventBridge). The store needs nothing from it.

With one dispatcher, as today, that dispatcher is always the leader, and the split above changes
nothing. The two changes below (no rebuild, the intake filter) apply to one dispatcher too.

### No rebuild from markers

Today `serve` starts by rebuilding the work items from the markers the factory writes on GitHub
(`wic.Rebuild`, controllers.md). It lists every bound repo's issues and PRs and recreates the
items, their counters and state. That existed because the store was in memory and a restart lost
everything (ADR-0001). With durable state (the file store, and now the bucket), the store is the
authority, and the rebuild only re-lists GitHub on every start to confirm what the store already
holds. With several instances and scale to zero, it would also need its own coordination: once
per bucket, per binding, with a gate on the rule controller while it runs.

So the rebuild is **removed**:
- **What goes:** `wic.Rebuild`, its call in `serve` and `/readyz`'s wait for it. `provider.Conn.List`
  goes too, since the rebuild is its only caller: from the interface, the GitHub adapter
  (`list.go`) and the fakes. This supersedes ADR-0001's start-up rebuild and its "every adapter
  must implement `List`".
- **A durable store is required.** Without the rebuild, the memory store would lose every open
  item's state and run counters on a restart. The next event would then create a fresh item, so
  state-driven rules start over and the per-item run cap resets. So `serve` refuses to start
  without `--state-dir` or `--state-url`, unless it is given `--dev-fake-providers`, whose fake
  GitHub is in memory anyway, or the explicit `--state-memory` flag (for tests and quick local
  runs). That flag logs that every item starts fresh on a restart. The run kit already passes
  `--state-dir`.
- **Projections stay.** The factory still writes its labels and marker comments on GitHub, for
  people to read. Nothing reads them back into the store. The GitHub adapter's `Project` still
  reads the App's own markers, to find the one it updates and which labels it owns ("Old
  markers" below). `Observe` stops parsing them: `Observation.Projections`, `projectionsIn` and
  the sentence about it in `provider.go` go, since `mergeProjections` in the rebuild is their
  only reader (the resync's digest leaves them out).
- **The work-item resync stays.** It observes each open link and emits
  `factory.workitem.resynced` for changes made on GitHub while no dispatcher was watching. Its
  baselines move to the store, so that holds across restarts and leaders (§Several
  dispatchers).
- **Recovery is replay.** Events that matter are stored (90 days, `--retention`), so an event
  handled wrongly is replayed (`POST /events/{id}/replay`) rather than reconstructed from
  markers.
- **What is lost: nothing comes back from GitHub (Decision 7).** When the state itself is lost,
  open items start fresh on their next event. Their rule state is gone, and their run counters
  restart, the per-item run cap included. That covers a deleted bucket or dir, a
  `factory state reset`, and a corrupt state dir that is reset. No stored events survive it to
  replay. People recover what they want by redelivering webhooks from the App's delivery log
  (GitHub keeps about 3 days), reopening issues or re-applying labels.
  - `factory state reset` says so. Today its prompt and output, its golden file and README §6
    promise that "work items come back from the GitHub markers"
    (`cmd/factory/state.go`, `testdata/state/reset.golden`). They change to say that open items
    restart with no state and no run counters, and how to recover: redelivery from the App's
    delivery log, reopening issues, re-applying labels.
  - For an accidental delete, bucket soft delete (GCS's default of 7 days) or S3 versioning
    restores the state itself.
- **Old markers.** The cloud instance starts fresh (Decision 2), so items on repos that carry
  markers from the workstation's runs start again. Work-item ids are deterministic
  (`objectID("wi", "link/<provider>/<ref>")`) and markers are one per work item and object.
  What happens depends on the marker:
  - **Same App, same provider name, same object:** the fresh item gets the old item's id, so its
    first projection takes over the old marker and its labels, with counters restarted.
  - **Same App, different id** (the provider is named differently in the new catalog, or a PR
    whose old marker names its issue's item): the fresh item writes its own marker beside the
    old one. `Project` reads the App's other markers to decide which labels it owns
    (`othersOwn`), so labels the old marker owns are never removed. Delete such stale markers
    by hand if they get in the way.
  - **Another App** (a new GitHub App for the cloud instance): its markers are ignored, as
    forged-looking markers are today. The fresh item writes its own marker and labels.
  - **The cutover:** stop the workstation's dispatcher, or remove the shared repos from its
    catalog, before the cloud instance's webhook goes live. Two dispatchers on the same App and
    repos would project the same work-item ids onto the same objects, flipping markers and
    labels between them. In the same-App case, the cloud item's first projection removes labels
    the old marker owned and restarts its counters (Decision 7).

### Only events that can matter are stored

Today the provider controller stores every delivery the GitHub App receives (a `Delivery`, then
an update when it is normalised), and the Events normalisation produces:
- **Event types the adapter doesn't map** (pushes and workflow runs, for example: `neutralType`
  returns "") produce no Event. The delivery alone costs 2 writes, plus a third when the
  retention sweep deletes it.
- **Mapped types** are stored as an `Event` whether or not a rule could use them: issues, PRs,
  comments, labels and reviews, but also completed check suites and check runs (one
  `pr.checks.completed` per PR), releases and deployment statuses. The rule controller then
  writes its `EventStatus`, and later the sweep deletes all three records. That is about 7 writes
  over the event's life, whether or not a rule could ever fire. A `WorkItem` is created only when
  a rule actually fires for the event (`workItemFor` checks `rules.CouldFire`, then evaluates
  against an empty item with permissive rails).

So the provider controller drops, at normalisation, every event that can't matter. An event is
stored only when one of these holds:
- **Its object is linked to a stored work item**, found through the `spec.links` index read from
  memory (`eventLink`, as `workItemFor` looks it up). The rule controller adds such an event to
  the item's timeline even when no rule fires, and it can be replayed later.
- **A rule could fire for it:** `rules.CouldFire(catalog, event, empty view)`, the check
  `workItemFor` already makes. It covers the RuleSet's providers and scope (repos minus
  exclude, branches and channels, and the rule for objects with no repo) and each rule's `on`
  and `providers`, but not its `where`. `where` needs the work item's state, and the item may
  not exist yet. An empty view is correct for a provider event: scope reads the item's links
  only for `factory.*` events. Those never pass through the provider controller, since they
  are created in the store directly.
- **The factory caused it** (`actor.isFactory`), even when unlinked and no rule could fire. When
  an agent's effect opens a PR, `CreateEffect` calls the provider (`conn.Apply`) before it records
  the link (`linkCreated`, `internal/server/effects.go`). So the PR's own `pr.opened` can arrive
  in between. It is stored, and the rule controller handles it as today: it finds the item once
  the link exists, or logs it at Info (`skipped`). It counts in neither drop reason. Such events
  are few.

Each dropped event is logged at Info with its type, its object and why it was dropped (no
provider text), and counted in `factory_events_dropped_total{provider,type,reason}`. A wrong
RuleSet scope, the most common misconfiguration, then leaves a trace at the run kit's log level.
That log is where "why didn't my rule fire?" starts for an event that isn't stored. A dropped
event is not in `factory state events` and can't be replayed (404). README §6 and §7 and
`factory state events --help` say so, and say the way back is redelivery from the App's delivery
log.

**A delivery left with no stored event is deleted at once,** whether its events were dropped or
it had none (an unmapped type). A delivery the adapter rejected (`DeliveryFailed`, with its
reason) keeps the delivery retention, for debugging. The create keeps it
durable until the webhook is acknowledged, and the delete is a log entry like any store write.
So such a delivery costs 2 writes, where today it costs 3 to 7. The bucket's own deletes, of log
entries and blobs, are free; the store's deletes are not.

**A redelivery** (GitHub's redeliver button, or `Recover`) of a deleted delivery is normalised
again, against the catalog as it is then. If a rule or a link was added meanwhile, its event is
now stored and evaluated. That is the one way an event dropped earlier reaches a rule added
later. Otherwise a rule sees only events that arrive after it.

This is a change to the provider controller, not to the store, and it applies to every store.

### Agents poll briefly (`supervise`)

Today `supervise` long-polls. Whenever a slot is free, its lease loop calls `LeaseTask` with
`wait=30s` (one call open at a time). The dispatcher holds the request until a task comes or the
wait ends, and on a 204 the agent asks again at once (`internal/agent/worker`). So a request is always open. On Cloud Run or Lambda that keeps an
instance busy, and billed for CPU, around the clock.

So `agent.yaml` gets a `poll` block:

```yaml
poll:
  wait: 0s       # how long each LeaseTask asks the dispatcher to hold it: whole seconds, 0s–60s (default 30s)
  interval: 30s  # how long an idle agent sleeps after a 204 before asking again (default 0s)
```

- **The defaults keep today's long poll** (`wait: 30s`, `interval: 0s`), for a dispatcher on a
  laptop or a VM. A cloud dispatcher's agents set `wait: 0s` and an interval such as `30s`.
- **`wait: 0s` needs an interval of at least `1s`**, or the agent would poll in a hot loop;
  `agent.yaml` rejects it.
- **Older agents reject the `poll` key,** because `agent.yaml` is decoded strictly. Rolling an
  agent back means removing the block, so `agent.yaml.example` shows it commented out.
- **A poll that gets a task polls again at once**, so free slots fill without waiting. Only a 204
  sleeps the interval. Throttling and failure backoff are unchanged.
- **The dispatcher needs no change.** `LeaseTask` already takes any `wait` from `0s` to `60s`, and
  the scheduler then runs one matching pass and answers. No contract change.
- **A zero wait must reach the wire.** Today `worker.New` reads a zero `LeaseWait` as unset and
  puts back the 30 s default, before its at-least-`1s` check. So relaxing the check alone would
  still send `wait=30s`. The config fills the 30 s default only when `poll.wait` is absent. The
  worker's options carry the poll settings as a struct whose zero wait means zero, and the check
  accepts `0s`.
- **Heartbeats, completes and abandons are unchanged.** They are short requests already.
- **What it costs:**
  - A task waits up to one interval before an agent sees it.
  - The scheduler's fairness picks among the polls waiting at that moment. With brief polls,
    that is usually just the agent asking, so fairness becomes "first to poll". That is fine for
    a few agents.
  - Anti-affinity still holds a task for up to 10 minutes for a different agent, whose next
    poll takes it.
- **What it buys:** Cloud Run, with CPU allocated only during requests, bills a few
  milliseconds per poll, not 24 hours of held connections. A poll every 30 s keeps an instance
  warm, unbilled, so cold starts stay rare.
- **What that billing mode costs: the leader's work stalls between requests.** A webhook is
  acknowledged once its Delivery is stored. Everything after that runs in the leader's loops:
  normalisation, rules, work items, tasks, effects and their GitHub calls, lease expiry, and
  the sync and leader-lock renewal. With CPU only during requests, those loops advance only
  while some request is in flight. A webhook can then take several polls to become a task or a
  comment, and a slow GitHub call stalls between them. Lambda behaves the same: nothing runs
  after the response. P3 doesn't close this, because it is a hosting choice (IMPROVEMENTS item 9).
  The options:
  - **CPU always allocated** (Cloud Run's instance-based billing): the loops run normally, but a
    warm instance is billed all the time it lives. Agents polling every 30 s keep it alive, so
    that costs about as much as long polls.
  - **Work inside requests:** the webhook handler, and a tick endpoint that Cloud Scheduler
    calls, run the leader's loops until they are idle (bounded) before answering.
  - **A VM behind a load balancer:** always-on CPU at a fixed price, the fallback of
    Decision 5.
  - With a dispatcher on a VM or a laptop, the loops always run. Brief polling there only adds
    pickup latency, and the defaults keep the long poll.

This is the agent workstream's change (`internal/agent`, `cmd/factory-agent`).

### Backends

`internal/store/objectstore` holds the log store and a small interface it uses:

```go
// Bucket is the little the object store needs from object storage.
type Bucket interface {
    Get(ctx context.Context, name string) ([]byte, error)              // ErrNotExist
    Create(ctx context.Context, name string, data []byte) error        // if absent; ErrExists when taken
    List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error // name order
    Delete(ctx context.Context, name string) error                     // ErrNotExist
}
```

| Bucket | `Create` | Credentials |
|---|---|---|
| `gs://` (`cloud.google.com/go/storage`) | `ifGenerationMatch=0` | Application Default Credentials (Cloud Run's service account) |
| `s3://` (`github.com/aws/aws-sdk-go-v2/service/s3`) | `If-None-Match: *` (S3 since Aug 2024) | the default chain (Lambda's role) |
| `file://` (a local directory) | write a tmp file, fsync, then `link(2)` it into place (fails if the name exists); fsync the dir. A reader never sees a half-written entry, and a crash never leaves one that can't be rewritten | none. For development and for tests with two dispatchers on one machine |
| fake (tests) | in memory, with injected failures and lost races | none |

`If-Match` and `ifGenerationMatch=<n>` are not needed: no object is ever updated in place or
overwritten, transcript blobs included (each put has its own name). GCS's limit of
one write per second per object name doesn't apply either, since each name is written once.

Decided while building PR 2 (Gustavo, 2026-10-27):
- **Errors never print the state URL or `FACTORY_S3_ENDPOINT`.** They name the scheme and which
  part is wrong. Masking userinfo isn't enough: a secret containing `/`, `?`, `#` or `"` parses
  outside the userinfo, and AWS secret keys can contain `/`. An `@` anywhere in the URL is refused.
- **`FACTORY_S3_ENDPOINT` must be `https://`.** Only a loopback host (`localhost`, `127.0.0.1`,
  `::1`) may use `http://`, as the emulator tests do.
- **A missing GCS bucket is never `ErrNotExist`.** The SDK reports every 404 on an object as "not
  found". So a not-found `Get` or `Delete` is confirmed with a one-object list of the prefix,
  whose 404 the SDK reports as a missing bucket whatever the wording. The list is skipped when a
  request in the last minute proved the bucket exists, so it costs at most about one list a
  minute while idle (about $0.22 a month with the CPU always on). Every idle sync ends on a
  not-found `Get`, so confirming each one would cost up to about $6.50 a month. Opening a
  bucket lists one object, so a bucket missing at start fails at once.
- **The S3 emulator is `pgsty/minio`, pinned by digest.** It is a community build of MinIO; the
  official images can no longer be pulled. Garage was tested and ignores `If-None-Match: *` on
  PutObject, so it can't test the one precondition.
- **S3's 409 (`ConditionalRequestConflict`) is an unknown outcome,** not `ErrExists`. It is
  re-sent a few times, then read back.

The two SDKs are new dependencies (the repo-layout LLD's allow-list): they get a justification in
their PR. Signing S3 requests (SigV4) and finding credentials on each platform are not worth
writing ourselves. Credentials always come from the platform's identity, never from a key in the
config or the image.

`serve` takes `--state-url` or `--state-dir` (not both). Without either it refuses to start,
unless it is given `--dev-fake-providers` or `--state-memory` (§No rebuild from markers).

**A `file://` bucket and a P2 state dir never share a directory.** Both keep transcript blobs
under `transcripts/`. On a P2 dir the object store would see an empty log, start empty,
and its orphan scan would delete P2's transcripts a day later, with no flock between them. So
the object store refuses a `file://` directory that holds `snapshot.json`, `journal.jsonl` or
`LOCK`, and the file store refuses one that holds `log/` or `snapshots/`.

## What it costs

Object storage charges per request. Every store write (a create, an update, and a delete too) is
one log entry, so one PUT. Only the bucket's own deletes of log entries and blobs are free. The
cloud instance runs the docs and workflows catalog only. The estimate assumes:

| Source | Writes each | Per day |
|---|---|---|
| A run of about 20 minutes: 40 heartbeats of 3–7 writes (task, lease, agent; run progress and 3 budget counters when there is progress and usage), plus the lease (~9), the completion (~15) and 4 for transcripts | ~150–310 | 3 runs: ~780 |
| A stored webhook a rule fires for: delivery (2), event, its status, the work-item lock (2), the work item, a task or effect per action, and the sweep's deletes later | ~10–13 | 20: ~250 |
| A stored webhook about a linked object that no rule fires for, mostly completed checks on a tracked PR: delivery (2), event, its status, the work-item lock (2), the item's timeline, and the sweep's deletes | ~10 | 20: ~200 |
| A webhook that can't matter (§Only events that can matter are stored) | 2 | 200: ~400 |
| An agent's token renewal: at most every 10 minutes, on any call it makes (`TokenRenewEvery`) | 1 | 2 agents connected all day: ~290 |
| The leader lock's renewal, every minute while an instance runs | 1 | 60 an hour of uptime |

That is about 1,900 writes a day before the leader lock. On top of that, two things depend on how
agents poll (§Agents poll briefly):
- the leader lock's renewal: at most one a minute;
- syncs: one GET every 2 s while the instance's CPU runs, or about one per request when it is
  throttled between requests.

| Agents… | Writes / month (Class A, PUT, LIST) | GETs / month (Class B) | GCS (us-central1) | S3 |
|---|---|---|---|---|
| poll briefly, every 30 s, on Cloud Run (CPU only during requests) | ~100,000 | ~180,000 | 5,000 + 50,000 free: **~$0.53** | **~$0.57** |
| long-poll, or the dispatcher runs on a VM (CPU always on) | ~100,000 | ~1,300,000 | **~$1.00** | **~$1.02** |

These use the list prices: $0.005 per 1,000 writes and $0.0004 per 1,000 reads, on both clouds.
Storage stays well under 1 GB, inside GCS's 5 GB free tier.

So it costs **about $1 a month**, still not $0. GCS's always-free 5,000 writes a month
last about 1.5 days. The leader lock is the largest single source of writes (about 43%), in
both modes, since an instance stays warm all day either way. Then come heartbeats, which rewrite the lease's expiry into three objects
(task, lease, agent) on every beat. Levers, if the bill ever matters, all outside the store:
- a longer leader-lock TTL (10 minutes, renewed every 3) costs a slower takeover after a leader
  dies;
- writing a heartbeat to the lease only, and charging budgets every few minutes, makes a
  heartbeat 1–2 writes;
- renewing agent tokens hourly instead of every 10 minutes (the token lasts 24 h).

The sync interval trades read freshness for GETs. The bucket is the small part of the bill. On
Cloud Run, agents that long-poll all day keep an instance billed for CPU all day, at roughly $60
a month, far above the free 180,000 vCPU-seconds. Brief polling avoids that, at the price in
§Agents poll briefly: the leader's work stalls between requests. Hosting itself is
IMPROVEMENTS item 9's question, and so is the fallback of a small VM behind a load balancer,
which mostly costs the load balancer.

## Security

- **Nothing secret in the bucket.** The same audit as the file store applies (store.md §File
  store, "What never reaches the disk"). Its two tests (every kind audited, and a scan for
  token-shaped strings) also run over the log entries and snapshots of the conformance run. The
  new internal kinds hold:
  - a lock holder's instance id (`StoreLock`);
  - a size and a time (`TranscriptBlob`);
  - digests and link refs (`LinkObservations`);
  - the hash of a used OAuth state's id (`UsedLoginState`).
- **The bucket is private,** with public access prevented (GCS) or blocked (S3). Only the
  dispatcher's identity can read or write it. Storage is encrypted at rest by default. `serve`
  doesn't check the bucket's policy; the deployment docs (IMPROVEMENTS item 9) say how to set it.
- **Deliveries hold raw webhook payloads** (untrusted, not secret), as in P2. Their bodies are
  dropped once handled.

## Alternatives considered

| Option | For | Against |
|---|---|---|
| **A log of create-only objects, replayed into memory** (proposed) | One precondition, the same on GCS, S3 and a local dir. Memory stays the engine, so `storetest` passes as is and the global version, `List` at a version and `Watch` with `SinceVersion` keep working. P2's formats are reused. | Writes are serialised: one PUT each, a few dozen a second at most. Every instance holds the whole state in memory, as today. |
| One object per record, changed with `If-Match` / generation preconditions | Writes to different records don't contend. | No global order: `resourceVersion` becomes per object, so `ListMeta.ResourceVersion`, `SinceVersion` and informer resync need a new design, and `storetest` changes. Other dispatchers can't see changes without listing everything or adding Pub/Sub or SQS notifications. A cold start is a LIST plus a GET per record. |
| One state object rewritten with `If-Match` | Simplest. | Each write rewrites the whole state, and GCS allows one write a second per object. |
| Firestore (GCP) or DynamoDB (AWS) | Built for small records with transactions. Firestore's free quota (about 20,000 writes a day) would cover our load at $0. | Two backends, one per cloud, each with its own concurrency model to keep `storetest`-conformant. Ties the store to a provider's database. |
| Postgres (store.md's "later") | The usual multi-node answer. | No free tier on Cloud SQL or RDS. A server to run. |

## Tests

- **Conformance:** `storetest.Run` on the object store over the fake bucket and the `file://`
  bucket.
- **Two dispatchers on one bucket:**
  - concurrent CAS updates of one object from two stores: exactly one wins per version;
  - concurrent creates of one key: one gets `ErrAlreadyExists`;
  - B's watch sees A's writes in version order;
  - lock exclusion and expiry reclaim across stores;
  - a leased task heartbeated through the other store;
  - two schedulers on two stores, polling at once against a queue with `concurrency: 1`:
    exactly one task leased;
  - a `sched` holder that dies inside the section: the lock expires after its TTL, the next
    commit proceeds, and no task is leased twice. Meanwhile each pass waits for the lock at
    most once and mints at most once, and passes started within 2 s of a timeout neither wait
    nor mint;
  - an object linked on A just before B looks it up: B's intake filter keeps the event, and
    `workItemFor` finds the item rather than creating a second one;
  - a newer schema version written by A: B stops writing, reports not-ready and loses nothing.
- **Faults** (fake bucket):
  - lost races at every step;
  - a create that timed out but landed, and one that didn't;
  - a create the SDK retried after its first attempt landed ("taken" with the write's own
    nonce): committed once, no `ErrConflict`, a BudgetCounter charged once;
  - an Unlock whose write fails: logged, the record expires, and the sweeper removes it;
  - GET and LIST errors during sync;
  - a writer paused past `MaxLag` reloads;
  - a stale writer never re-creates a deleted entry (fake clock across `SegmentRetention`).
- **Compaction and cold start:** load from every combination of snapshot and log tail, with
  snapshots and entries deleted under the retention rules mid-load.
- **Secrets:** the file store's two tests, run over the bucket's objects.
- **Real buckets:** opt-in tests against `fake-gcs-server` and MinIO in Docker
  (`make e2e-objectstore`). CI runs them as a non-required job, like `e2e-run-container`.
- **Intake filter** (provider controller, every store):
  - an event `CouldFire` rejects, about an object with no work item, stores no Event or
    EventStatus, and its delivery is deleted at once;
  - an event the factory caused (`actor.isFactory`), arriving before its link is recorded, is
    stored, and the rule controller finds the item once the link exists;
  - a delivery of an unmapped type (no events) is deleted at once too;
  - an event about a linked object is stored even when no rule's `on` matches it;
  - an event `CouldFire` accepts is stored even when its `where` would be false, and one outside
    a rule's `providers`, branches or channels is dropped;
  - a dropped event is logged at Info with its reason, and counted in
    `factory_events_dropped_total`;
  - a `DeliveryFailed` delivery keeps the delivery retention;
  - a redelivery after a rule was added is stored and evaluated;
  - `factory rules test` fixtures are unchanged.
- **Brief polling** (`internal/agent/worker`):
  - with `wait: 0s`, a 204 sleeps the interval, and a lease polls again at once;
  - the defaults keep today's long poll;
  - `agent.yaml` rejects a `wait` outside `0s`–`60s` or not in whole seconds (`500ms`, `1.5s`), and a negative interval;
  - `poll: {wait: 0s}` sends `wait=0s` on the wire, and an absent `wait` sends `wait=30s`;
  - against a real dispatcher, a task created while the agent sleeps is leased on its next
    poll.
- **No rebuild:**
  - `serve` starts without calling `Conn.List`;
  - `serve` refuses to start with no `--state-dir`, `--state-url`, `--dev-fake-providers` or
    `--state-memory`;
  - the three old-marker cases: the same id takes over the marker, a different id writes
    beside it and leaves the old marker's labels, and another App's marker is ignored.
- **`serve`:** two `serve` processes on one `file://` bucket:
  - a webhook taken by one leads to a task leased and completed through the other;
  - one leader at a time, and the second takes over when the first stops;
  - a UI login started on one completes on the other (signed OAuth state);
  - a `/stream` resumed on the lagging one with the other's `Last-Event-ID` resumes;
  - a change made on GitHub while the first was leader is reported by the second's first
    resync (stored baselines);
  - `--state-url file://` on a P2 dir, and `--state-dir` on an object-store dir, are refused.

## Rollout

These start after the org move (IMPROVEMENTS item 4) is merged, since every file imports the
module path. There are seven PRs, each through lisa and ccf-review:

1. **`internal/store/objectstore`**. It ships in two parts for review size: first the shared
   refactor (P2's encoding in `internal/store/journal`, the secrets audit in
   `internal/store/storeaudit`, memory's apply-from-log and conditional delete, and the store
   interfaces), then the object store on it:
   - the log store, sync (including the sync-on-empty lookup), schema versions, locks,
     transcripts, compaction and load;
   - the `Bucket` interface with the fake and `file://` buckets, and the guard against a P2 dir
     (and the file store's guard against an object-store dir);
   - memory's apply-from-log and conditional delete;
   - the conformance, two-store, fault and secrets tests;
   - `StoreLock` and `TranscriptBlob` added to `internalKinds()` in `cmd/factory/serve_store.go`,
     or `TestServeStoreKinds` fails, which also registers them with the file store's kind audit;
   - store.md §Object store.
2. **GCS and S3 buckets**, with the opt-in emulator tests and their CI job (RT, plus a non-required
   CI job).
3. **The intake filter** (RT):
   - the provider controller's drop: the `eventLink` lookup (synced when empty) plus
     `rules.CouldFire`, as `workItemFor` makes them, and the events the factory caused kept;
   - the immediate deletion of a delivery left with no stored event, while failed deliveries
     keep their retention;
   - the Info log and `factory_events_dropped_total`;
   - its tests;
   - controllers.md §provider, README §6 and §7, and `factory state events --help`.
4. **Contract: readiness and a store that stopped writing** (FND; AGENTS.md: a contract change
   goes in its own PR, merged first):
   - `/readyz`'s description today says "The catalog is loaded and the startup rebuild is done".
     It becomes §Two releases on one log's definition: 200 once the catalog and the store are
     loaded and the first sync has succeeded; 503 when the store has stopped writing or its
     last sync is older than `MaxLag`. That holds on every instance, leader or not.
   - Every operation that writes gets the existing `ServiceUnavailable` response where it lacks
     one, with a new problem code for a store that stopped writing.
   - Web's regenerated client (`npm run gen:api`), api-server.md and contracts.md.
5. **`serve --state-url`** (RT):
   - the leader lock, and which loops run where;
   - the recovery at each instance's start, as today;
   - per-process state:
     - the signed OAuth state, single-use through `UsedLoginState`;
     - `FACTORY_SESSION_KEY` required with `--state-url`, and `FACTORY_SESSION_KEY_PREVIOUS`
       for rotation;
     - `/stream`'s resume;
     - the resync's stored baselines (`LinkObservations`, with their deletion), and the new
       leader's jittered first resync;
   - `/readyz`, `factory_ready` and the 503 on writes once the store stops writing (PR 4's
     contract);
   - the new internal kinds (`LinkObservations`, `UsedLoginState`) in `internalKinds()`,
     `fixtures_test.go`'s audit and store.md, with `ReadableSchema` and then `SchemaVersion`
     bumped as §Two releases on one log says;
   - the work-item rebuild removed: `wic.Rebuild`, its call, and `/readyz`'s wait;
   - a durable store required, unless `--dev-fake-providers` or `--state-memory`;
   - the scheduler's `sched` lock around lease commits, `errSchedBusy` and its busy-until;
   - the audit of the leader-only loops' store writes and provider calls;
   - `factory state --state-url` (read-only, as `--state-dir` is);
   - the two-`serve` test;
   - **tests it changes:**
     - `TestRestartRebuildsFromProjections` goes;
     - `TestServeStateDirRestart` loses its "work items rebuilt" assertion;
     - `TestServeMemoryStore` asserts the new message;
     - `TestBeginLoginBounds` changes with `MaxPendingLogins`' removal, and `authneg_test.go`'s
       "a state used twice" keeps passing through `UsedLoginState`;
     - every `TestParseServeConfig` case that checks a config with no state flag gets
       `--state-memory`, not only "defaults". So do `TestServeGitHubEndToEnd` and
       `TestServeGitHubProviderNeedsSecrets`, whose expected error must still be the one it
       reports, not the refusal;
     - the RT and cmd tests that read `Observation.Projections` (`rule_test.go`,
       `serve_store_test.go`) move to the fake's projection log (`gh.Projections()`), so that
       PR 6 compiles without touching RT's files;
   - the docs:
     - store.md §Serving (no rebuild), controllers.md (the work-item controller without
       Rebuild), scheduler-and-pools.md (the `sched` lock);
     - 01-context-and-goals.md, 02-architecture.md, 04-flows.md, 08-roadmap.md,
       workstreams/definitions.md, and ADR-0001's body, where they describe the rebuild;
     - deploy/poc/README.md (§6, the reset);
   - `factory state reset`'s prompt and output, and its golden file (Decision 7);
6. **The provider side of the rebuild's removal** (FND for the `provider.Conn` interface and the
   fake, GH for the GitHub adapter), after PR 5:
   - `Conn.List` (interface, `github/list.go`, fakes);
   - `Observe`'s marker parsing (`Observation.Projections`, `projectionsIn`, and the sentence
     about it in `provider.go`);
   - the fake keeps its projections in its own accessor (`fake/effects.go`, `fake/observe.go`);
   - the tests: `fake_test.go`, `github/observe_test.go`, and `github/project_test.go`, which
     checks Project's output through Observe's projections and moves to the marker parser;
   - github-provider.md and foundation's contracts.md (`List`, the projection round-trip, the
     fake's records).
7. **Brief polling in `supervise`** (AGT), independent of PRs 1–6:
   - `agent.yaml`'s `poll` block, with its checks (whole seconds, no hot loop);
   - the worker's sleep after a 204, its poll settings (a zero wait stays zero, and the 30 s
     default applies only when `poll.wait` is absent), the relaxed wait check, and the worker
     tests that change with it;
   - `deploy/poc/agent.yaml.example` with the block commented out, and the poll type added to
     the deploykit test's list of walked types;
   - supervise.md.

Deploying to Cloud Run or Lambda, the tick endpoint and the bucket policy are IMPROVEMENTS item 9.

## Decisions (Gustavo, 2026-10-27)

1. **Cost:** a small bill is fine. The cloud instance runs the docs and workflows catalog only,
   with at most 10–20 webhooks a day that matter (§What it costs: about $1 a month).
2. **A fresh start:** the cloud instance starts with an empty bucket and a new catalog in its own
   repo (IMPROVEMENTS item 8). Nothing is pushed from the workstation's state dir, so no
   `factory state push`.
3. **No rebuild from markers:** with durable state and stored events, the work-item rebuild is
   removed (§No rebuild from markers).
4. **Store only the events that can matter:** an event no rule could fire for, about an object
   with no work item, is dropped at intake (§Only events that can matter are stored).
5. **No webhook-recovery change:** a failed live delivery stays a known limit (§Several
   dispatchers, the recovery bullet). If cold starts make it bite, the dispatcher runs on a VM
   behind a load balancer instead.
6. **Agents poll briefly** when the dispatcher runs where idle time is free (§Agents poll
   briefly), as an agent PR in this rollout. On Cloud Run with CPU only during requests, the
   leader's work then advances only during requests. How to close that (work inside requests,
   always-allocated CPU, or a VM) is IMPROVEMENTS item 9's choice.
7. **Nothing comes back after lost state:** after a reset, a lost bucket or dir, or a corrupt dir
   that is reset, open items start fresh, run counters and the run cap included. No events
   survive to replay. People redeliver webhooks from the App's delivery log, reopen issues or
   re-apply labels to recover what they want (§No rebuild from markers). The reset's messages
   say so.

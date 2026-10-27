# P3: The dispatcher keeps its state in object storage

> Status: **proposed**. Workstream: RT (dispatcher runtime). Builds on
> [P2](P2-dispatcher-state-file.md) (its journal and snapshot formats) and
> [ADR-0009](../adr/0009-dispatcher-state-is-a-log-in-object-storage.md). Replaces nothing in
> the contracts. Touches `internal/store`, `cmd/factory serve`, `factory state`, the provider
> controller's intake and `factory-agent supervise`'s polling, and removes the work-item rebuild
> from markers.
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
  transcripts/<run>.jsonl.gz        one blob per transcript or environment log (P2's naming)
```

- **Log entries** are `{"v","at","op","kind","name","obj"?,"w"}`. This is P2's journal line plus
  `w`, the writer's instance id and a per-write nonce. The version `v` is the object's name, and
  versions are contiguous. An entry is about 1–2 KB.
- **Snapshots** are named by the version they hold. They are written by the leader (§Several
  dispatchers) and are never overwritten.
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
- **Unknown outcome** (a timeout or a 5xx after the request was sent): the hook reads `log/<v>`
  and goes on as for "taken". If it is missing, the create is retried, which is safe because it
  is create-if-absent. Each request has a 5 s deadline.

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
Decisions that read many objects and write a different one are not covered by a CAS. The
scheduler's lease matching is the one such decision, and it gets a lock (§Locks and leases).

### Watches

Watches are memory's: changes applied from the log (from a sync or a lost race) fire watch events
exactly like local writes, in version order. A watcher on B sees A's writes in order, one sync
interval late at most while syncs run. `SinceVersion` replays memory's ring, as before. After a
cold start the ring begins at the loaded version, and an older `SinceVersion` is
`ErrWatchExpired` (the informer resyncs), as after a file-store restart.

### Locks and leases

`Lock` is the only part of the `Store` interface that memory keeps per process today. In the
object store a lock is a stored record of an internal kind, `StoreLock`, named after the lock.
It holds `{holder, token, expiresAt}` on the store's clock and is written through the log like
any other object:

- **Lock** creates the record. If the record exists and has expired, Lock takes it with a CAS
  update. While another holder has it, Lock waits, syncing every second until the lock is free
  or expired, or until `ctx` ends.
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

Each blob (a transcript or an environment log) is an object at `transcripts/<run>.jsonl.gz`,
with P2's escaping for unsafe names. It is written with a plain PUT, because a re-put replaces
it, and read with a GET.

The caps and retention need sizes and creation times on the store's clock. Listing can't give
these (S3's listing has no custom metadata), so each blob gets an index record of an internal kind,
`TranscriptBlob`, `{size, putAt}`, written through the log. `putAt` is set on every put, a replace
included, on the store's clock. `createdAt` can't serve, because an Update keeps it, and memory's
transcripts count a replaced blob as new (storetest's `TotalCapEvictsOldest`):
- **The 2 GB total cap and oldest-first eviction** come from these records, read from memory:
  the oldest `putAt` goes first, and on a tie the lower resourceVersion goes first.
- **Retention** sweeps the records by `putAt` and deletes their blobs.
- **A Run's delete** removes its records and blobs, after the Run's own delete commits.

A crash between the blob and its record leaves an orphan blob. Once a day the leader lists
`transcripts/` and deletes blobs that have had no record for a day.

### Snapshots, cold start and log retention

- **Compaction** runs on the leader when the log is 1,000 entries past the last snapshot, and
  when the leader stops. It writes `snapshots/<V>.json.gz` from `memory.Snapshot` at version V.
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
- **The leader only** runs every loop that writes from a watch or a timer: the controllers
  (provider, rule, debounce, work-item, run), the scheduler's lease controller and its
  `Refresh`, the sweeper (lock records included), compaction, the transcript orphan scan, and
  the App installation check.
- **The provider's delivery recovery** (`pc.Recover`) runs when an instance becomes leader, with
  `since` set to that moment, not on every cold start. Today `since` is the process start. There
  is no other start-up work: the work-item rebuild from markers is removed (§No rebuild from
  markers).

  **Known limit (Decision 5):** a webhook is safe once its Delivery is stored and GitHub has its
  2xx. GitHub doesn't retry a failed delivery, though, and `Recover` covers only the window from
  becoming leader. So a live delivery that fails is lost unless someone redelivers it from the
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
  `factory.workitem.resynced` for changes made on GitHub while no dispatcher was watching.
- **Recovery is replay.** Events that matter are stored (90 days, `--retention`), so an event
  handled wrongly is replayed (`POST /events/{id}/replay`) rather than reconstructed from
  markers.
- **What is lost** is rebuilding from GitHub after the state itself is lost (a deleted bucket or
  dir). Open items would then start fresh on their next event. Bucket soft delete (GCS's default
  of 7 days) or S3 versioning covers an accidental delete instead.
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

Each dropped event is logged with its type, its object and why it was dropped. A dropped event
caused by the factory itself (`actor.isFactory`) is logged at Info, as `RuleController.skipped`
logs it today. Every other is logged at debug. That log is where "why didn't my rule fire?" starts
for an event that isn't stored.

**A delivery left with no stored event is deleted at once,** whether its events were dropped or
it had none (an unmapped type). It isn't kept for the delivery retention. The create keeps it
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
    Put(ctx context.Context, name string, data []byte) error           // unconditional (transcript blobs)
    List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error // name order
    Delete(ctx context.Context, name string) error                     // ErrNotExist
}
```

| Bucket | `Create` | Credentials |
|---|---|---|
| `gs://` (`cloud.google.com/go/storage`) | `ifGenerationMatch=0` | Application Default Credentials (Cloud Run's service account) |
| `s3://` (`github.com/aws/aws-sdk-go-v2/service/s3`) | `If-None-Match: *` (S3 since Aug 2024) | the default chain (Lambda's role) |
| `file://` (a local directory) | `O_CREAT\|O_EXCL`, then fsync | none. For development and for tests with two dispatchers on one machine |
| fake (tests) | in memory, with injected failures and lost races | none |

`If-Match` and `ifGenerationMatch=<n>` are not needed: no object is ever updated in place. Only
transcript blobs, which have no concurrent writers that matter, are overwritten. GCS's limit of
one write per second per object name doesn't apply either, since each name is written once.

The two SDKs are new dependencies (the repo-layout LLD's allow-list): they get a justification in
their PR. Signing S3 requests (SigV4) and finding credentials on each platform are not worth
writing ourselves. Credentials always come from the platform's identity, never from a key in the
config or the image.

`serve` takes `--state-url` or `--state-dir` (not both). Without either, the store stays in
memory.

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
  new internal kinds hold a lock holder's instance id (`StoreLock`) and a size (`TranscriptBlob`).
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
  - a delivery of an unmapped type (no events) is deleted at once too;
  - an event about a linked object is stored even when no rule's `on` matches it;
  - an event `CouldFire` accepts is stored even when its `where` would be false, and one outside
    a rule's `providers`, branches or channels is dropped;
  - a dropped event is logged at debug with its reason, and at Info when the factory caused it;
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
- **`serve`:** two `serve` processes on one `file://` bucket. A webhook taken by one leads to a
  task leased and completed through the other, with one leader at a time, and the second takes
  over when the first stops.

## Rollout

These start after the org move (IMPROVEMENTS item 4) is merged, since every file imports the
module path. There are six PRs, each through lisa and ccf-review:

1. **`internal/store/objectstore`**:
   - the log store, sync, locks, transcripts, compaction and load;
   - the `Bucket` interface with the fake and `file://` buckets;
   - memory's apply-from-log and conditional delete;
   - the conformance, two-store, fault and secrets tests;
   - store.md §Object store.
2. **GCS and S3 buckets**, with the opt-in emulator tests and their CI job (RT, plus a non-required
   CI job).
3. **The intake filter** (RT):
   - the provider controller's drop: the `eventLink` lookup plus `rules.CouldFire`, as
     `workItemFor` makes them;
   - the immediate deletion of a delivery left with no stored event;
   - its tests;
   - controllers.md §provider.
4. **`serve --state-url`**:
   - the leader lock, and which loops run where;
   - the recovery on becoming leader;
   - the work-item rebuild removed: `wic.Rebuild`, its call, and `/readyz`'s wait;
   - a durable store required, unless `--dev-fake-providers` or `--state-memory`. The serve
     tests that rely on the memory default get `--state-memory`: `TestServeGitHubEndToEnd`,
     `TestServeGitHubProviderNeedsSecrets` (its expected error must still be the one it
     reports, not the refusal) and the "defaults" config case in serve_test.go;
   - the scheduler's `sched` lock around lease commits, `errSchedBusy` and its busy-until;
   - the audit of the leader-only loops' store writes and provider calls;
   - `factory state --state-url` (read-only, as `--state-dir` is);
   - the two-`serve` test;
   - the docs:
     - store.md §Serving (no rebuild), controllers.md (the work-item controller without
       Rebuild), scheduler-and-pools.md (the `sched` lock), api-server.md;
     - 01-context-and-goals.md and 02-architecture.md, where they describe the rebuild;
     - deploy/poc/README.md.
5. **The provider side of the rebuild's removal** (FND for the `provider.Conn` interface and the
   fake, GH for the GitHub adapter), after PR 4:
   - `Conn.List` (interface, `github/list.go`, fakes) and its tests;
   - `Observe`'s marker parsing (`Observation.Projections`, `projectionsIn`, and the sentence
     about it in `provider.go`);
   - github-provider.md.
6. **Brief polling in `supervise`** (AGT), independent of PRs 1–5:
   - `agent.yaml`'s `poll` block;
   - the worker's sleep after a 204, its poll settings (a zero wait stays zero; the 30 s default
     only when `poll.wait` is absent), and the relaxed wait check;
   - `deploy/poc/agent.yaml.example`, and the poll type added to the deploykit test's list of
     walked types;
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

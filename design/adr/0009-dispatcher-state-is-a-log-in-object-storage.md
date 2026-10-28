# ADR-0009: The dispatcher's shared state is a log of create-only objects in a bucket

- **Status:** proposed
- **Date:** 2026-10-27

## Context
The dispatcher is going to run on Cloud Run or Lambda: no local disk, several instances at once,
and scale to zero. P2's file store gives one dispatcher durable state in a local dir, behind an
exclusive flock. The rest of the dispatcher was built on the store's semantics:
- a global, monotonic `resourceVersion`;
- CAS on it;
- `List` at a version;
- `Watch` with `SinceVersion`, which informers use to resync;
- leased locks.

Object storage (GCS, S3) offers durable objects and per-object preconditions. It has no
transactions, no change feed and no order across objects.

## Decision
- The state in a bucket is an **append-only log**. Each change is one object, `log/<version>`,
  created only if absent (GCS `ifGenerationMatch=0`, S3 `If-None-Match: *`). Whoever creates
  version N has committed it. There are periodic **snapshots**, `snapshots/<version>`, in the
  file store's formats.
- **Every dispatcher replays the log into the memory engine** and serves reads from memory. A
  write is checked against memory and then committed by creating the next log object. A lost
  race reads the entries it missed and checks the write again. Creating N proves the writer saw
  everything before N, so CAS holds across dispatchers.
- **Locks become stored records** written through the log, so they hold across dispatchers.
  The scheduler's lease commits, which check limits across many objects, serialise on one
  such lock.
- **One leader**, chosen with a store lock, runs the loops that write from watches and timers.
  Every instance serves requests.
- **The only precondition used is "create if absent".** Nothing is updated in place or
  overwritten: each transcript put has its own blob name.
- Design and numbers: [P3](../proposals/P3-dispatcher-state-in-object-storage.md).

## Consequences
- The `Store` interface and `storetest` don't change.
- What changes around it:
  - the scheduler's lease commits take a store lock;
  - the work-item rebuild from markers is removed: the stored state is the authority, and
    stored events are replayed instead. This supersedes ADR-0001's start-up rebuild and
    `provider.Conn.List`, and `serve` needs a durable store (or an explicit in-memory flag);
  - only events a rule could fire for (`rules.CouldFire`), or that concern a linked object, are
    stored;
  - agents can poll briefly instead of long-polling, so a dispatcher on Cloud Run or Lambda
    isn't kept busy between tasks. With CPU only during requests, the leader's loops then
    advance only during requests (IMPROVEMENTS item 9 closes that);
  - a webhook whose live delivery fails is not recovered automatically (a known limit).
- One contract change: `/readyz` no longer waits for a rebuild (the store is loaded and synced).
- Every log entry carries a schema version, and stored-type changes ship expand-then-write over
  two releases, so two releases can share the log during a deploy or a rollback. An instance
  that meets a version it can't read stops writing.
- Per-process state that a client's next request may miss moves to the store or into signed
  tokens: the OAuth state, `/stream`'s resume and the resync's baselines.
- Writes are serialised through one log, one PUT each (tens per second at most): enough for the
  factory, not for a high-volume service.
- Each instance holds the whole state in memory, as today. Reads may lag other instances by the
  sync interval. A CAS write never acts on stale state, and a lookup whose empty answer leads to
  a create or a drop syncs first.
- Lost state (a reset, a lost bucket or dir) means open items start fresh, run counters and the
  run cap included. Recovery is redelivering webhooks, reopening issues and re-applying labels.
- Requests cost money: about $1 a month at our load (docs and workflows), above GCS's
  always-free operations.
- Old log entries may be deleted only under a time rule (covered by a snapshot and older than
  7 days, with writers reloading after an hour's lag). Bucket lifecycle rules must not touch
  the log.
- The same code runs on GCS, S3 and a local directory, so two dispatchers can be tested on one
  machine.

## Alternatives considered
- **One object per record with `If-Match` / generation preconditions:** there is no global order,
  so `resourceVersion`, `SinceVersion` and resync would change. Seeing other instances' changes
  would need full listings or bucket notifications (Pub/Sub, SQS).
- **Firestore or DynamoDB:** a better fit for small transactional records, and Firestore's free
  quota covers our load. But it means one backend per cloud, each with its own consistency model
  to keep conformant.
- **Postgres:** no free tier, and a server to run.

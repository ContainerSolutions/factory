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
- **The only precondition used is "create if absent".** Nothing is updated in place, except
  transcript blobs, which a plain PUT replaces.
- Design and numbers: [P3](../proposals/P3-dispatcher-state-in-object-storage.md).

## Consequences
- The `Store` interface and `storetest` don't change. The controllers, the scheduler and the
  API keep their semantics.
- Writes are serialised through one log, one PUT each (tens per second at most): enough for the
  factory, not for a high-volume service.
- Each instance holds the whole state in memory, as today. Reads may lag other instances by the
  sync interval; writes never act on stale state.
- Requests cost money: about $2–3 a month at our load, above GCS's always-free operations.
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

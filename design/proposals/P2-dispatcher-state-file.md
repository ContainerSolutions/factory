# P2: The dispatcher keeps its state in a file

> Status: **accepted** (Gustavo, 2026-10-21; decisions at the end). Workstream: RT (dispatcher runtime).
> Replaces nothing in the contracts. Touches `internal/store`, `cmd/factory serve`, and a new
> `factory state` CLI.

## Why

The dispatcher's store is in memory only (store.md §In-memory implementation). The live runs
on 20–21 Oct showed what that costs:

- **Every restart loses all tasks, runs, transcripts, effects and decisions.** The work items
  come back from GitHub's marker comments, but nothing else does. After a restart there was no
  task to redispatch (the mock-docs#4 epic), so we had to hand-craft a `factory.run.completed`
  event from the marker to restart the stack.
- **A failure can't be investigated after the fact.** Run summaries and transcripts are reachable
  only through the API, with the CLI token, and only until the next restart. Diagnosing each live
  failure meant scraping the JSON log for request lines.
- **Fixes need restarts.** Every fix we shipped needed a dispatcher restart to deploy, and each
  restart erased the evidence for the next issue.

## Goal

1. **Restart without losing state:** `factory serve --state-dir <dir>` loads everything from disk
   and continues where it stopped. Tasks, runs, effects, events, deliveries, decisions, budget
   counters, agents, leases, sessions and transcripts all survive.
2. **State you can read without the API:** plain files you can `jq` and `grep`, plus a small
   `factory state` CLI for the common questions.
3. **No new dependency and no new semantics:** the memory store stays the engine and keeps
   passing `storetest`.

Non-goals: HA, several dispatchers sharing one state, Postgres. store.md's "Postgres later"
stays the durable multi-node path.

## Design: snapshot plus journal on the memory store

`internal/store/filestore` wraps `store/memory` and implements the same `Store`, `Sweeper` and
`TranscriptStore` interfaces.

```
<state-dir>/                  0700, owned by the dispatcher's user
  snapshot.json               the whole store at journal offset N (atomic rename on write)
  journal.jsonl               one line per change since the snapshot, fsync'd
  transcripts/<run>.jsonl.gz  one file per run (already gzip'd by the agent)
  LOCK                        flock: one dispatcher per state dir
```

- **Journal lines** are one per committed change, in store-version order:
  ```json
  {"v":"1042","at":"2026-10-21T11:23:39Z","op":"update","kind":"Task","name":"tsk_…","obj":{…}}
  {"v":"1043","at":"…","op":"delete","kind":"Lease","name":"lse_…"}
  ```
  `v` is the store's ResourceVersion counter. `obj` is the stored object exactly as the memory
  store keeps it (JSON already, since it deep-copies by JSON round-trip). Ops are create, update
  and delete. Locks are not journaled: they are advisory and lease-based, and expire on restart.
- **Write path:** a write is applied to memory, then appended to the journal before `Create`,
  `Update` or `Delete` returns. A journal write error fails the call; the memory change is rolled
  back under the same lock, so memory and disk never disagree. Group commit (one fsync for
  everything appended within ~5 ms) keeps webhook bursts cheap. At PoC scale (thousands of
  objects, a few writes a second) a plain fsync per write is also fine; start with that and add
  group commit if a benchmark asks for it.
- **Compaction:** when the journal passes 64 MB or 50,000 lines, and at shutdown, write
  `snapshot.json.tmp` (the whole store plus the version counter), fsync it, rename it over
  `snapshot.json`, then truncate the journal. A crash at any point leaves either the old snapshot
  plus the full journal, or the new snapshot plus a journal whose lines at or below its version
  are skipped on load.
- **Load:** at start, read `snapshot.json`, replay `journal.jsonl` lines above the snapshot's
  version, and restore the version counter. A torn last line (a crash mid-append) is dropped with
  a warning; corruption anywhere else stops the start with the line number. Then the existing
  start-up runs as today:
  - **The work-item rebuild from markers still runs.** The file is authoritative for runtime
    state. The rebuild adds links and state that changed on GitHub while the dispatcher was down,
    using the same merge as today's rebuild, so markers win for projected state keys.
  - **Leases** load as they were. The scheduler's expiry handling (expiry.go) treats one past its
    TTL as lost, as it does for an agent that stops heartbeating. An agent that is still running
    keeps heartbeating against the restarted dispatcher, so a run that outlives the restart can
    finish, which the in-memory store can't do today.
  - **Watches** start from the restored version. Controllers resync from List, as they do after
    `ErrWatchExpired`.
- **Transcripts** move from the in-memory blob map to `transcripts/`. The caps (20 MB each,
  2 GB total, oldest evicted first) stay.
- **Retention** is unchanged: the sweeper's deletes are journaled like any other.

### What never reaches the disk

Today no stored kind carries a credential:
- agent tokens are kept as `sha256:` hashes (scheduler/register.go);
- UI sessions are named by the hash of the session ID;
- git credentials are minted per lease and never stored (scheduler LLD: "never persisted").

The state file inherits that property and must keep it:
- a test lists every kind the store accepts and fails when a new one appears without an audit
  note;
- a scanner test asserts no stored object in the conformance run contains a token-shaped string
  (`ghs_`, `gho_`, `sk-ant-`, …).

`Delivery` holds raw webhook payloads. They're untrusted, not secret; retention bounds them.

The state dir is 0700 and its files 0600. `serve` refuses a state dir that others can read, as
run.sh does for `.env`.

## Troubleshooting: `factory state`

The CLI reads the state dir directly. It takes a shared flock, so it works while `serve` runs
and against a copy of a stopped dispatcher's dir.

```
factory state tasks [--phase Failed] [--spec implement] [--workitem wi_…]
factory state task tsk_…              # spec, status, runs, Result summary, decisions that made it
factory state run run_…  [--transcript]   # Result, progress events, transcript (gunzipped) to stdout
factory state workitem wi_…           # links, state, decisions, tasks
factory state events [--since 1h] [--type pr.opened]
factory state log [--since 10m]       # the journal as a readable timeline: version, time, op, kind, name, phase changes
```

`jq` on `snapshot.json` and `journal.jsonl` works too, and LOCAL-GUIDE's troubleshooting table
gets a "where to look" column pointing at these commands.

## `serve` and the run kit

- `factory serve --state-dir DIR` turns the file store on. Without the flag the store stays
  in memory, as today; tests and `--dev-fake-providers` keep that default.
- `deploy/poc/run.sh` passes `--state-dir "${FACTORY_STATE_DIR:-$HOME/.factory/state}"`.
  `.env.example` documents the variable.
- **`factory state reset`** (with `--yes`) moves the state dir aside to `<dir>.<timestamp>`, for a
  clean start that keeps the evidence.
- **Format versioning:** `snapshot.json` carries `{"format": 1, "storeVersion": …}`. A dispatcher
  that finds an unknown format refuses to start, and says how to move the dir aside.

## Alternatives considered

| Option | For | Against |
|---|---|---|
| **Snapshot + JSONL journal on the memory store** (proposed) | No dependency. Memory semantics, so `storetest` passes as is. The files are readable with `jq`. | Compaction and crash handling are ours to get right (they're small and testable). |
| SQLite (modernc.org/sqlite, pure Go) | Queryable with `sqlite3`. Durable. | A new, large dependency (the LLD allow-list). A second implementation of watch, CAS and indexes to keep conformant. |
| bbolt (go.etcd.io/bbolt) | Small, pure Go, transactional. | Binary file: you can't read it without our CLI. Also a second implementation. |
| Postgres (store.md's "later") | The real multi-node answer. | One more process to run on a laptop. Overkill for the PoC. |

## Tests

- **Conformance:** `storetest.Run` against the file store, where each subject is opened on a
  fresh temp dir.
- **Crash and restart:** write N objects and kill (simulated by truncating the journal at random
  byte offsets), then reopen. The state is the last fully journaled version, nothing later, and
  never a half-applied object.
- **Compaction:** across a compaction boundary, with a crash injected at each step (tmp written,
  renamed, journal truncated).
- **Restart continuity:** a run whose agent keeps heartbeating across a dispatcher restart
  completes, and its Result and transcript are readable after.
- **Secrets:** the two tests above (the kind audit and the token-shaped string scan).
- **CLI:** `factory state` golden outputs on a fixture state dir.

## Rollout

Three PRs, each through lisa and ccf-review:
1. `internal/store/filestore` with conformance, crash and compaction tests (RT).
2. `serve --state-dir`, run.sh and `.env.example`, and the transcript move (RT).
3. `factory state` CLI and LOCAL-GUIDE updates (RT and docs).

The LLD update (store.md §File store) ships in PR 1.

## Decisions (Gustavo, 2026-10-21)

1. **On by default in the run kit:** run.sh passes `--state-dir`, defaulting to
   `~/.factory/state`. `factory serve` without the flag stays in memory, for tests and
   `--dev-fake-providers`.
2. **Keep the journal:** snapshot plus fsync'd journal, so a crash loses nothing already
   journaled.

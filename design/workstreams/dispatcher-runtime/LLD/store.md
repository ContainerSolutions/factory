# LLD — Resource store (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Task: RT-S1-T01 · Package: `internal/store` (+ `store/memory`, `store/filestore`, `store/storetest`)

## Interface

```go
type Object = v1alpha1.Object // GetTypeMeta().Kind, GetName(); metadata through accessors (below)
type Key struct { Kind, Name string }

type Store interface {
    Get(ctx context.Context, k Key, out Object) error                                  // ErrNotFound
    List(ctx context.Context, kind string, opts ListOptions, out any) (ListMeta, error) // out: *[]T, *[]*T or *[]Object
    Create(ctx context.Context, obj Object) error                                       // ErrAlreadyExists; sets ResourceVersion, CreatedAt
    Update(ctx context.Context, obj Object) error                                       // CAS on ResourceVersion → ErrConflict
    Delete(ctx context.Context, k Key) error                                            // ErrNotFound
    Watch(ctx context.Context, kind string, opts WatchOptions) (Watcher, error)
    Lock(ctx context.Context, name string, ttl time.Duration) (Unlock, error)           // advisory, lease-based; waits until free or ctx ends
}

type ListOptions struct { LabelSelector *v1alpha1.LabelSelector; Field map[string]string; Cursor string; Limit int }
type ListMeta struct { NextCursor string; ResourceVersion string } // NextCursor empty on the last page
type WatchOptions struct { LabelSelector *v1alpha1.LabelSelector; SinceVersion string; Buffer int }
type Watcher interface {
    Events() <-chan WatchEvent   // closed when the watch ends (ctx done, Stop, or expiry)
    Err() error                  // after Events() is closed: nil, context error, or ErrWatchExpired
    Stop()
}
type WatchEvent struct { Type WatchEventType /* added|modified|deleted */; Object Object; Version string }
type Unlock interface {
    Unlock()                                            // idempotent; never releases a lock someone else reclaimed
    Renew(ctx context.Context, ttl time.Duration) error // ErrLockLost once expired or released
}

type Sweeper interface { Sweep(ctx context.Context) (SweepResult, error) }
type TranscriptStore interface {
    PutTranscript(ctx context.Context, run string, blob []byte) error // ErrTooLarge over 20 MB
    GetTranscript(ctx context.Context, run string) ([]byte, error)   // ErrNotFound
}
```

Errors (`errors.Is`; most come as `*store.Error{Op, Key, Detail, Err}`): `ErrNotFound`, `ErrAlreadyExists`, `ErrConflict`, `ErrInvalid`, `ErrWatchExpired`, `ErrLockLost`, `ErrTooLarge`. Accessors: `ResourceVersion`, `SetResourceVersion`, `CreatedAt`, `SetCreatedAt`, `HasResourceVersion`, `Labels`; internal kinds outside v1alpha1 implement `MetaObject{GetObjectMeta() *v1alpha1.ObjectMeta}`. Label selectors use `v1alpha1.LabelSelector.Matches`/`Validate`, shared with the catalog.

Kinds stored: `Delivery` (raw provider webhook, pre-normalisation), `Event`, `WorkItem`, `Task`, `Run`, `Agent`, `Effect`, `BudgetCounter`, `Lease` (internal), `Debounce` (internal), `Session` (UI, internal). Config kinds live in the `Catalog` snapshot, **not** in the store.

## In-memory implementation

- One `map[kind]map[name]stored` guarded by a single `sync.RWMutex` (PoC scale: thousands of objects).
- `ResourceVersion` = global monotonic counter (`uint64`, decimal string).
- Objects are **deep-copied** on write and read (JSON round-trip helper; generated `DeepCopy` later if needed).
- **Field indexes** (maintained on write): `Task` by `status.phase`, `spec.queue`, `metadata.workItem`; `Run` by `task`; `WorkItem` by every `links[].provider+ref`; `Effect` by `idempotencyKey`.
- **Watch:** fan-out to subscriber channels with a bounded buffer (default 256). Sends never block the writer: if a subscriber's buffer is full, the store sets its `Err()` to `ErrWatchExpired` and **closes its channel**. Closing always succeeds, so expiry is always observable. Consumers loop until the channel closes, then check `Err()`; on `ErrWatchExpired` the controller runtime re-lists (resync) and starts a new watch. `SinceVersion` replays from a ring buffer of the last 10 000 events; if it's too old, `Watch` returns `ErrWatchExpired` immediately.
- **Locks:** `map[name]{owner, expires}`; `Unlock` is idempotent; expired locks are reclaimable.

## Retention

A sweeper goroutine (every 10 min) deletes:

| Kind | Retention | Rule |
|---|---|---|
| `Run`, transcripts (blob map), `Event`, `Effect` | **90 days** (Q13, configurable `--retention`) | by `CreatedAt` |
| `Task` | 90 days after terminal phase | `Succeeded/Failed/Cancelled` only |
| `WorkItem` | never deleted while any link is open; 90 days after last activity otherwise | |

Transcripts are stored as compressed blobs in a separate `map[runID][]byte`, capped at 20 MB each and 2 GB total (oldest evicted first, logged).

## File store

`store/filestore` is the single-node durable store (design: proposal P2, "The dispatcher keeps its state in a file"). It wraps `store/memory`, which stays the engine: CAS, indexes, watches, locks and retention all come from it, so the file store passes `storetest` unmodified. `filestore.Open(dir, opts, ...Option)` returns a `*Store` (`Store`, `Sweeper`, `TranscriptStore`, plus `Close`).

```
<dir>/                        0700, owned by the dispatcher's user
  snapshot.json               {"format":1,"storeVersion":"N","objects":[{kind,name,v,created,at,obj}…]}
  journal.jsonl               {"v","at","op","kind","name","obj"?} per change above the snapshot
  transcripts/<run>.jsonl.gz  one file per run (a run name that isn't a safe file name: ~<base64url>.jsonl.gz)
  LOCK                        exclusive flock while open
```

- **Write path:** memory's commit hook (`memory.WithCommitHook`) is called under its write lock once a write is checked and encoded, before it is applied. The file store appends the journal line there and fsyncs it; only then does memory apply the change and tell watchers. A failed append or fsync fails the call and memory is left untouched (nothing to roll back, no watch event to retract), so memory never holds a change the journal doesn't. A failed write is rolled back by truncating the journal to the last good line; a failed fsync (or a rollback that fails) stops all writes (`ErrJournal`) until the store is reopened, since the page cache can't be trusted after it. One fsync per write; group commit would replace the fsync in `append`, not built.
- **Kinds:** loading needs each kind's Go type. The v1alpha1 runtime kinds are built in; internal kinds are registered with `filestore.WithKind(kind, proto)`. A write of an unregistered kind is `ErrInvalid`, so nothing reaches the disk that can't be loaded back.
- **Locks** are not journaled (advisory, lease-based; a restart frees them). **Watches** after a reopen start at the restored version: the history isn't persisted, so an older `SinceVersion` is `ErrWatchExpired`, as when the ring is trimmed.
- **Compaction:** when a write takes the journal past 64 MB or 50 000 lines (`WithCompaction`), on that writer's goroutine (writers that find one running don't wait), and on `Close`. The snapshot is memory at version V (`memory.Snapshot`, one read lock); writes go on meanwhile. Steps: write `snapshot.json.tmp` and fsync; rename over `snapshot.json` and fsync the dir; under the journal lock, write the lines above V to `journal.jsonl.tmp`, fsync, rename over the journal, fsync the dir. A crash after any step leaves the old snapshot and the full journal, or the new snapshot and a journal whose lines at or below V are skipped. A failed compaction is logged and retried after more lines; the write that triggered it has succeeded.
- **Load:** remove leftover tmp files; read the snapshot (an unknown `format` fails Open saying to move the dir aside); replay the journal lines above its version, each exactly one above the last (a gap is corruption); restore memory (`memory.Restore`) and the version counter. A torn last line (no trailing newline, or not a valid line) is dropped with a warning and the journal truncated to the last good line; any other bad line fails Open with `ErrCorrupt` and its line number. Creation times of kinds without metadata (Event) come from the line that created the object.
- **Modes:** dir 0700, files 0600, owned by this user. Open refuses (`ErrInsecure`, naming the `chmod`) a dir or file group or others can access, or a symlink. A second Open of a dir that is open fails with `ErrLocked`.
- **Transcripts** are files with the memory store's caps (20 MB each, 2 GB total, oldest evicted first). Each is written to a tmp file, fsync'd and renamed; its mtime is its creation time on the store's clock, so retention and eviction order survive a restart. Deleted with their Run (from the commit hook) and swept by age.

**What never reaches the disk.** The stored kinds carry no credential, and two tests keep it so: one fails when a kind reaches the disk without an audit line (in `fixtures_test.go`), one scans every byte of the snapshot and journal of the conformance run and of a fixture with a real-shaped object of every kind (strings, and base64 `[]byte` fields decoded) for `ghs_`, `gho_`, `ghp_`, `github_pat_`, `sk-ant-`, `-----BEGIN` and `x-access-token:`. The audit:

| Kind | Why it holds no secret |
|---|---|
| Event, WorkItem, Run, Effect, BudgetCounter, Approval, KillSwitch, EventStatus, Debounce | provider refs, rule state, results and counters; no credentials |
| Task | `spec.credentials` describes the git token to mint; tokens are minted per lease and never stored; `llm` is always `agent-local` |
| Agent, AgentToken | the agent token is stored as a `sha256:` hash only |
| Lease | the lease and usage; git credentials never stored, only their expiry |
| Session | named by the hash of the session id; holds the login, no GitHub token |
| Delivery | the raw webhook minus `Authorization`, `Cookie`, `Proxy-Authorization`; untrusted, not secret; body and headers dropped once handled |
| TaskPrompt | the rendered prompt: catalog templates and untrusted provider text |

## Postgres later

`store/storetest` holds a **conformance suite** (`func Run(t *testing.T, newStore func(store.Options) storetest.Subject)`) covering CAS conflicts, watch ordering, resync, selectors, locks and retention. A future Postgres store must pass the same suite. Until then the file store (§File store) is the single-node durable store; Postgres stays the multi-node path.

## Tests

- Conformance suite on `memory` under `-race`.
- Concurrency: 50 goroutines updating the same object → exactly one wins per version.
- Watch slow-consumer → channel closed, `Err() == ErrWatchExpired`, then successful resync; writer never blocks.

## Decisions taken in RT-S1-T01

- **`store.Object = v1alpha1.Object`**, not a store-specific interface: v1alpha1 kinds carry no `GetResourceVersion`/`SetResourceVersion`, so the store reaches metadata through accessors (a type switch over the runtime kinds, plus `MetaObject` for internal kinds).
- **Event** has no metadata: the store keeps its version and creation time beside the object, and `Update` on an Event is unconditional (no CAS). Every other kind's `Update` requires the stored `resourceVersion`; an empty one is `ErrConflict`.
- **`List` returns `ListMeta{NextCursor, ResourceVersion}`**: the cursor for pagination (keyset, name order, opaque), and the store version at list time, so a consumer can list then `Watch(SinceVersion: meta.ResourceVersion)` with no gap (resync).
- **`Unlock` is an interface with `Renew`.** A lease that has expired is lost: `Renew` returns `ErrLockLost` even if nobody reclaimed it, and the old holder's `Unlock` is a no-op.
- **Retention and transcripts are separate interfaces** (`Sweeper`, `TranscriptStore`), not part of `Store`; `store.RunSweeper(ctx, s, interval, log)` is the ticker helper (default 10 min).
- **Suite signature:** `storetest.Run(t, func(store.Options) storetest.Subject)`, with `Subject = Store + Sweeper + TranscriptStore`. The suite injects a fake clock and small caps, buffers and history sizes through `Options`.
- **Retention specifics:** Event by the store's creation time; Task by its last write in a terminal phase (`Succeeded/Failed/Cancelled`; there is no terminal timestamp); `BudgetCounter` swept at 90 days with Runs, Events and Effects; Agent, Approval and the internal kinds kept; a Run's transcript is deleted with the Run (and transcripts also age out on their own). WorkItems are never swept by the store: the work-item controller owns their retention (it knows whether links are open).
- **`--retention`** is wired by `factory serve` into `store.Options.Retention`; the store only takes the option.
- **Clock:** the store takes an `internal/clock.Clock` (`Real`, `Fake`). `rules.Clock` has the same shape and should converge on it.

## Decisions taken in the file store (P2 PR 1)

- **Journal before apply, under memory's write lock** (the commit hook), not apply-then-roll-back: a rollback can't retract a watch event already sent, and readers would see a change that may vanish. The cost is that reads wait for a write's fsync; group commit is the seam if that matters.
- **The hook is the only change to memory:** `WithCommitHook`, `Snapshot` and `Restore`. Without a hook memory behaves as before (`New` takes the options variadically).
- **Snapshot objects carry `v`, `created` and `at` beside `obj`**, as journal lines carry `v` and `at`: Event has no metadata, and SinceUpdated retention needs the last-write time.
- **Kinds are registered** (`WithKind`) rather than learned at write time, and an unregistered kind can't be written: a kind that could be stored but not loaded would fail the next start.
- **A failed fsync stops writes until reopen** (`ErrJournal`); a failed write that was rolled back doesn't.
- **Transcript names:** safe run names are the file name; others are `~` + base64url, so no run name can escape `transcripts/`.

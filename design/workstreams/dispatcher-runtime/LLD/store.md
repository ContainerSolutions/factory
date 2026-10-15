# LLD — Resource store (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Task: RT-S1-T01 · Package: `internal/store` (+ `store/memory`, `store/storetest`)

## Interface

```go
type Key struct { Kind, Name string }

type Store interface {
    Get(ctx context.Context, k Key, out Object) error                         // ErrNotFound
    List(ctx context.Context, kind string, opts ListOptions, out any) error    // out: *[]T
    Create(ctx context.Context, obj Object) error                              // ErrAlreadyExists; sets ResourceVersion, CreatedAt
    Update(ctx context.Context, obj Object) error                              // CAS on ResourceVersion → ErrConflict
    Delete(ctx context.Context, k Key) error
    Watch(ctx context.Context, kind string, opts WatchOptions) (Watcher, error)
    Lock(ctx context.Context, name string, ttl time.Duration) (Unlock, error)  // advisory, lease-based
}

type ListOptions struct { LabelSelector labels.Selector; Field map[string]string; Cursor string; Limit int }
type WatchOptions struct { LabelSelector labels.Selector; SinceVersion string; Buffer int }
type Watcher interface {
    Events() <-chan WatchEvent   // closed when the watch ends (ctx done, Stop, or expiry)
    Err() error                  // after Events() is closed: nil, context error, or ErrWatchExpired
    Stop()
}
type WatchEvent struct { Type string /* added|modified|deleted */; Object Object; Version string }
type Object interface { GetKind() string; GetName() string; GetResourceVersion() string; SetResourceVersion(string) }
```

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

## Postgres later

`store/storetest` holds a **conformance suite** (`func Run(t *testing.T, newStore func() Store)`) covering CAS conflicts, watch ordering, resync, selectors, locks and retention. A future Postgres store must pass the same suite.

## Tests

- Conformance suite on `memory` under `-race`.
- Concurrency: 50 goroutines updating the same object → exactly one wins per version.
- Watch slow-consumer → channel closed, `Err() == ErrWatchExpired`, then successful resync; writer never blocks.

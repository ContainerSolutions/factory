// Package journal is the encoding the durable stores share: P2's journal line (one committed
// change) and snapshot (the whole store at a version), the kinds they can load and the
// transcript blob names. The file store (store/filestore) writes them to a journal file and a
// snapshot file; an object store (P3) writes one line per log object, plus
// the writer and the schema version, and gzip'd snapshots.
//
// Design: design/workstreams/dispatcher-runtime/LLD/store.md §File store, and §Object store
// when present; P3.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// The names each durable store keeps at the top of its directory or bucket prefix. Both name
// transcript blobs TranscriptDir/<TranscriptName>, so neither may open a directory that holds
// the other's: the file store refuses ObjectStoreLog and ObjectStoreSnapshots, a file://
// bucket refuses the file store's files.
const (
	FileStoreSnapshot    = "snapshot.json"
	FileStoreJournal     = "journal.jsonl"
	FileStoreLock        = "LOCK"
	ObjectStoreLog       = "log"
	ObjectStoreSnapshots = "snapshots"
	TranscriptDir        = "transcripts"
)

// Line is one committed change: a line of the file store's journal, or one object of the
// object store's log. W and Schema are the object store's; the file store leaves them out.
type Line struct {
	V    string          `json:"v"`
	At   time.Time       `json:"at"`
	Op   memory.Op       `json:"op"`
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Obj  json.RawMessage `json:"obj,omitzero"`
	// W is who wrote the change: the writer's instance id and the write's nonce.
	W *Writer `json:"w,omitzero"`
	// Schema is the writer's store.SchemaVersion.
	Schema int `json:"schema,omitzero"`
}

// Writer names the write that made a change.
type Writer struct {
	// ID is the writing store's instance id.
	ID string `json:"id"`
	// Nonce is random per write: a writer that finds its own nonce in the log knows the
	// change is its own.
	Nonce string `json:"nonce"`
}

// LineOf returns c as a line.
func LineOf(c memory.Change) Line {
	return Line{V: store.FormatVersion(c.Version), At: c.At, Op: c.Op, Kind: c.Key.Kind, Name: c.Key.Name, Obj: c.Data}
}

// Change returns the line as a memory change at version v.
func (l Line) Change(v uint64) memory.Change {
	return memory.Change{Op: l.Op, Key: store.Key{Kind: l.Kind, Name: l.Name}, Version: v, At: l.At, Data: l.Obj}
}

// DecodeLine parses and checks one line, and returns it with its version.
func DecodeLine(line []byte) (Line, uint64, error) {
	var jl Line
	if err := json.Unmarshal(line, &jl); err != nil {
		return jl, 0, fmt.Errorf("invalid JSON: %w", err)
	}
	v, err := store.ParseVersion("load", store.Key{}, jl.V)
	if err != nil || v == 0 {
		return jl, 0, fmt.Errorf("bad version %q", jl.V)
	}
	if jl.Kind == "" || jl.Name == "" {
		return jl, 0, errors.New("missing kind or name")
	}
	switch jl.Op {
	case memory.OpCreate, memory.OpUpdate:
		if len(jl.Obj) == 0 {
			return jl, 0, fmt.Errorf("%s without obj", jl.Op)
		}
	case memory.OpDelete:
	default:
		return jl, 0, fmt.Errorf("unknown op %q", jl.Op)
	}
	if jl.Schema < 0 {
		return jl, 0, fmt.Errorf("bad schema %d", jl.Schema)
	}
	return jl, v, nil
}

// Entry is one object while loading.
type Entry struct {
	V                uint64
	Created, Updated time.Time
	Data             []byte
}

// State is the objects while loading, by key.
type State map[store.Key]*Entry

// Apply replays one line, at version v, onto the state. types are the kinds that can be
// loaded (Registry).
func (st State) Apply(types map[string]reflect.Type, jl Line, v uint64) error {
	k := store.Key{Kind: jl.Kind, Name: jl.Name}
	if types[k.Kind] == nil {
		return UnknownKind(k.Kind)
	}
	old := st[k]
	switch jl.Op {
	case memory.OpCreate:
		if old != nil {
			return fmt.Errorf("create %s: it exists", k)
		}
		st[k] = &Entry{V: v, Created: jl.At, Updated: jl.At, Data: jl.Obj}
	case memory.OpUpdate:
		if old == nil {
			return fmt.Errorf("update %s: it doesn't exist", k)
		}
		st[k] = &Entry{V: v, Created: old.Created, Updated: jl.At, Data: jl.Obj}
	case memory.OpDelete:
		if old == nil {
			return fmt.Errorf("delete %s: it doesn't exist", k)
		}
		delete(st, k)
	default:
		return fmt.Errorf("%s: unknown op %q", k, jl.Op)
	}
	return nil
}

// Records returns the state as memory records (for memory.Restore), and the objects per kind.
func (st State) Records(types map[string]reflect.Type) ([]memory.Record, map[string]int) {
	recs := make([]memory.Record, 0, len(st))
	counts := map[string]int{}
	for k, e := range st {
		recs = append(recs, memory.Record{Key: k, Type: types[k.Kind], Data: e.Data, Version: e.V, Created: e.Created, Updated: e.Updated})
		counts[k.Kind]++
	}
	return recs, counts
}

// UnknownKind is the error for a stored kind with no registered Go type.
func UnknownKind(kind string) error {
	return fmt.Errorf("kind %q has no Go type registered (WithKind on the file store or an object store)", kind)
}

// BuiltinKinds are the v1alpha1 kinds the store holds.
var BuiltinKinds = []string{
	v1alpha1.KindEvent, v1alpha1.KindWorkItem, v1alpha1.KindTask, v1alpha1.KindRun,
	v1alpha1.KindAgent, v1alpha1.KindEffect, v1alpha1.KindBudgetCounter, v1alpha1.KindApproval,
}

// Registry returns the Go type of every kind a durable store may hold: the built-in kinds and
// extra, the internal kinds (a pointer to each one's zero value).
func Registry(extra map[string]store.Object) (map[string]reflect.Type, error) {
	types := map[string]reflect.Type{}
	for _, k := range BuiltinKinds {
		o, err := v1alpha1.New(k)
		if err != nil {
			return nil, err
		}
		types[k] = reflect.TypeOf(o).Elem()
	}
	for k, proto := range extra {
		t := reflect.TypeOf(proto)
		if proto == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
			return nil, fmt.Errorf("kind %q: the prototype must be a pointer to a struct, got %T", k, proto)
		}
		if k == "" || v1alpha1.IsConfigKind(k) {
			return nil, fmt.Errorf("kind %q can't be stored", k)
		}
		if old, ok := types[k]; ok && old != t.Elem() {
			return nil, fmt.Errorf("kind %q is already %s, not %s", k, old, t.Elem())
		}
		types[k] = t.Elem()
	}
	return types, nil
}

// CheckType rejects a write of obj, of a kind the store couldn't load back. Writes that are
// invalid anyway (store.CheckObject) are left to the memory store, so its errors stay the
// conformant ones. name is the store's, for the message.
func CheckType(types map[string]reflect.Type, op string, obj store.Object, name string) error {
	k, err := store.CheckObject(op, obj)
	if err != nil {
		return nil
	}
	t := types[k.Kind]
	if t == nil {
		return store.Errorf(op, k, store.ErrInvalid, "kind %s has no Go type registered with the %s (WithKind)", k.Kind, name)
	}
	if got := reflect.TypeOf(obj).Elem(); got != t {
		return store.Errorf(op, k, store.ErrInvalid, "object is %T, the %s holds %s as *%s", obj, name, k.Kind, t)
	}
	return nil
}

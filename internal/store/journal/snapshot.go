package journal

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"time"

	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// Format is the snapshot format this package reads and writes.
const Format = 1

// SnapshotObject is one object in a snapshot. created, at and v are kept beside obj because
// kinds without metadata (Event) don't carry them.
type SnapshotObject struct {
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	V       string          `json:"v"`
	Created time.Time       `json:"created"`
	At      time.Time       `json:"at"`
	Obj     json.RawMessage `json:"obj"`
}

// FormatError is a snapshot whose format this package doesn't read.
type FormatError struct{ Got int }

func (e *FormatError) Error() string {
	return fmt.Sprintf("format %d, and this dispatcher reads format %d", e.Got, Format)
}

// WriteSnapshot writes the snapshot of recs at version v to w, one object per line (so it
// greps), in kind and name order. schema is the writer's schema version; 0 leaves it out (the
// file store's snapshots have none).
func WriteSnapshot(w io.Writer, recs []memory.Record, v uint64, schema int) error {
	recs = slices.Clone(recs)
	slices.SortFunc(recs, func(a, b memory.Record) int {
		return cmp.Or(cmp.Compare(a.Key.Kind, b.Key.Kind), cmp.Compare(a.Key.Name, b.Key.Name))
	})
	bw := bufio.NewWriterSize(w, 1<<20)
	if schema > 0 {
		fmt.Fprintf(bw, `{"format":%d,"storeVersion":"%d","schema":%d,"objects":[`, Format, v, schema)
	} else {
		fmt.Fprintf(bw, `{"format":%d,"storeVersion":"%d","objects":[`, Format, v)
	}
	for i, r := range recs {
		b, err := json.Marshal(SnapshotObject{
			Kind: r.Key.Kind, Name: r.Key.Name, V: store.FormatVersion(r.Version),
			Created: r.Created, At: r.Updated, Obj: r.Data,
		})
		if err != nil {
			return fmt.Errorf("encode %s: %w", r.Key, err)
		}
		if i > 0 {
			_ = bw.WriteByte(',')
		}
		_ = bw.WriteByte('\n')
		_, _ = bw.Write(b)
	}
	_, _ = bw.WriteString("\n]}\n")
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	return nil
}

// Header is a snapshot's header.
type Header struct {
	Format       int    `json:"format"`
	StoreVersion string `json:"storeVersion"`
	Schema       int    `json:"schema,omitzero"`
}

// ReadHeader parses a snapshot's header: a *FormatError for a format this package doesn't
// read. The format is decoded alone first, so another format's header fields can't fail it.
func ReadHeader(data []byte) (Header, error) {
	var f struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return Header{}, err
	}
	if f.Format != Format {
		return Header{Format: f.Format}, &FormatError{Got: f.Format}
	}
	var hdr Header
	if err := json.Unmarshal(data, &hdr); err != nil {
		return hdr, err
	}
	return hdr, nil
}

// ParseSnapshot loads a snapshot's objects into state, which must be empty, and returns its
// version and schema. A *FormatError is a format this package doesn't read; any other error
// is a corrupt snapshot, saying where.
func ParseSnapshot(data []byte, types map[string]reflect.Type, state State) (version uint64, schema int, err error) {
	hdr, err := ReadHeader(data)
	if err != nil {
		return 0, 0, err
	}
	var snap struct {
		Objects []SnapshotObject `json:"objects"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return 0, 0, err
	}
	version, err = store.ParseVersion("load", store.Key{}, hdr.StoreVersion)
	if err != nil {
		return 0, 0, fmt.Errorf("storeVersion %q", hdr.StoreVersion)
	}
	if hdr.Schema < 0 {
		return 0, 0, fmt.Errorf("schema %d", hdr.Schema)
	}
	for i, o := range snap.Objects {
		k := store.Key{Kind: o.Kind, Name: o.Name}
		bad := func(format string, args ...any) error {
			return fmt.Errorf("object %d (%s): %s", i, k, fmt.Sprintf(format, args...))
		}
		if k.Kind == "" || k.Name == "" {
			return 0, 0, bad("missing kind or name")
		}
		if types[k.Kind] == nil {
			return 0, 0, bad("%v", UnknownKind(k.Kind))
		}
		v, err := store.ParseVersion("load", k, o.V)
		if err != nil || v == 0 || v > version {
			return 0, 0, bad("version %q outside 1..%d", o.V, version)
		}
		if len(o.Obj) == 0 {
			return 0, 0, bad("missing obj")
		}
		if state[k] != nil {
			return 0, 0, bad("duplicate")
		}
		state[k] = &Entry{V: v, Created: o.Created, Updated: o.At, Data: o.Obj}
	}
	return version, hdr.Schema, nil
}

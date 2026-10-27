package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

var at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestLineFormat: the file store's line is P2's, unchanged (no w, no schema); the object
// store's carries both; both decode back.
func TestLineFormat(t *testing.T) {
	c := memory.Change{Op: memory.OpCreate, Key: store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, Version: 7, At: at, Data: []byte(`{"kind":"Run"}`)}
	p2, err := json.Marshal(LineOf(c))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":"7","at":"2026-01-01T00:00:00Z","op":"create","kind":"Run","name":"r1","obj":{"kind":"Run"}}`; string(p2) != want {
		t.Fatalf("file store line = %s, want %s", p2, want)
	}
	l := LineOf(c)
	l.W, l.Schema = &Writer{ID: "ins_1", Nonce: "n1"}, 2
	shared, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	got, v, err := DecodeLine(shared)
	if err != nil || v != 7 || !reflect.DeepEqual(got, l) || !reflect.DeepEqual(got.Change(v), c) {
		t.Fatalf("DecodeLine = %+v, %d, %v", got, v, err)
	}
	for _, bad := range []string{`{`, `{"v":"0","kind":"Run","name":"r","op":"delete"}`, `{"v":"1","name":"r","op":"delete"}`,
		`{"v":"1","kind":"Run","name":"r","op":"create"}`, `{"v":"1","kind":"Run","name":"r","op":"rename"}`,
		`{"v":"1","kind":"Run","name":"r","op":"delete","schema":-1}`} {
		if _, _, err := DecodeLine([]byte(bad)); err == nil {
			t.Fatalf("DecodeLine(%s) succeeded", bad)
		}
	}
}

// TestSnapshotRoundTrip: a snapshot with a schema reads back, and one in another format is a
// *FormatError.
func TestSnapshotRoundTrip(t *testing.T) {
	types, err := Registry(nil)
	if err != nil {
		t.Fatal(err)
	}
	recs := []memory.Record{
		{Key: store.Key{Kind: v1alpha1.KindRun, Name: "b"}, Data: []byte(`{"kind":"Run","metadata":{"name":"b"}}`), Version: 2, Created: at, Updated: at},
		{Key: store.Key{Kind: v1alpha1.KindRun, Name: "a"}, Data: []byte(`{"kind":"Run","metadata":{"name":"a"}}`), Version: 1, Created: at, Updated: at},
	}
	for _, schema := range []int{0, 3} {
		var buf bytes.Buffer
		if err := WriteSnapshot(&buf, recs, 5, schema); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), `"schema"`) != (schema > 0) {
			t.Fatalf("schema %d: %s", schema, buf.String())
		}
		st := State{}
		v, gotSchema, err := ParseSnapshot(buf.Bytes(), types, st)
		if err != nil || v != 5 || gotSchema != schema || len(st) != 2 || st[store.Key{Kind: v1alpha1.KindRun, Name: "a"}].V != 1 {
			t.Fatalf("ParseSnapshot = %d, %d, %v, %v", v, gotSchema, st, err)
		}
		if i := strings.Index(buf.String(), `"name":"a"`); i < 0 || i > strings.Index(buf.String(), `"name":"b"`) {
			t.Fatal("objects not in name order")
		}
	}
	var fe *FormatError
	if _, _, err := ParseSnapshot([]byte(`{"format":2,"storeVersion":"1","objects":[]}`), types, State{}); !errors.As(err, &fe) || fe.Got != 2 {
		t.Fatalf("format 2 = %v", err)
	}
}

func TestRegistry(t *testing.T) {
	if _, err := Registry(map[string]store.Object{v1alpha1.KindRun: &v1alpha1.Task{}}); err == nil {
		t.Fatal("a built-in kind registered as another type")
	}
	if _, err := Registry(map[string]store.Object{"Queue": &v1alpha1.Run{}}); err == nil {
		t.Fatal("a config kind registered")
	}
	types, err := Registry(map[string]store.Object{"Thing": &thing{}})
	if err != nil || types["Thing"] != reflect.TypeFor[thing]() || types[v1alpha1.KindTask] == nil {
		t.Fatalf("Registry = %v, %v", types, err)
	}
}

type thing struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
}

func (t *thing) GetName() string { return t.Metadata.Name }

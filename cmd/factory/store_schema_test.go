package main

import (
	"encoding"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/storeaudit"
)

// schemaGolden holds the JSON field set of every stored kind at store.ReadableSchema.
var schemaGolden = filepath.Join("testdata", "store", "schema.golden")

// TestStoredSchema keeps store.SchemaVersion and store.ReadableSchema honest (P3 §Two releases
// on one log): the golden holds the JSON fields of every stored kind (the v1alpha1 runtime
// kinds and internalKinds) and the ReadableSchema they belong to. Changing a stored type
// without bumping ReadableSchema fails, and -update refuses to rewrite the golden until it is
// bumped.
func TestStoredSchema(t *testing.T) {
	got := storedShape(t)
	raw, err := os.ReadFile(schemaGolden)
	if err != nil && !*updateGolden {
		t.Fatalf("%v: run go test ./cmd/factory -run TestStoredSchema -update", err)
	}
	wantSchema, want := parseSchemaGolden(t, string(raw))
	same := slices.Equal(got, want)
	switch {
	case same && wantSchema == store.ReadableSchema:
		return
	case *updateGolden && (same || store.ReadableSchema > wantSchema):
		if err := os.MkdirAll(filepath.Dir(schemaGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(schemaGolden, []byte(renderSchemaGolden(got)), 0o644); err != nil { //nolint:gosec // a test golden
			t.Fatal(err)
		}
		return
	case !same && store.ReadableSchema == wantSchema:
		t.Fatalf(`the stored types changed, and store.ReadableSchema is still %d.
A shared log (the object store) is read by two releases during a deploy, so a change to a stored
kind ships expand-then-write (internal/store/schema.go):
  1. this release: add the kind or optional field, bump store.ReadableSchema to %d and keep
     store.SchemaVersion at %d; then go test ./cmd/factory -run TestStoredSchema -update;
  2. the next release: bump store.SchemaVersion to %d.
Changes:
%s`, store.ReadableSchema, store.SchemaVersion+1, store.SchemaVersion, store.SchemaVersion+1, shapeDiff(want, got))
	default:
		t.Fatalf("store.ReadableSchema is %d and %s was written at %d: run go test ./cmd/factory -run TestStoredSchema -update\nChanges:\n%s",
			store.ReadableSchema, schemaGolden, wantSchema, shapeDiff(want, got))
	}
}

// TestInternalKindsAudited: serve's table of internal kinds is the secrets audit's, so every
// kind serve stores is written and scanned by the stores' secrets tests.
func TestInternalKindsAudited(t *testing.T) {
	audited := storeaudit.InternalKinds()
	for kind, proto := range internalKinds() {
		a, ok := audited[kind]
		if !ok || reflect.TypeOf(a) != reflect.TypeOf(proto) {
			t.Errorf("internal kind %s (%T) is not in storeaudit.InternalKinds: add it there, with an audit line and a fixture", kind, proto)
		}
		if storeaudit.KindAudit[kind] == "" {
			t.Errorf("internal kind %s has no line in storeaudit.KindAudit", kind)
		}
	}
	for kind := range audited {
		if _, ok := internalKinds()[kind]; !ok {
			t.Errorf("storeaudit.InternalKinds has %s, which serve doesn't store", kind)
		}
	}
}

// storedShape returns one line per JSON field of every stored kind: "<kind> <path> <type>".
func storedShape(t *testing.T) []string {
	t.Helper()
	kinds := map[string]reflect.Type{}
	for _, k := range journal.BuiltinKinds {
		o, err := v1alpha1.New(k)
		if err != nil {
			t.Fatal(err)
		}
		kinds[k] = reflect.TypeOf(o).Elem()
	}
	for k, proto := range internalKinds() {
		kinds[k] = reflect.TypeOf(proto).Elem()
	}
	var lines []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range kinds {
			if !yield(k) {
				return
			}
		}
	}) {
		walkShape(k, "", kinds[k], map[reflect.Type]bool{}, &lines)
	}
	return lines
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// walkShape appends the fields of t, at path, to out. Types that encode themselves (time.Time,
// json.RawMessage) are leaves, named by their type; other leaves by their kind, so renaming a
// Go type doesn't count as a change, but renaming a field or changing its kind does.
func walkShape(kind, path string, t reflect.Type, stack map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	leaf := func(desc string) { *out = append(*out, kind+" "+cmpPath(path)+" "+desc) }
	pt := reflect.PointerTo(t)
	switch {
	case t.Implements(jsonMarshaler) || pt.Implements(jsonMarshaler) || t.Implements(textMarshaler) || pt.Implements(textMarshaler):
		leaf(t.String())
		return
	case stack[t]:
		leaf("recursive " + t.String())
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		stack[t] = true
		defer delete(stack, t)
		if path != "" {
			leaf("object")
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" {
				walkShape(kind, path, f.Type, stack, out) // embedded: its fields are inline
				continue
			}
			if name == "" {
				name = f.Name
			}
			walkShape(kind, path+"."+name, f.Type, stack, out)
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			leaf("bytes")
			return
		}
		leaf("list")
		walkShape(kind, path+"[]", t.Elem(), stack, out)
	case reflect.Map:
		leaf("map")
		walkShape(kind, path+"{}", t.Elem(), stack, out)
	case reflect.Interface:
		leaf("any")
	default:
		leaf(t.Kind().String())
	}
}

func cmpPath(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func renderSchemaGolden(lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# The JSON fields of every stored kind (the v1alpha1 runtime kinds and cmd/factory's\n")
	fmt.Fprintf(&b, "# internalKinds), at store.ReadableSchema below. A change here needs a ReadableSchema bump\n")
	fmt.Fprintf(&b, "# first (expand-then-write, internal/store/schema.go), then:\n")
	fmt.Fprintf(&b, "#   go test ./cmd/factory -run TestStoredSchema -update\n")
	fmt.Fprintf(&b, "readableSchema %d\n", store.ReadableSchema)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

func parseSchemaGolden(t *testing.T, s string) (int, []string) {
	t.Helper()
	schema := 0
	var lines []string
	for l := range strings.SplitSeq(s, "\n") {
		switch {
		case l == "", strings.HasPrefix(l, "#"):
		case strings.HasPrefix(l, "readableSchema "):
			n, err := strconv.Atoi(strings.TrimPrefix(l, "readableSchema "))
			if err != nil {
				t.Fatalf("%s: %q", schemaGolden, l)
			}
			schema = n
		default:
			lines = append(lines, l)
		}
	}
	return schema, lines
}

// shapeDiff lists the lines only in want (-) and only in got (+).
func shapeDiff(want, got []string) string {
	in := func(l []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range l {
			m[x] = true
		}
		return m
	}
	w, g := in(want), in(got)
	var b strings.Builder
	for _, l := range want {
		if !g[l] {
			b.WriteString("  - " + l + "\n")
		}
	}
	for _, l := range got {
		if !w[l] {
			b.WriteString("  + " + l + "\n")
		}
	}
	return b.String()
}

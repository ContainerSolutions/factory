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
// kinds and internalKinds), as a sorted set, and the ReadableSchema they belong to. Changing a
// stored type needs ReadableSchema at SchemaVersion+1 (schemaVerdict says when -update may
// rewrite the golden).
func TestStoredSchema(t *testing.T) {
	got := storedShape(t)
	raw, err := os.ReadFile(schemaGolden)
	if err != nil && !*updateGolden {
		t.Fatalf("%v: run go test ./cmd/factory -run TestStoredSchema -update", err)
	}
	goldenSchema, want := parseSchemaGolden(t, string(raw))
	same := slices.Equal(got, want)
	write, msg := schemaVerdict(same, goldenSchema, store.ReadableSchema, store.SchemaVersion, *updateGolden)
	if !write && msg == "" && string(raw) != renderSchemaGolden(got) {
		// Same fields and version, other text (the header): rewrite it, or say to.
		write, msg = *updateGolden, "the text of "+schemaGolden+" is out of date: run go test ./cmd/factory -run TestStoredSchema -update"
		if write {
			msg = ""
		}
	}
	if write {
		if err := os.MkdirAll(filepath.Dir(schemaGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(schemaGolden, []byte(renderSchemaGolden(got)), 0o644); err != nil { //nolint:gosec // a test golden
			t.Fatal(err)
		}
		return
	}
	if msg != "" {
		t.Fatalf("%s\nChanges:\n%s", msg, shapeDiff(want, got))
	}
}

// schemaVerdict decides what TestStoredSchema does: write the golden (with -update), fail
// with msg, or pass (neither). same says whether the stored types' shape matches the golden,
// written at goldenSchema.
//
// Expand-then-write (internal/store/schema.go): a change to a stored type goes into a pending
// ReadableSchema of SchemaVersion+1, which -update accepts. Further changes join it until a
// release that reads it ships; a test can't know that, so schema.go and store.md say it. A
// change that also bumps SchemaVersion skips the expand step, and -update refuses it.
func schemaVerdict(same bool, goldenSchema, readable, schemaVersion int, update bool) (write bool, msg string) {
	pending := readable == schemaVersion+1
	switch {
	case readable != schemaVersion && !pending, goldenSchema > 0 && readable < goldenSchema:
		return false, fmt.Sprintf(`store.ReadableSchema is %d and store.SchemaVersion %d (the golden was written at %d):
ReadableSchema must be SchemaVersion or SchemaVersion+1, and never goes below the golden's
(internal/store/schema.go).`, readable, schemaVersion, goldenSchema)
	case same && goldenSchema == readable:
		return false, ""
	case update && goldenSchema == 0:
		return true, "" // no golden yet
	case goldenSchema == 0:
		return false, "there is no " + schemaGolden + ": run go test ./cmd/factory -run TestStoredSchema -update"
	case !same && schemaVersion > goldenSchema:
		return false, fmt.Sprintf(`the stored types changed, and store.SchemaVersion (%d) moved past the golden's %d: a release
would write a schema no deployed release reads. Expand first: bump ReadableSchema alone and
ship it; bump SchemaVersion in a later release (internal/store/schema.go).`, schemaVersion, goldenSchema)
	case update && (same || pending):
		return true, ""
	case same:
		return false, fmt.Sprintf("store.ReadableSchema is %d and %s was written at %d: run go test ./cmd/factory -run TestStoredSchema -update",
			readable, schemaGolden, goldenSchema)
	case pending && readable == goldenSchema:
		return false, fmt.Sprintf(`the stored types changed. store.ReadableSchema %d is pending (store.SchemaVersion is still %d),
so this change joins it, and the release that bumps store.SchemaVersion to %d writes them all.
Unless a release that reads %d has shipped: then %d is frozen, and this change waits for
SchemaVersion %d to ship and opens %d.
Run go test ./cmd/factory -run TestStoredSchema -update.`, readable, schemaVersion, readable, readable, readable, readable, readable+1)
	case pending:
		return false, fmt.Sprintf("store.ReadableSchema is now %d (pending; %s was written at %d): run go test ./cmd/factory -run TestStoredSchema -update",
			readable, schemaGolden, goldenSchema)
	default: // the types changed, and ReadableSchema == SchemaVersion == the golden's
		return false, fmt.Sprintf(`the stored types changed, and store.ReadableSchema is still %d.
A shared log (an object store) is read by two releases during a deploy, so a change to a stored
kind ships expand-then-write (internal/store/schema.go):
  1. this release: add the kind or optional field, bump store.ReadableSchema to %d and keep
     store.SchemaVersion at %d; then go test ./cmd/factory -run TestStoredSchema -update
     (later changes join the pending version until a release that reads it ships);
  2. a later release: bump store.SchemaVersion to %d.`, readable, schemaVersion+1, schemaVersion, schemaVersion+1)
	}
}

// TestSchemaVerdict covers the policy: a change needs a pending ReadableSchema, and changes
// accumulate into it until SchemaVersion catches up.
func TestSchemaVerdict(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		same                            bool
		golden, readable, schemaVersion int
		update                          bool
		write                           bool
		msg                             string // a substring; "" for a pass or a write
	}{
		{name: "unchanged", same: true, golden: 1, readable: 1, schemaVersion: 1},
		{name: "changed, no bump", golden: 1, readable: 1, schemaVersion: 1, msg: "bump store.ReadableSchema to 2"},
		{name: "changed, no bump, -update refused", golden: 1, readable: 1, schemaVersion: 1, update: true, msg: "bump store.ReadableSchema to 2"},
		{name: "changed, bumped", golden: 1, readable: 2, schemaVersion: 1, msg: "run go test"},
		{name: "changed, bumped, -update", golden: 1, readable: 2, schemaVersion: 1, update: true, write: true},
		{name: "another change while pending", golden: 2, readable: 2, schemaVersion: 1, msg: "joins it"},
		{name: "another change while pending, -update", golden: 2, readable: 2, schemaVersion: 1, update: true, write: true},
		{name: "written, unchanged", same: true, golden: 2, readable: 2, schemaVersion: 2},
		{name: "written, changed again", golden: 2, readable: 2, schemaVersion: 2, update: true, msg: "bump store.ReadableSchema to 3"},
		{name: "bumped with no change, -update", same: true, golden: 1, readable: 2, schemaVersion: 1, update: true, write: true},
		{name: "changed, both bumped", golden: 1, readable: 2, schemaVersion: 2, msg: "Expand first"},
		{name: "changed, both bumped, -update refused", golden: 1, readable: 2, schemaVersion: 2, update: true, msg: "Expand first"},
		{name: "unchanged, versions moved, -update", same: true, golden: 1, readable: 2, schemaVersion: 2, update: true, write: true},
		{name: "no golden", golden: 0, readable: 1, schemaVersion: 1, msg: "there is no"},
		{name: "no golden, -update", golden: 0, readable: 1, schemaVersion: 1, update: true, write: true},
		{name: "ReadableSchema two ahead", golden: 1, readable: 3, schemaVersion: 1, msg: "must be SchemaVersion or SchemaVersion+1"},
		{name: "ReadableSchema two ahead, -update refused", golden: 1, readable: 3, schemaVersion: 1, update: true, msg: "must be SchemaVersion or SchemaVersion+1"},
		{name: "ReadableSchema behind SchemaVersion", golden: 2, readable: 1, schemaVersion: 2, update: true, msg: "must be SchemaVersion or SchemaVersion+1"},
		{name: "ReadableSchema below the golden", same: true, golden: 2, readable: 1, schemaVersion: 1, update: true, msg: "never goes below"},
		{name: "changed, SchemaVersion past the golden while pending", golden: 1, readable: 3, schemaVersion: 2, update: true, msg: "Expand first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write, msg := schemaVerdict(tc.same, tc.golden, tc.readable, tc.schemaVersion, tc.update)
			if write != tc.write || (tc.msg == "") != (msg == "") || !strings.Contains(msg, tc.msg) {
				t.Fatalf("schemaVerdict = %v, %q; want %v, %q", write, msg, tc.write, tc.msg)
			}
		})
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

// storedShape returns one line per JSON field of every stored kind, "<kind> <path> <type>", sorted.
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
	slices.Sort(lines) // a set: reordering a struct's fields changes no JSON
	return slices.Compact(lines)
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
	fmt.Fprintf(&b, "# internalKinds), a sorted set, at store.ReadableSchema below. A change here needs a\n")
	fmt.Fprintf(&b, "# pending ReadableSchema (SchemaVersion+1, bumped alone: expand-then-write,\n")
	fmt.Fprintf(&b, "# internal/store/schema.go). Changes join it until a release that reads it ships; then it\n")
	fmt.Fprintf(&b, "# is frozen until SchemaVersion catches up. To rewrite this file:\n")
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
	slices.Sort(lines)
	return schema, slices.Compact(lines)
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

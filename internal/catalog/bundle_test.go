package catalog

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compileServed compiles a served output schema the way a reader without the catalog does:
// a compiler that knows only the v1alpha1 schemas, the document under an unrelated URL.
func compileServed(t *testing.T, doc []byte) *jsonschema.Schema {
	t.Helper()
	c, err := newSchemaCompiler()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("the document isn't JSON: %v\n%s", err, doc)
	}
	const u = "https://reader.invalid/task-output/work.json"
	if err := c.AddResource(u, parsed); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(u)
	if err != nil {
		t.Fatalf("the served schema doesn't compile on its own: %v\n%s", err, doc)
	}
	return s
}

func validates(t *testing.T, s *jsonschema.Schema, inst string) bool {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(inst))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(v) == nil
}

func TestOutputSchemaDocument(t *testing.T) {
	const (
		// work.json refers to a sibling file and, through it, to a file in another directory.
		work = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "allOf": [{ "$ref": "https://factory.ccf.dev/schemas/v1alpha1/result.schema.json" }],
  "properties": { "data": { "$ref": "parts/data.json" } },
  "required": ["data"]
}`
		data  = `{"type": "object", "required": ["prs"], "properties": {"prs": {"$ref": "../common/prs.json#/$defs/prs"}}}`
		prs   = `{"$defs": {"prs": {"type": "array", "items": {"type": "string", "pattern": "#[0-9]+$"}}}}`
		other = `{"type": "string"}`
	)
	tests := []struct {
		name  string
		files map[string]string
		// raw: the document is work.json's bytes as written.
		raw bool
		// embedded: the catalog schemas the bundle embeds.
		embedded []string
	}{
		{name: "no catalog refs: the raw bytes", files: map[string]string{"schemas/other.json": other}, raw: true},
		{
			name:     "relative refs: one compound document",
			files:    map[string]string{"schemas/work.json": work, "schemas/parts/data.json": data, "schemas/common/prs.json": prs, "schemas/other.json": other},
			embedded: []string{"schemas/common/prs.json", "schemas/parts/data.json", "schemas/work.json"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, errs := LoadDir(fsWith(tc.files), Options{})
			if c == nil || HasErrors(errs) {
				t.Fatalf("errors:\n%s", format(errs))
			}
			ts := c.TaskSpecs["work"]
			if ts.OutputSchema == nil || ts.OutputSchemaDocument == nil {
				t.Fatalf("compiled %v, document %q", ts.OutputSchema != nil, ts.OutputSchemaDocument)
			}
			if got, want := c.Assets.SchemaSources["schemas/work.json"], fsWith(tc.files)["schemas/work.json"].Data; !bytes.Equal(got, want) {
				t.Errorf("Assets.SchemaSources = %q, want the raw file", got)
			}
			if len(c.Assets.SchemaSources) != len(c.Assets.Schemas) {
				t.Errorf("%d sources for %d schemas", len(c.Assets.SchemaSources), len(c.Assets.Schemas))
			}
			if tc.raw {
				if !bytes.Equal(ts.OutputSchemaDocument, c.Assets.SchemaSources["schemas/work.json"]) {
					t.Errorf("document %s, want the raw bytes", ts.OutputSchemaDocument)
				}
				return
			}
			var doc struct {
				Ref  string                     `json:"$ref"`
				Defs map[string]json.RawMessage `json:"$defs"`
			}
			if err := json.Unmarshal(ts.OutputSchemaDocument, &doc); err != nil {
				t.Fatal(err)
			}
			var keys []string
			for k := range doc.Defs {
				keys = append(keys, k)
			}
			if doc.Ref != assetBaseURL+"schemas/work.json" || strings.Join(sortedKeys(doc.Defs), ",") != strings.Join(tc.embedded, ",") {
				t.Errorf("bundle $ref %q embeds %v, want %v", doc.Ref, keys, tc.embedded)
			}
			served := compileServed(t, ts.OutputSchemaDocument)
			for _, inst := range []string{
				`{"task": "t", "status": "done", "summary": "s", "question": null, "data": {"prs": ["acme/docs#13"]}}`,
				`{"task": "t", "status": "done", "summary": "s", "question": null, "data": {"prs": ["acme/docs"]}}`,
				`{"task": "t", "status": "done", "summary": "s", "question": null, "data": {}}`,
				`{"task": "t", "status": "done", "summary": "s", "question": null}`,
				`{"status": "done"}`,
			} {
				if got, want := validates(t, served, inst), validates(t, ts.OutputSchema, inst); got != want {
					t.Errorf("%s: the served schema says %v, the compiled one %v", inst, got, want)
				}
			}
			if !validates(t, served, `{"task": "t", "status": "done", "summary": "s", "question": null, "data": {"prs": ["acme/docs#13"]}}`) {
				t.Error("the served schema refuses a valid result")
			}
		})
	}
}

func TestOutputSchemaDocumentNone(t *testing.T) {
	c, errs := LoadDir(fsWith(map[string]string{
		"taskspecs/work.yaml": strings.Replace(base["taskspecs/work.yaml"], "schema: schemas/work.json, ", "", 1),
	}), Options{})
	if c == nil || HasErrors(errs) {
		t.Fatalf("errors:\n%s", format(errs))
	}
	if ts := c.TaskSpecs["work"]; ts.OutputSchema != nil || ts.OutputSchemaDocument != nil {
		t.Errorf("a TaskSpec without output.schema has a schema: %v %q", ts.OutputSchema != nil, ts.OutputSchemaDocument)
	}
}

func TestCatalogRef(t *testing.T) {
	tests := []struct {
		from, ref, want string
		ok              bool
	}{
		{"schemas/a.json", "b.json", "schemas/b.json", true},
		{"schemas/x/a.json", "../b.json#/$defs/y", "schemas/b.json", true},
		{"schemas/a.json", "#/$defs/y", "schemas/a.json", true},
		{"schemas/a.json", "https://factory.ccf.dev/schemas/v1alpha1/result.schema.json", "", false},
		{"schemas/a.json", "https://catalog.invalid/schemas/c.json", "schemas/c.json", true},
	}
	for _, tc := range tests {
		got, ok := catalogRef(tc.from, tc.ref)
		if got != tc.want || ok != tc.ok {
			t.Errorf("catalogRef(%q, %q) = %q %v, want %q %v", tc.from, tc.ref, got, ok, tc.want, tc.ok)
		}
	}
}

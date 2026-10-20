package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// noLoader fails every load: the converted schema must compile from itself alone.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) { return nil, errors.New("no loading: " + url) }

// compileDraft7 compiles doc as draft-07 with nothing but doc itself known.
func compileDraft7(t *testing.T, doc string) *jsonschema.Schema {
	t.Helper()
	parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft7)
	c.UseLoader(noLoader{})
	const u = "mem:///draft7.json"
	if err := c.AddResource(u, parsed); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(u)
	if err != nil {
		t.Fatalf("the converted schema doesn't compile as draft-07: %v\n%s", err, doc)
	}
	return s
}

// compileOriginal compiles doc as the pipeline does: draft 2020-12, the v1alpha1 schemas
// registered by their $id.
func compileOriginal(t *testing.T, doc []byte) *jsonschema.Schema {
	t.Helper()
	gen, err := v1alpha1.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	for n, b := range gen {
		d, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(v1alpha1.SchemaBaseURL+n, d); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	const u = "mem:///original.json"
	if err := c.AddResource(u, parsed); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(u)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// harnessSchemas are the schemas claude can be given: the Result schema and every catalog
// result schema, as the pipeline writes them (bundled).
func harnessSchemas(t *testing.T) map[string][2][]byte {
	t.Helper()
	gen, err := v1alpha1.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string][]byte{"generic": gen[v1alpha1.ResultSchemaFile]}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "catalog", "schemas", "results", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no catalog result schemas: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		docs[strings.TrimSuffix(filepath.Base(f), ".json")] = b
	}
	out := map[string][2][]byte{}
	for n, d := range docs {
		bundled, err := pipeline.BundleSchema(d)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = [2][]byte{d, bundled}
	}
	return out
}

// draft7Fixtures are Results across the base schema's shapes and the catalog's data.
var draft7Fixtures = []string{
	`{"task": "t", "status": "noop", "question": null}`,
	`{"task": "t", "status": "nope", "question": null}`,
	`{"task": "t", "status": "noop"}`,
	`{"task": "t", "status": "noop", "question": null, "extra": 1}`,
	`{"task": "t", "status": "noop", "question": null, "artifacts": [{"kind": "pr"}]}`,
	`{"task": "t", "status": "noop", "question": null, "artifacts": [{"url": "u"}]}`,
	`{"task": "t", "status": "noop", "question": null, "harness": {"name": "c", "effectiveConfig": {"model": "m"}, "usage": {"inputTokens": 1, "outputTokens": 2, "costUSD": 0.5, "wallClock": "1m"}}}`,
	`{"task": "t", "status": "noop", "question": null, "harness": {"name": "c", "effectiveConfig": {"model": "m"}, "usage": {"inputTokens": 1, "outputTokens": 2, "costUSD": "x"}}}`,
	`{"task": "t", "status": "done", "outcome": "approved", "question": null, "data": {"findings": {}, "prs": [{"pr": "acme/api#7", "event": "approve"}]}}`,
	`{"task": "t", "status": "done", "outcome": "approved", "question": null, "data": {"findings": {}, "prs": []}}`,
	`{"task": "t", "status": "done", "outcome": "pr-opened", "question": null, "data": {"pr": "acme/api#7", "branch": "factory/x", "train": "a/w#1", "repos": []}}`,
	`{"task": "t", "status": "done", "outcome": "pr-opened", "question": null, "data": {"pr": "not a pr"}}`,
	`{"task": "t", "status": "done", "outcome": "merged", "question": null, "data": {"merged": [{"pr": "acme/api#7", "sha": "0123456789abcdef0123456789abcdef01234567"}]}}`,
	`{"task": "t", "status": "done", "outcome": "decomposed", "question": null, "data": {"plan": [{"issue": "a/b#1", "title": "x"}]}}`,
}

func TestCompactSchemaDraft7(t *testing.T) {
	for name, docs := range harnessSchemas(t) {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "result.schema.json")
			if err := os.WriteFile(p, docs[1], 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := compactSchema(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, `"$schema"`) || strings.Contains(got, `"$id"`) || strings.Contains(got, "$defs") || strings.Contains(got, "\n") {
				t.Errorf("not converted: %s", got)
			}
			orig, conv := compileOriginal(t, docs[0]), compileDraft7(t, got)
			accepted, rejected := 0, 0
			for _, fx := range draft7Fixtures {
				inst, err := jsonschema.UnmarshalJSON(strings.NewReader(fx))
				if err != nil {
					t.Fatal(err)
				}
				want, have := orig.Validate(inst), conv.Validate(inst)
				if (want == nil) != (have == nil) {
					t.Errorf("%s: original %v, draft-07 %v", fx, want, have)
				}
				if want == nil {
					accepted++
				} else {
					rejected++
				}
			}
			if accepted == 0 || rejected == 0 {
				t.Errorf("the fixtures don't exercise the schema: %d accepted, %d rejected", accepted, rejected)
			}
		})
	}
}

func TestToDraft7(t *testing.T) {
	tests := []struct {
		name, in, want, wantErr string
	}{
		{"top-level $schema and $id dropped, nested $defs renamed",
			`{"$schema": "s", "$id": "i", "$defs": {"a": {"$defs": {"b": {}}, "$ref": "#/$defs/a/$defs/b"}}, "items": {"$ref": "#/$defs/a"}}`,
			`{"definitions":{"a":{"allOf":[{"$ref":"#/definitions/a/definitions/b"}],"definitions":{"b":{}}}},"items":{"$ref":"#/definitions/a"}}`, ""},
		{"a property named $defs is a name",
			`{"properties": {"$defs": {"$ref": "#/properties/$defs/properties/x"}, "x": {"const": {"$defs": 1}}}}`,
			`{"properties":{"$defs":{"$ref":"#/properties/$defs/properties/x"},"x":{"const":{"$defs":1}}}}`, ""},
		{"a $ref's siblings move under allOf",
			`{"$ref": "#/$defs/a", "required": ["x"], "allOf": [{"type": "object"}], "$defs": {"a": {}}}`,
			`{"allOf":[{"type":"object"},{"$ref":"#/definitions/a"}],"definitions":{"a":{}},"required":["x"]}`, ""},
		{"draft-07 keywords kept",
			`{"if": {"const": 1}, "then": {"oneOf": [{"type": "null"}, {"enum": [null]}]}}`,
			`{"if":{"const":1},"then":{"oneOf":[{"type":"null"},{"enum":[null]}]}}`, ""},
		{"2020-only keywords refused, named",
			`{"properties": {"a": {"prefixItems": []}, "b": {"unevaluatedProperties": false}}, "$defs": {"c": {"$anchor": "c", "dependentRequired": {}}}}`,
			"", "$anchor, dependentRequired, prefixItems, unevaluatedProperties"},
		{"a property named like one is a name",
			`{"properties": {"prefixItems": {"type": "string"}}, "const": {"$dynamicRef": "x"}}`,
			`{"const":{"$dynamicRef":"x"},"properties":{"prefixItems":{"type":"string"}}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal([]byte(tt.in), &doc); err != nil {
				t.Fatal(err)
			}
			err := toDraft7(doc)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Errorf("got  %s\nwant %s", b, tt.want)
			}
		})
	}
}

// TestToDraft7RefSiblings: a $ref's siblings still apply under a draft-07 validator.
func TestToDraft7RefSiblings(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(`{"$ref": "#/$defs/obj", "required": ["x"], "$defs": {"obj": {"type": "object"}}}`), &doc); err != nil {
		t.Fatal(err)
	}
	if err := toDraft7(doc); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	s := compileDraft7(t, string(b))
	for in, ok := range map[string]bool{`{"x": 1}`: true, `{}`: false, `1`: false} {
		inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(in))
		if err := s.Validate(inst); (err == nil) != ok {
			t.Errorf("%s: %v, want valid %v", in, err, ok)
		}
	}
}

func TestDraft7Ref(t *testing.T) {
	tests := map[string]string{
		"#":                                  "#",
		"#/$defs/a":                          "#/definitions/a",
		"#/$defs/factory-result/$defs/Usage": "#/definitions/factory-result/definitions/Usage",
		"#/$defs/$defs":                      "#/definitions/$defs",
		"#/properties/$defs":                 "#/properties/$defs",
		"#/allOf/0/$defs/x":                  "#/allOf/0/definitions/x",
		"https://x/y.json#/$defs/a":          "https://x/y.json#/$defs/a",
	}
	keys := make([]string, 0, len(tests))
	for k := range tests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, in := range keys {
		if got := draft7Ref(in); got != tests[in] {
			t.Errorf("draft7Ref(%q) = %q, want %q", in, got, tests[in])
		}
	}
}

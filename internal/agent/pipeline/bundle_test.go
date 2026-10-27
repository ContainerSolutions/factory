package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// noLoader fails every load: a bundled schema must compile from itself alone.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) { return nil, errors.New("no loading: " + url) }

// compileAlone compiles doc with nothing but doc itself known.
func compileAlone(t *testing.T, doc []byte) *jsonschema.Schema {
	t.Helper()
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	const u = "mem:///bundled.json"
	if err := c.AddResource(u, parsed); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(u)
	if err != nil {
		t.Fatalf("the bundled schema doesn't compile alone: %v\n%s", err, doc)
	}
	return s
}

// outputSchemas are the schemas a harness can be given: the Result schema and every
// catalog result schema.
func outputSchemas(t *testing.T) map[string][]byte {
	t.Helper()
	gen, err := v1alpha1Schemas()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{"generic": gen[v1alpha1.ResultSchemaFile]}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "catalog", "schemas", "results", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no catalog result schemas: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = b
	}
	return out
}

// resultFixtures are Results across the base schema's shapes and the catalog's data.
var resultFixtures = []string{
	`{"task": "t", "status": "noop", "question": null}`,
	`{"task": "t", "status": "failed", "question": null, "summary": "x"}`,
	`{"task": "t", "status": "nope", "question": null}`,
	`{"task": "t", "status": "noop"}`,
	`{"task": "t", "status": "noop", "question": null, "extra": 1}`,
	`{"task": "t", "status": "noop", "question": null, "artifacts": [{"kind": "pr", "url": "u"}]}`,
	`{"task": "t", "status": "noop", "question": null, "artifacts": [{"url": "u"}]}`,
	`{"task": "t", "status": "noop", "question": null, "suggest": [{"taskSpec": "s", "inputs": {"a": 1}}]}`,
	`{"task": "t", "status": "noop", "question": null, "suggest": [{"reason": "r"}]}`,
	`{"task": "t", "status": "noop", "question": null, "harness": {"name": "codex", "effectiveConfig": {"model": "m"}, "usage": {"inputTokens": 1, "outputTokens": 2, "costUSD": null, "wallClock": "1m"}}}`,
	`{"task": "t", "status": "noop", "question": null, "harness": {"name": "codex", "effectiveConfig": {}, "usage": {"inputTokens": 1, "outputTokens": 2, "costUSD": null}}}`,
	`{"task": "t", "status": "noop", "question": null, "harness": {"name": "codex", "effectiveConfig": {"model": "m"}, "usage": {"inputTokens": 1, "outputTokens": 2, "costUSD": "free"}}}`,
	`{"task": "t", "status": "done", "outcome": "approved", "question": null, "data": {"findings": {"posted": 2}, "prs": [{"pr": "acme/api#7", "event": "approve"}]}}`,
	`{"task": "t", "status": "done", "outcome": "approved", "question": null, "data": {"findings": {}, "prs": []}}`,
	`{"task": "t", "status": "done", "outcome": "pr-opened", "question": null, "data": {"pr": "acme/api#7", "branch": "factory/x", "train": "acme/workflows#1", "repos": [{"repo": "api", "action": "cut"}]}}`,
	`{"task": "t", "status": "done", "outcome": "pr-opened", "question": null, "data": {"pr": "not a pr"}}`,
	`{"task": "t", "status": "done", "outcome": "merged", "question": null, "data": {"merged": [{"pr": "acme/api#7", "sha": "0123456789abcdef0123456789abcdef01234567"}]}}`,
	`{"task": "t", "status": "done", "outcome": "merged", "question": null, "data": {"merged": []}}`,
	`{"task": "t", "status": "done", "outcome": "decomposed", "question": null, "data": {"plan": [{"issue": "a/b#1", "title": "x"}, {"issue": "a/b#2", "title": "y"}, {"issue": "a/b#3", "title": "z"}]}}`,
	`{"task": "t", "status": "done", "outcome": "decomposed", "question": null, "data": {"plan": [{"issue": "a/b#1", "title": "x"}]}}`,
}

// originalSchema compiles a schema as the pipeline does: the v1alpha1 schemas registered.
func originalSchema(t *testing.T, name string, doc []byte) *jsonschema.Schema {
	t.Helper()
	if name == "generic" {
		s, err := resultSchema()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s, err := compileSpecSchema(name, doc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBundleSchemaSelfContained(t *testing.T) {
	for name, doc := range outputSchemas(t) {
		t.Run(name, func(t *testing.T) {
			bundled, err := BundleSchema(doc)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(bundled, []byte(`"`+v1alpha1.SchemaBaseURL+v1alpha1.ResultSchemaFile+`"`)) && name != "generic" {
				t.Errorf("the bundled schema still refs the Result schema remotely:\n%s", bundled)
			}
			orig, got := originalSchema(t, name, doc), compileAlone(t, bundled)
			accepted, rejected := 0, 0
			for _, fx := range resultFixtures {
				inst, err := jsonschema.UnmarshalJSON(strings.NewReader(fx))
				if err != nil {
					t.Fatal(err)
				}
				want, have := orig.Validate(inst), got.Validate(inst)
				if (want == nil) != (have == nil) {
					t.Errorf("%s: original %v, bundled %v", fx, want, have)
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

func TestBundleSchema(t *testing.T) {
	base := v1alpha1.SchemaBaseURL + v1alpha1.ResultSchemaFile
	tests := []struct {
		name    string
		doc     string
		refs    []string // the refs of the bundled document, in walk order
		wantErr string
	}{
		{"no remote refs: unchanged", `{"type": "object", "properties": {"a": {"$ref": "#/$defs/A"}}, "$defs": {"A": {}}}`, nil, ""},
		{"the base, a fragment and the empty fragment",
			`{"allOf": [{"$ref": "` + base + `"}, {"$ref": "` + base + `#/$defs/Artifact"}, {"$ref": "` + base + `#"}]}`,
			[]string{"#/$defs/factory-result", "#/$defs/factory-result/$defs/Artifact", "#/$defs/factory-result"}, ""},
		{"a ref-shaped const is data", `{"allOf": [{"$ref": "` + base + `"}], "const": {"$ref": "https://elsewhere/x.json"}}`, []string{"#/$defs/factory-result"}, ""},
		{"a property named like a keyword", `{"allOf": [{"$ref": "` + base + `"}], "properties": {"enum": {"$ref": "#/$defs/factory-result/$defs/Usage"}}}`,
			[]string{"#/$defs/factory-result", "#/$defs/factory-result/$defs/Usage"}, ""},
		{"another remote ref", `{"allOf": [{"$ref": "` + base + `"}, {"$ref": "https://elsewhere/x.json"}]}`, nil, "https://elsewhere/x.json"},
		{"a relative ref", `{"$ref": "result.schema.json"}`, nil, "result.schema.json"},
		{"an anchor in the base", `{"$ref": "` + base + `#Artifact"}`, nil, "#Artifact"},
		{"a taken $defs key", `{"$ref": "` + base + `", "$defs": {"factory-result": {}}}`, nil, "already has"},
		{"$defs not an object", `{"$ref": "` + base + `", "$defs": []}`, nil, "not an object"},
		{"not JSON", `{`, nil, "the output schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BundleSchema([]byte(tt.doc))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.refs == nil {
				if string(got) != tt.doc {
					t.Errorf("changed:\n%s", got)
				}
				return
			}
			var doc map[string]any
			if err := json.Unmarshal(got, &doc); err != nil {
				t.Fatal(err)
			}
			defs := doc["$defs"].(map[string]any)
			res := defs["factory-result"].(map[string]any)
			if res["$id"] != nil || res["$schema"] != nil || res["title"] != "Result" {
				t.Errorf("the bundled base keeps its $id/$schema: %v", res)
			}
			delete(defs, "factory-result")
			var refs []string
			walkRefs(doc, func(r string) string { refs = append(refs, r); return r })
			if strings.Join(refs, " ") != strings.Join(tt.refs, " ") {
				t.Errorf("refs %q, want %q", refs, tt.refs)
			}
			var inner []string
			walkRefs(res, func(r string) string { inner = append(inner, r); return r })
			for _, r := range inner {
				if !strings.HasPrefix(r, "#/$defs/factory-result/$defs/") {
					t.Errorf("the base's ref %q isn't rebased", r)
				}
			}
			if len(inner) == 0 {
				t.Error("the base has no refs to rebase")
			}
		})
	}
}

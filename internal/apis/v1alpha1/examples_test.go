package v1alpha1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

// The examples are every YAML/JSON example in design/03-domain-model.md and design/10-poc.md,
// one document per file. Lines changed from the docs carry a "# changed from the doc" comment.
const examplesDir = "testdata/examples"

func exampleFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(examplesDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	return files
}

func isResultExample(path string) bool { return strings.HasSuffix(path, "-result.yaml") }

// TestExamplesRoundTrip decodes every example strictly, marshals it back and requires the
// output to be the same document (compared as generic maps) and to decode strictly again.
func TestExamplesRoundTrip(t *testing.T) {
	for _, path := range exampleFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var out []byte
			if isResultExample(path) {
				r, err := DecodeResult(data)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if out, err = marshalYAML(r); err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if _, err := DecodeResult(out); err != nil {
					t.Fatalf("re-decode: %v\n%s", err, out)
				}
			} else {
				obj, err := DecodeOne(data)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				wantKind := strings.TrimSuffix(filepath.Base(path)[6:], ".yaml")
				if got := strings.ToLower(obj.GetTypeMeta().Kind); got != wantKind {
					t.Fatalf("kind %q, file name says %q", got, wantKind)
				}
				if out, err = Encode(obj); err != nil {
					t.Fatalf("encode: %v", err)
				}
				if _, err := DecodeOne(out); err != nil {
					t.Fatalf("re-decode: %v\n%s", err, out)
				}
			}
			in, got := genericYAML(t, data), genericYAML(t, out)
			if !reflect.DeepEqual(in, got) {
				a, _ := json.MarshalIndent(in, "", "  ")
				b, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("round trip changed the document\n--- input\n%s\n--- output\n%s", a, b)
			}
		})
	}
}

// TestExamplesMatchSchemas validates every config-kind example and the Result example, both
// as written and as re-marshalled from the types, against the generated schemas.
func TestExamplesMatchSchemas(t *testing.T) {
	schemas := compileSchemas(t)
	for _, path := range exampleFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var (
			file  string
			typed any
		)
		if isResultExample(path) {
			file = ResultSchemaFile
			if typed, err = DecodeResult(data); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		} else {
			obj, err := DecodeOne(data)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if !IsConfigKind(obj.GetTypeMeta().Kind) {
				continue
			}
			file, typed = SchemaFile(obj.GetTypeMeta().Kind), obj
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			sch := schemas[file]
			if err := sch.Validate(schemaInstance(t, genericYAML(t, data))); err != nil {
				t.Errorf("example doesn't match %s: %v", file, err)
			}
			b, err := json.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("marshalled object doesn't match %s: %v\n%s", file, err, b)
			}
		})
	}
}

func compileSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	files, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	for name, b := range files {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := c.AddResource(SchemaBaseURL+name, doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	out := make(map[string]*jsonschema.Schema, len(files))
	for name := range files {
		s, err := c.Compile(SchemaBaseURL + name)
		if err != nil {
			t.Fatalf("compile %s: %v", name, err)
		}
		out[name] = s
	}
	return out
}

func schemaInstance(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// genericYAML decodes one YAML document into generic values (YAML 1.2), normalised through
// JSON so that numbers compare the same way on both sides (int vs float64).
func genericYAML(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

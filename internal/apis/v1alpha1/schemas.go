package v1alpha1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/invopop/jsonschema"
)

// SchemaBaseURL is the base of the generated schemas' $id.
const SchemaBaseURL = "https://factory.ccf.dev/schemas/v1alpha1/"

// ResultSchemaFile is the file name of the Result schema.
const ResultSchemaFile = "result.schema.json"

// SchemaFile returns the schema file name for a config kind, e.g. taskspec.schema.json.
func SchemaFile(kind string) string { return strings.ToLower(kind) + ".schema.json" }

// Schemas generates the JSON Schema of every config kind and of Result, keyed by file name.
// The output is deterministic and indented, with a trailing newline. Objects refuse
// additional properties, matching strict decoding.
func Schemas() (map[string][]byte, error) {
	out := make(map[string][]byte, len(configKinds)+1)
	for _, kind := range configKinds {
		obj, err := New(kind)
		if err != nil {
			return nil, err
		}
		s := reflectSchema(obj)
		props := s.Properties
		if props == nil {
			return nil, fmt.Errorf("schema for %s has no properties", kind)
		}
		if p, ok := props.Get("kind"); ok {
			p.Const = kind
		}
		if p, ok := props.Get("apiVersion"); ok {
			p.Const = APIVersion
		}
		s.Title = kind
		b, err := marshalSchema(s, SchemaFile(kind))
		if err != nil {
			return nil, fmt.Errorf("schema for %s: %w", kind, err)
		}
		out[SchemaFile(kind)] = b
	}
	s := reflectSchema(&Result{})
	s.Title = "Result"
	b, err := marshalSchema(s, ResultSchemaFile)
	if err != nil {
		return nil, fmt.Errorf("schema for Result: %w", err)
	}
	out[ResultSchemaFile] = b
	return out, nil
}

func reflectSchema(v any) *jsonschema.Schema {
	r := &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		Namer:          schemaTypeName,
	}
	return r.Reflect(v)
}

func marshalSchema(s *jsonschema.Schema, file string) ([]byte, error) {
	s.ID = jsonschema.ID(SchemaBaseURL + file)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// schemaTypeName names generic types without their package paths and brackets, so $defs
// keys stay valid URI fragments: ExprOr[…v1alpha1.HarnessRequest] → ExprOrHarnessRequest.
func schemaTypeName(t reflect.Type) string {
	n := t.Name()
	i := strings.IndexByte(n, '[')
	if i < 0 || !strings.HasSuffix(n, "]") {
		return ""
	}
	args := strings.Split(n[i+1:len(n)-1], ",")
	var b strings.Builder
	b.WriteString(n[:i])
	for _, a := range args {
		if j := strings.LastIndexByte(a, '.'); j >= 0 {
			a = a[j+1:]
		}
		b.WriteString(strings.TrimSpace(a))
	}
	return b.String()
}

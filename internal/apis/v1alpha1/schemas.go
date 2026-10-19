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

func reflectSchema(v any) *jsonschema.Schema { return NewReflector().Reflect(v) }

// NewReflector returns the JSON Schema reflector Schemas uses: anonymous (no $id), the root
// struct expanded, other named types under $defs, generic types named without their package
// paths (ExprOr[…v1alpha1.HarnessRequest] → ExprOrHarnessRequest), free-form maps open
// (freeFormMap). Other generators (the OpenAPI components in api/) use it so their schemas
// match these.
func NewReflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		Namer:          schemaTypeName,
		Mapper:         freeFormMap,
	}
}

// freeFormMap gives an unnamed string-keyed map of interface values (map[string]any) the
// schema {type: object, additionalProperties: true}. The reflector leaves
// additionalProperties unset for them, which code generators read as "no properties at all"
// (openapi-typescript: Record<string, never>). Maps with typed values already get the
// value's schema; every other type falls through (nil).
func freeFormMap(t reflect.Type) *jsonschema.Schema {
	if t.Kind() != reflect.Map || t.Name() != "" || t.Key().Kind() != reflect.String || t.Elem().Kind() != reflect.Interface {
		return nil
	}
	return &jsonschema.Schema{Type: "object", AdditionalProperties: jsonschema.TrueSchema}
}

// enumSchema is a string enum of the given values, in order.
func enumSchema[T ~string](values []T) *jsonschema.Schema {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = string(v)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
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

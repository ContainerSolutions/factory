package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/compliance-framework/factory/api"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// goTypes maps every x-go-type in the spec to a constructor of that type. A new x-go-type
// must be added here, so its examples are strictly decoded and its declared fields checked.
var goTypes = map[string]func() any{
	"v1alpha1.ObjectMeta":        func() any { return new(v1alpha1.ObjectMeta) },
	"v1alpha1.TaskMeta":          func() any { return new(v1alpha1.TaskMeta) },
	"v1alpha1.EventType":         func() any { return new(v1alpha1.EventType) },
	"v1alpha1.EffectOp":          func() any { return new(v1alpha1.EffectOp) },
	"v1alpha1.TaskPhase":         func() any { return new(v1alpha1.TaskPhase) },
	"v1alpha1.AgentPhase":        func() any { return new(v1alpha1.AgentPhase) },
	"v1alpha1.ProgressEvent":     func() any { return new(v1alpha1.ProgressEvent) },
	"v1alpha1.HarnessOffer":      func() any { return new(v1alpha1.HarnessOffer) },
	"v1alpha1.AgentCapabilities": func() any { return new(v1alpha1.AgentCapabilities) },
	"v1alpha1.Lending":           func() any { return new(v1alpha1.Lending) },
	"v1alpha1.HarnessRequest":    func() any { return new(v1alpha1.HarnessRequest) },
	"v1alpha1.StringOrList":      func() any { return new(v1alpha1.StringOrList) },
	"v1alpha1.Result":            func() any { return new(v1alpha1.Result) },
	"v1alpha1.Event":             func() any { return new(v1alpha1.Event) },
	"v1alpha1.WorkItem":          func() any { return new(v1alpha1.WorkItem) },
	"v1alpha1.Task":              func() any { return new(v1alpha1.Task) },
	"v1alpha1.Run":               func() any { return new(v1alpha1.Run) },
	"v1alpha1.Agent":             func() any { return new(v1alpha1.Agent) },
	"v1alpha1.Effect":            func() any { return new(v1alpha1.Effect) },
	"v1alpha1.Provider":          func() any { return new(v1alpha1.Provider) },
	"v1alpha1.Group":             func() any { return new(v1alpha1.Group) },
	"v1alpha1.Template":          func() any { return new(v1alpha1.Template) },
	"v1alpha1.Rule":              func() any { return new(v1alpha1.Rule) },
	"v1alpha1.RuleSet":           func() any { return new(v1alpha1.RuleSet) },
	"v1alpha1.TaskSpec":          func() any { return new(v1alpha1.TaskSpec) },
	"v1alpha1.Queue":             func() any { return new(v1alpha1.Queue) },
	"v1alpha1.Pool":              func() any { return new(v1alpha1.Pool) },
}

const v1alpha1Import = "github.com/compliance-framework/factory/internal/apis/v1alpha1"

func load(t *testing.T) (*openapi3.T, []api.Operation) {
	t.Helper()
	doc, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := api.Operations(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc, ops
}

func TestSpecVersion(t *testing.T) {
	doc, _ := load(t)
	// 3.0.3, not 3.1: see contracts.md §HTTP API (oapi-codegen 3.1 support needs a newer Echo).
	if doc.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", doc.OpenAPI)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "/api/v1" {
		t.Errorf("servers = %v, want one server /api/v1", doc.Servers)
	}
}

func TestOperations(t *testing.T) {
	doc, ops := load(t)
	tags := map[string]bool{}
	for _, tag := range doc.Tags {
		tags[tag.Name] = true
	}
	ids := map[string]bool{}
	for _, o := range ops {
		name := o.Method + " " + o.Path
		if o.ID == "" {
			t.Errorf("%s: no operationId", name)
		}
		if ids[o.ID] {
			t.Errorf("%s: duplicate operationId %q", name, o.ID)
		}
		ids[o.ID] = true
		if len(o.Tags) != 1 || !tags[o.Tags[0]] {
			t.Errorf("%s: tags %v, want exactly one declared tag", name, o.Tags)
			continue
		}
		if o.Operation.Security == nil {
			t.Errorf("%s: security must be explicit (use [] for none)", name)
		}
		if o.Operation.Responses.Default() == nil {
			t.Errorf("%s: no default (problem) response", name)
		}
		for code, ref := range o.Operation.Responses.Map() {
			if code[0] != '4' && code[0] != '5' && code != "default" {
				continue
			}
			if ref.Value == nil || ref.Value.Content.Get("application/problem+json") == nil || len(ref.Value.Content) != 1 {
				t.Errorf("%s: response %s must be application/problem+json only", name, code)
			}
		}
		var wantPrefix string
		switch o.Tags[0] {
		case "webhooks":
			wantPrefix = "/hooks/"
		case "ops":
			wantPrefix = "/"
		default:
			wantPrefix = "/api/v1/"
		}
		if !strings.HasPrefix(o.Path, wantPrefix) || (o.Tags[0] == "ops" && strings.HasPrefix(o.Path, "/api/")) {
			t.Errorf("%s: tag %s wants a path under %s", name, o.Tags[0], wantPrefix)
		}
	}
	if len(ops) == 0 {
		t.Fatal("no operations")
	}
}

// TestExamples checks that every operation with a success body has exactly one example, that
// the example validates against the response schema, and that its v1alpha1 parts strictly
// decode into the Go types.
func TestExamples(t *testing.T) {
	doc, ops := load(t)
	want := map[string]bool{}
	for _, o := range ops {
		t.Run(o.ID, func(t *testing.T) {
			_, err := fs.Stat(api.Examples, api.ExampleFile(o.ID))
			if !o.HasBody() {
				if err == nil {
					t.Fatalf("%s has no success body, so it must have no example", o.ID)
				}
				return
			}
			want[api.ExampleFile(o.ID)] = true
			if err != nil {
				t.Fatalf("missing example %s", api.ExampleFile(o.ID))
			}
			schema := o.Media.Schema
			if schema == nil || schema.Value == nil {
				t.Fatal("success response has no schema")
			}
			switch {
			case o.ContentType == api.MediaTypeSSE:
				checkFrames(t, doc, o)
			case api.IsJSON(o.ContentType):
				b, err := o.ExampleBody()
				if err != nil {
					t.Fatal(err)
				}
				checkJSON(t, doc, schema, b)
			default:
				text, err := o.ExampleText()
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Value.VisitJSON(text); err != nil {
					t.Fatalf("example does not match the schema: %v", err)
				}
				if o.ContentType == api.MediaTypeGzip {
					checkGzip(t, o, text)
				}
			}
		})
	}
	err := fs.WalkDir(api.Examples, api.ExamplesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !want[p] {
			t.Errorf("%s is not the example of an operation with a success body", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// checkGzip checks that a gzip example serves its text compressed, and that the text is JSON
// Lines (the transcript format).
func checkGzip(t *testing.T, o api.Operation, text string) {
	t.Helper()
	b, err := o.ExampleBody()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != text {
		t.Errorf("gunzipped body = %q, want %q", plain, text)
	}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not JSON: %q", i+1, line)
		}
	}
}

func checkFrames(t *testing.T, doc *openapi3.T, o api.Operation) {
	t.Helper()
	frames, err := o.ExampleFrames()
	if err != nil {
		t.Fatal(err)
	}
	events := sseEvents(t, doc, o.Media)
	seen := map[string]bool{}
	for i, f := range frames {
		s, ok := events[f.Event]
		if !ok {
			t.Errorf("frame %d: event %q is not in x-sse-events", i, f.Event)
			continue
		}
		seen[f.Event] = true
		checkJSON(t, doc, s, f.Data)
		if _, err := f.Encode(); err != nil {
			t.Errorf("frame %d: %v", i, err)
		}
	}
	for name := range events {
		if !seen[name] {
			t.Errorf("no example frame for event %q", name)
		}
	}
}

// sseEvents resolves the x-sse-events extension: event name → component schema.
func sseEvents(t *testing.T, doc *openapi3.T, media *openapi3.MediaType) map[string]*openapi3.SchemaRef {
	t.Helper()
	ext, ok := media.Extensions["x-sse-events"].(map[string]any)
	if !ok || len(ext) == 0 {
		t.Fatal("text/event-stream response needs x-sse-events")
	}
	out := map[string]*openapi3.SchemaRef{}
	for name, v := range ext {
		m, _ := v.(map[string]any)
		ref, _ := m["$ref"].(string)
		comp, ok := strings.CutPrefix(ref, "#/components/schemas/")
		s := doc.Components.Schemas[comp]
		if !ok || s == nil {
			t.Fatalf("x-sse-events %s: %q is not a component schema", name, ref)
		}
		out[name] = s
	}
	return out
}

func checkJSON(t *testing.T, doc *openapi3.T, schema *openapi3.SchemaRef, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("example is not JSON: %v", err)
	}
	if err := schema.Value.VisitJSON(v, openapi3.MultiErrors(), openapi3.EnableFormatValidation()); err != nil {
		t.Errorf("example does not match the schema: %v", err)
	}
	if err := decodeGoTypes(doc, schema, raw, "$"); err != nil {
		t.Errorf("strict decode: %v", err)
	}
}

// decodeGoTypes walks a value along its schema and strictly decodes every part whose schema
// is an x-go-type into that Go type.
func decodeGoTypes(doc *openapi3.T, ref *openapi3.SchemaRef, raw json.RawMessage, at string) error {
	if ref == nil || ref.Value == nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	s := ref.Value
	if name, ok := s.Extensions["x-go-type"].(string); ok {
		newT, ok := goTypes[name]
		if !ok {
			return fmt.Errorf("%s: x-go-type %s is not in goTypes", at, name)
		}
		if err := strictDecode(raw, newT()); err != nil {
			return fmt.Errorf("%s (%s): %w", at, name, err)
		}
		return nil
	}
	if len(s.OneOf) > 0 && s.Discriminator != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		var kind string
		if err := json.Unmarshal(obj[s.Discriminator.PropertyName], &kind); err != nil {
			return fmt.Errorf("%s: discriminator %s: %w", at, s.Discriminator.PropertyName, err)
		}
		target := s.Discriminator.Mapping[kind]
		for _, one := range s.OneOf {
			if one.Ref == target.Ref {
				return decodeGoTypes(doc, one, raw, at)
			}
		}
		return fmt.Errorf("%s: discriminator value %q has no mapping", at, kind)
	}
	if len(s.Properties) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		for name, p := range s.Properties {
			if err := decodeGoTypes(doc, p, obj[name], at+"."+name); err != nil {
				return err
			}
		}
	}
	if s.Items != nil {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		for i, item := range items {
			if err := decodeGoTypes(doc, s.Items, item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// strictDecode decodes like the v1alpha1 decoder: kinds through v1alpha1.DecodeOne (which
// also runs their Validate), Result through DecodeResult, other types with unknown fields
// rejected.
func strictDecode(raw []byte, v any) error {
	switch v.(type) {
	case v1alpha1.Object:
		obj, err := v1alpha1.DecodeOne(raw)
		if err != nil {
			return err
		}
		if reflect.TypeOf(obj) != reflect.TypeOf(v) {
			return fmt.Errorf("decoded a %T, want %T", obj, v)
		}
		return nil
	case *v1alpha1.Result:
		_, err := v1alpha1.DecodeResult(raw)
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// TestGoTypes checks every x-go-type schema: it is registered in goTypes, imports v1alpha1,
// and every property it declares is a JSON field of the Go type (recursively), with required
// properties never omitted on output. This keeps the described envelope from drifting from
// the Go types, which stay authoritative.
func TestGoTypes(t *testing.T) {
	doc, _ := load(t)
	seen := map[*openapi3.Schema]bool{}
	var walk func(ref *openapi3.SchemaRef, at string)
	walk = func(ref *openapi3.SchemaRef, at string) {
		// A $ref is checked as its own component, so errors name the component.
		if ref == nil || ref.Value == nil || seen[ref.Value] || (ref.Ref != "" && strings.ContainsAny(at, ".[{")) {
			return
		}
		s := ref.Value
		seen[s] = true
		if name, ok := s.Extensions["x-go-type"].(string); ok {
			imp, _ := s.Extensions["x-go-type-import"].(map[string]any)
			if imp["path"] != v1alpha1Import {
				t.Errorf("%s: x-go-type-import.path = %v, want %s", at, imp["path"], v1alpha1Import)
			}
			newT, ok := goTypes[name]
			if !ok {
				t.Errorf("%s: x-go-type %s is not in goTypes", at, name)
			} else {
				for _, e := range checkFields(reflect.TypeOf(newT()), s, at) {
					t.Error(e)
				}
			}
		}
		for name, p := range s.Properties {
			walk(p, at+"."+name)
		}
		walk(s.Items, at+"[]")
		for i, one := range s.OneOf {
			walk(one, fmt.Sprintf("%s.oneOf[%d]", at, i))
		}
		if s.AdditionalProperties.Schema != nil {
			walk(s.AdditionalProperties.Schema, at+"{}")
		}
	}
	names := make([]string, 0, len(doc.Components.Schemas))
	for name := range doc.Components.Schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		walk(doc.Components.Schemas[name], name)
	}
}

func checkFields(t reflect.Type, s *openapi3.Schema, at string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if s == nil {
		return nil
	}
	var errs []string
	switch t.Kind() {
	case reflect.Struct:
		if len(s.Properties) == 0 {
			return nil
		}
		fields := jsonFields(t)
		for name, p := range s.Properties {
			f, ok := fields[name]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: not a JSON field of %s", at, name, t))
				continue
			}
			errs = append(errs, checkFields(f.typ, p.Value, at+"."+name)...)
		}
		for _, name := range s.Required {
			if f, ok := fields[name]; ok && f.omitted {
				errs = append(errs, fmt.Sprintf("%s.%s: required, but %s omits it when empty", at, name, t))
			}
		}
	case reflect.Slice, reflect.Array:
		if s.Items != nil && t != reflect.TypeOf(json.RawMessage(nil)) {
			errs = append(errs, checkFields(t.Elem(), s.Items.Value, at+"[]")...)
		}
	case reflect.Map:
		if s.AdditionalProperties.Schema != nil {
			errs = append(errs, checkFields(t.Elem(), s.AdditionalProperties.Schema.Value, at+"{}")...)
		}
	}
	return errs
}

type jsonField struct {
	typ     reflect.Type
	omitted bool
}

// jsonFields lists a struct's JSON fields as encoding/json sees them, embedded structs inlined.
func jsonFields(t reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for k, v := range jsonFields(et) {
					out[k] = v
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		omit := slices.ContainsFunc(strings.Split(opts, ","), func(o string) bool { return o == "omitzero" || o == "omitempty" })
		out[name] = jsonField{typ: f.Type, omitted: omit}
	}
	return out
}

// TestVocabulary checks that the spec's enums are the v1alpha1 vocabulary.
func TestVocabulary(t *testing.T) {
	doc, _ := load(t)
	strs := func(vs ...any) []string {
		out := make([]string, 0, len(vs))
		for _, v := range vs {
			out = append(out, fmt.Sprint(v))
		}
		sort.Strings(out)
		return out
	}
	var events, ops []any
	for _, e := range v1alpha1.EventTypes() {
		events = append(events, e)
	}
	for _, o := range v1alpha1.EffectOps() {
		ops = append(ops, o)
	}
	var kinds []any
	for _, k := range v1alpha1.ConfigKinds() {
		kinds = append(kinds, k)
	}
	tests := []struct {
		schema string
		want   []string
	}{
		{"EventType", strs(events...)},
		{"EffectOp", strs(ops...)},
		{"CatalogKind", strs(kinds...)},
		{"TaskPhase", strs(v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskCancelling, v1alpha1.TaskCancelled)},
		{"AgentPhase", strs(v1alpha1.AgentReady, v1alpha1.AgentBusy, v1alpha1.AgentThrottled, v1alpha1.AgentDraining, v1alpha1.AgentOffline)},
	}
	for _, tt := range tests {
		t.Run(tt.schema, func(t *testing.T) {
			s := doc.Components.Schemas[tt.schema]
			if s == nil {
				t.Fatalf("no schema %s", tt.schema)
			}
			if got := strs(s.Value.Enum...); !slices.Equal(got, tt.want) {
				t.Errorf("enum = %v, want %v", got, tt.want)
			}
		})
	}
	status := doc.Components.Schemas["Result"].Value.Properties["status"].Value
	want := strs(v1alpha1.ResultDone, v1alpha1.ResultFailed, v1alpha1.ResultNeedsInput, v1alpha1.ResultNoop)
	if got := strs(status.Enum...); !slices.Equal(got, want) {
		t.Errorf("Result.status enum = %v, want %v", got, want)
	}
}

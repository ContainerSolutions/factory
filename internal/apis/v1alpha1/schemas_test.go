package v1alpha1

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSchemasDeterministic(t *testing.T) {
	a, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(ConfigKinds())+1 {
		t.Errorf("%d schemas, want one per config kind plus Result", len(a))
	}
	for name := range a {
		if !bytes.Equal(a[name], b[name]) {
			t.Errorf("%s differs between runs", name)
		}
	}
}

// mapFields is a struct with each kind of map the reflector handles.
type mapFields struct {
	Free   map[string]any            `json:"free"`
	Typed  map[string]string         `json:"typed"`
	Nested map[string]map[string]any `json:"nested"`
}

// TestReflectorMaps checks that a free-form map is an open object (code generators read an
// object with no additionalProperties as one with no properties at all) and that a typed
// map carries its value's schema.
func TestReflectorMaps(t *testing.T) {
	s := NewReflector().Reflect(&mapFields{})
	tests := []struct{ field, want string }{
		{"free", `{"additionalProperties":true,"type":"object"}`},
		{"typed", `{"additionalProperties":{"type":"string"},"type":"object"}`},
		{"nested", `{"additionalProperties":{"additionalProperties":true,"type":"object"},"type":"object"}`},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			p, ok := s.Properties.Get(tt.field)
			if !ok {
				t.Fatalf("no property %s", tt.field)
			}
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Errorf("got %s, want %s", b, tt.want)
			}
		})
	}
}

func TestSchemasRejectWhatDecodingRejects(t *testing.T) {
	schemas := compileSchemas(t)
	tests := []struct {
		name, file, doc string
		valid           bool
	}{
		{"minimal rule", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"set":{"a":1}}]}}`, true},
		{"unknown field", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[],"when":"x"}}`, false},
		{"wrong kind", "rule.schema.json", `{"kind":"Queue","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[]}}`, false},
		{"wrong apiVersion", "rule.schema.json", `{"apiVersion":"x/v1","kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[]}}`, false},
		{"unknown event type", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.exploded"],"actions":[]}}`, false},
		{"action with two members", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"set":{},"cancel":{"taskSpec":"t"}}]}}`, false},
		{"action with no member", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{}]}}`, false},
		{"templated harness", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"dispatch":{"taskSpec":"t","queue":"q","harness":"{{ params.harness }}"}}]}}`, true},
		{"CEL harness", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"dispatch":{"taskSpec":"t","queue":"q","harness":"=params.h"}}]}}`, true},
		{"bare harness string", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"dispatch":{"taskSpec":"t","queue":"q","harness":"codex"}}]}}`, false},
		{"effect args inline", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"effect":{"op":"label.add","labels":["x"]}}]}}`, true},
		{"effect args under args", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"effect":{"op":"label.add","args":{"labels":["x"]}}}]}}`, false},
		{"effect without op", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"effect":{"target":"primary"}}]}}`, false},
		{"effect target not a string", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"effect":{"op":"close","target":1}}]}}`, false},
		{"typed harness", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"dispatch":{"taskSpec":"t","queue":"q","harness":{"name":"codex","config":{"model":["a","b"]}}}}]}}`, true},
		{"unknown effect op", "taskspec.schema.json", `{"kind":"TaskSpec","metadata":{"name":"t"},"spec":{"effects":{"allow":["force-push"]}}}`, false},
		{"template text and blocks", "template.schema.json", `{"kind":"Template","metadata":{"name":"t"},"spec":{"bodies":{"github":"hi","slack":{"blocks":"a.json"}}}}`, true},
		{"group member, one source", "group.schema.json", `{"kind":"Group","metadata":{"name":"g"},"spec":{"members":[{"provider":"p","users":[]}]}}`, true},
		{"group member, no source", "group.schema.json", `{"kind":"Group","metadata":{"name":"g"},"spec":{"members":[{"provider":"p"}]}}`, false},
		{"group member, two sources", "group.schema.json", `{"kind":"Group","metadata":{"name":"g"},"spec":{"members":[{"provider":"p","team":"a/b","orgRole":{"org":"o","role":"admin"}}]}}`, false},
		{"group member, empty team", "group.schema.json", `{"kind":"Group","metadata":{"name":"g"},"spec":{"members":[{"provider":"p","team":""}]}}`, false},
		{"destination group with via and as", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"notify":{"template":"t","to":[{"group":"g","via":"slack-ccf","as":"dm"},{"channel":{"provider":"s","name":"#c"}},{"item":"all"}]}}]}}`, true},
		{"destination with no target", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"notify":{"template":"t","to":[{"via":"slack-ccf"}]}}]}}`, false},
		{"destination with two targets", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"notify":{"template":"t","to":[{"group":"g","item":"all"}]}}]}}`, false},
		{"result status enum", "result.schema.json", `{"task":"t","status":"done | failed","question":null}`, false},
		{"result question is required (nullable)", "result.schema.json", `{"task":"t","status":"noop"}`, false},
		{"minimal result", "result.schema.json", `{"task":"t","status":"noop","question":null}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(tt.doc)))
			if err != nil {
				t.Fatal(err)
			}
			err = schemas[tt.file].Validate(inst)
			if (err == nil) != tt.valid {
				t.Errorf("valid = %v, want %v: %v", err == nil, tt.valid, err)
			}
		})
	}
}

// TestExprPatternMatchesGo checks that the schema's expression pattern and IsExpression agree.
func TestExprPatternMatchesGo(t *testing.T) {
	sch := compileSchemas(t)["rule.schema.json"]
	for _, s := range []string{
		"{{ params.harness }}", "=params.h", "=", "{{}}", "claude-{{ p }}-5", "{{ a\nb }}",
		"codex", "", "a=b", "{{ open", "close }}", " =x", "{ {x} }",
	} {
		doc := map[string]any{"kind": "Rule", "metadata": map[string]any{"name": "r"}, "spec": map[string]any{
			"on":      []any{"issue.opened"},
			"actions": []any{map[string]any{"dispatch": map[string]any{"taskSpec": "t", "queue": "q", "harness": s}}},
		}}
		schemaOK := sch.Validate(schemaInstance(t, doc)) == nil
		if schemaOK != IsExpression(s) {
			t.Errorf("%q: schema accepts = %v, IsExpression = %v", s, schemaOK, IsExpression(s))
		}
	}
}

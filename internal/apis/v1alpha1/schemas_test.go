package v1alpha1

import (
	"bytes"
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
		{"typed harness", "rule.schema.json", `{"kind":"Rule","metadata":{"name":"r"},"spec":{"on":["issue.opened"],"actions":[{"dispatch":{"taskSpec":"t","queue":"q","harness":{"name":"codex","config":{"model":["a","b"]}}}}]}}`, true},
		{"unknown effect op", "taskspec.schema.json", `{"kind":"TaskSpec","metadata":{"name":"t"},"spec":{"effects":{"allow":["force-push"]}}}`, false},
		{"template text and blocks", "template.schema.json", `{"kind":"Template","metadata":{"name":"t"},"spec":{"bodies":{"github":"hi","slack":{"blocks":"a.json"}}}}`, true},
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

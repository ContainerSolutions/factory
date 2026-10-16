package main

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestConvert(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"ref", `{"$ref": "#/$defs/Task", "description": "dropped"}`, "$ref: '#/components/schemas/Task'\n"},
		{"const", `{"type": "string", "const": "Task"}`, "type: string\nenum:\n  - Task\n"},
		{"nullable scalar", `{"oneOf": [{"type": "string"}, {"type": "null"}]}`, "type: string\nnullable: true\n"},
		{"nullable ref", `{"oneOf": [{"$ref": "#/$defs/X"}, {"type": "null"}]}`, "nullable: true\nallOf:\n  - $ref: '#/components/schemas/X'\n"},
		{"any", `{"properties": {"raw": true}, "propertyNames": {"enum": ["a"]}}`, "properties:\n  raw: {}\n"},
		{"order", `{"required": ["a"], "type": "object", "description": "d"}`, "description: d\ntype: object\nrequired:\n  - a\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tt.in), &doc); err != nil {
				t.Fatal(err)
			}
			n := doc.Content[0]
			resetStyle(n)
			var b strings.Builder
			enc := yaml.NewEncoder(&b)
			enc.SetIndent(2)
			if err := enc.Encode(convert(n)); err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Errorf("got\n%s\nwant\n%s", b.String(), tt.want)
			}
		})
	}
}

func TestSplice(t *testing.T) {
	spec := "a:\n    " + beginMark + " x\n    # do not edit\n    Old: {}\n    " + endMark + " y\nb: 1\n"
	got, err := splice([]byte(spec), []byte("    New: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "a:\n    " + beginMark + " x\n    # do not edit\n    New: {}\n    " + endMark + " y\nb: 1\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := splice([]byte(strings.ReplaceAll(spec, endMark, "")), nil); err == nil {
		t.Error("missing END marker must fail")
	}
}

// TestGenerateIsDeterministic runs the generator twice.
func TestGenerateIsDeterministic(t *testing.T) {
	t.Chdir("../..") // AddGoComments reads ./internal/apis/v1alpha1
	a, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("two runs differ")
	}
}

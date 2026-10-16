// Command componentsgen writes the OpenAPI components of the v1alpha1 types into
// api/openapi.yaml, between its BEGIN GENERATED and END GENERATED markers (make generate).
//
// It reflects every kind the API carries with v1alpha1.NewReflector (the reflector behind
// schemas/*.schema.json), so the components match strict decoding, and converts the JSON
// Schema 2020-12 output to OpenAPI 3.0.3:
//   - $ref #/$defs/X → #/components/schemas/X, one component per named v1alpha1 type;
//   - const → a one-value enum; oneOf [T, null] → T with nullable: true;
//   - propertyNames (not in 3.0) is dropped; boolean schemas become {};
//   - siblings of $ref (descriptions) are dropped, as 3.0 ignores them.
//
// Every component gets x-go-type and x-go-type-import, so oapi-codegen aliases the v1alpha1
// type instead of generating a copy. Go doc comments become descriptions. The output is
// deterministic: components sorted by name, keys in a fixed order.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
	"go.yaml.in/yaml/v3"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

const (
	module     = "github.com/compliance-framework/factory"
	pkgDir     = "./internal/apis/v1alpha1"
	importPath = module + "/internal/apis/v1alpha1"
	beginMark  = "# ---- BEGIN GENERATED: v1alpha1 types"
	endMark    = "# ---- END GENERATED"
	indent     = "    " // components.schemas entries
)

// roots are the kinds the API returns or accepts. Every other v1alpha1 type it carries
// (Result, ObjectMeta, HarnessRequest, the enums, …) is reached from them.
var roots = []v1alpha1.Object{
	&v1alpha1.Event{}, &v1alpha1.WorkItem{}, &v1alpha1.Task{}, &v1alpha1.Run{}, &v1alpha1.Agent{}, &v1alpha1.Effect{},
	&v1alpha1.Provider{}, &v1alpha1.Group{}, &v1alpha1.Template{}, &v1alpha1.Rule{}, &v1alpha1.RuleSet{},
	&v1alpha1.TaskSpec{}, &v1alpha1.Queue{}, &v1alpha1.Pool{},
}

func main() {
	spec := flag.String("spec", "api/openapi.yaml", "the OpenAPI file to update")
	flag.Parse()
	if err := run(*spec); err != nil {
		fmt.Fprintln(os.Stderr, "componentsgen:", err)
		os.Exit(1)
	}
}

func run(spec string) error {
	components, err := generate()
	if err != nil {
		return err
	}
	old, err := os.ReadFile(spec)
	if err != nil {
		return err
	}
	out, err := splice(old, components)
	if err != nil {
		return fmt.Errorf("%s: %w", spec, err)
	}
	if bytes.Equal(out, old) {
		return nil
	}
	return os.WriteFile(spec, out, 0o644)
}

// generate returns the components, as YAML indented for components.schemas.
func generate() ([]byte, error) {
	r := v1alpha1.NewReflector()
	if err := r.AddGoComments(module, pkgDir); err != nil {
		return nil, fmt.Errorf("read v1alpha1 doc comments: %w", err)
	}
	goTypes := map[string]string{} // component name → Go type
	for _, root := range roots {
		collectTypes(r, reflect.TypeOf(root), goTypes)
	}

	components := map[string]*yaml.Node{}
	add := func(name string, n *yaml.Node) error {
		goType, ok := goTypes[name]
		if !ok {
			return fmt.Errorf("schema %s is not a v1alpha1 type", name)
		}
		n = convert(n)
		setGoType(r, n, goType)
		sortKeys(n)
		if prev, ok := components[name]; ok {
			a, errA := yaml.Marshal(prev)
			b, errB := yaml.Marshal(n)
			if err := errors.Join(errA, errB); err != nil {
				return err
			}
			if !bytes.Equal(a, b) {
				return fmt.Errorf("schema %s differs between kinds", name)
			}
		}
		components[name] = n
		return nil
	}
	for _, root := range roots {
		s := r.Reflect(root)
		name := reflect.TypeOf(root).Elem().Name()
		setKindConst(s, name)
		defs := s.Definitions
		s.Definitions, s.Version, s.ID = nil, "", ""
		n, err := toNode(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := add(name, n); err != nil {
			return nil, err
		}
		for defName, def := range defs {
			n, err := toNode(def)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", defName, err)
			}
			if err := add(defName, n); err != nil {
				return nil, err
			}
		}
	}

	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	doc := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range names {
		doc.Content = append(doc.Content, str(name), components[name])
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if line != "" {
			out.WriteString(indent + line)
		}
	}
	return out.Bytes(), nil
}

// setKindConst pins kind and apiVersion on a kind's root, as Schemas does for config kinds.
func setKindConst(s *jsonschema.Schema, kind string) {
	if p, ok := s.Properties.Get("kind"); ok {
		p.Const = kind
	}
	if p, ok := s.Properties.Get("apiVersion"); ok {
		p.Const = v1alpha1.APIVersion
	}
}

// collectTypes maps the schema name of every named v1alpha1 type reachable from t to its Go
// type expression (v1alpha1.ExprOr[v1alpha1.HarnessRequest]).
func collectTypes(r *jsonschema.Reflector, t reflect.Type, out map[string]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.PkgPath() == importPath && t.Name() != "" {
		name := r.Namer(t)
		if name == "" {
			name = t.Name()
		}
		if _, seen := out[name]; seen {
			return
		}
		out[name] = "v1alpha1." + strings.ReplaceAll(t.Name(), importPath+".", "v1alpha1.")
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Tag.Get("json") == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		collectTypes(r, f.Type, out)
	}
}

func toNode(s *jsonschema.Schema) (*yaml.Node, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	n := doc.Content[0]
	resetStyle(n)
	return n, nil
}

// resetStyle drops the JSON flow style and quoting, so the encoder writes block style and
// quotes only where a plain scalar would read back as another type.
func resetStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		resetStyle(c)
	}
}

func str(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

func get(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func del(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func set(m *yaml.Node, key string, v *yaml.Node) {
	if old := get(m, key); old != nil {
		*old = *v
		return
	}
	m.Content = append(m.Content, str(key), v)
}

// convert turns one JSON Schema 2020-12 node into OpenAPI 3.0.3, recursively.
func convert(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!bool" && n.Value == "true" {
		return &yaml.Node{Kind: yaml.MappingNode}
	}
	if n.Kind != yaml.MappingNode {
		return n
	}
	if ref := get(n, "$ref"); ref != nil {
		ref.Value = strings.Replace(ref.Value, "#/$defs/", "#/components/schemas/", 1)
		n.Content = []*yaml.Node{str("$ref"), ref}
		return n
	}
	if c := get(n, "const"); c != nil {
		del(n, "const")
		set(n, "enum", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{c}})
	}
	del(n, "propertyNames")
	if one := get(n, "oneOf"); one != nil {
		var rest []*yaml.Node
		nullable := false
		for _, m := range one.Content {
			if t := get(m, "type"); t != nil && t.Value == "null" && len(m.Content) == 2 {
				nullable = true
				continue
			}
			rest = append(rest, m)
		}
		if nullable {
			del(n, "oneOf")
			switch {
			case len(rest) == 1 && get(rest[0], "$ref") != nil:
				set(n, "allOf", &yaml.Node{Kind: yaml.SequenceNode, Content: rest})
			case len(rest) == 1:
				n.Content = append(n.Content, rest[0].Content...)
			default:
				set(n, "oneOf", &yaml.Node{Kind: yaml.SequenceNode, Content: rest})
			}
			set(n, "nullable", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		}
	}
	for _, key := range []string{"items", "additionalProperties", "not"} {
		if v := get(n, key); v != nil {
			*v = *convert(v)
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if v := get(n, key); v != nil {
			for i, c := range v.Content {
				v.Content[i] = convert(c)
			}
		}
	}
	if props := get(n, "properties"); props != nil {
		for i := 1; i < len(props.Content); i += 2 {
			props.Content[i] = convert(props.Content[i])
		}
	}
	sortKeys(n)
	return n
}

// setGoType adds x-go-type and x-go-type-import, and the type's doc comment as the
// description when the schema has none (types with their own JSONSchema method).
func setGoType(r *jsonschema.Reflector, n *yaml.Node, goType string) {
	set(n, "x-go-type", str(goType))
	set(n, "x-go-type-import", &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{str("path"), str(importPath)}})
	if get(n, "description") != nil {
		return
	}
	typeName, _, _ := strings.Cut(strings.TrimPrefix(goType, "v1alpha1."), "[")
	desc := r.CommentMap[importPath+"."+typeName]
	if desc == "" {
		desc = "The " + goType + " type."
	}
	set(n, "description", str(desc))
}

// keyOrder is the order schema keys are written in; others follow in their own order.
var keyOrder = []string{
	"x-go-type", "x-go-type-import", "description", "type", "format", "enum", "pattern", "nullable",
	"minLength", "minProperties", "maxProperties", "required", "properties", "additionalProperties",
	"items", "oneOf", "anyOf", "allOf", "not",
}

// sortKeys orders one schema mapping's keys by keyOrder (stable for the others).
func sortKeys(n *yaml.Node) {
	if n.Kind != yaml.MappingNode {
		return
	}
	rank := func(k string) int {
		for i, o := range keyOrder {
			if o == k {
				return i
			}
		}
		return len(keyOrder)
	}
	type pair struct{ k, v *yaml.Node }
	pairs := make([]pair, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		pairs = append(pairs, pair{n.Content[i], n.Content[i+1]})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return rank(pairs[i].k.Value) < rank(pairs[j].k.Value) })
	n.Content = n.Content[:0]
	for _, p := range pairs {
		n.Content = append(n.Content, p.k, p.v)
	}
}

// splice replaces the lines between the markers with the components.
func splice(spec, components []byte) ([]byte, error) {
	lines := strings.SplitAfter(string(spec), "\n")
	begin, end := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, beginMark):
			begin = i
		case strings.HasPrefix(t, endMark):
			end = i
		}
	}
	if begin < 0 || end < begin {
		return nil, errors.New("no BEGIN GENERATED … END GENERATED markers")
	}
	// The line after BEGIN is the do-not-edit comment.
	head := strings.Join(lines[:begin+2], "")
	tail := strings.Join(lines[end:], "")
	return []byte(head + string(components) + tail), nil
}

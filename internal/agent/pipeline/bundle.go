package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// bundledResultDef is the $defs key the v1alpha1 Result schema is bundled under.
const bundledResultDef = "factory-result"

// dataKeywords hold instance data, not subschemas: a "$ref" inside them is not a reference.
var dataKeywords = map[string]bool{"const": true, "enum": true, "default": true, "examples": true}

// schemaMaps map names to subschemas: their keys are names, not keywords.
var schemaMaps = map[string]bool{
	"properties": true, "patternProperties": true, "$defs": true, "definitions": true,
	"dependentSchemas": true, "dependencies": true,
}

// BundleSchema makes a task's output schema self-contained for a harness CLI, which can't
// resolve the v1alpha1 Result schema by its remote $id. When the document refs that schema
// (v1alpha1.SchemaBaseURL + ResultSchemaFile, with or without a fragment), the schema is
// copied into $defs["factory-result"] (without its $schema and $id) and those refs, and the
// copy's own internal refs, point there. A document that refs nothing remote is returned
// unchanged; any other remote $ref is an error. The pipeline's own validation still compiles
// the original documents.
func BundleSchema(doc []byte) ([]byte, error) {
	root, err := decodeSchema(doc)
	if err != nil {
		return nil, fmt.Errorf("the output schema: %w", err)
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return doc, nil // a boolean schema refs nothing
	}
	used := false
	var bad []string
	walkRefs(obj, func(ref string) string {
		local, isBase, ok := rebaseRef(ref, v1alpha1.SchemaBaseURL+v1alpha1.ResultSchemaFile)
		switch {
		case isBase && ok:
			used = true
			return local
		case isBase || !strings.HasPrefix(ref, "#"):
			bad = append(bad, ref)
		}
		return ref
	})
	if len(bad) > 0 {
		return nil, fmt.Errorf("the output schema has a $ref a harness can't resolve: %s", strings.Join(dedupe(bad), ", "))
	}
	if !used {
		return doc, nil
	}
	base, err := bundledBase()
	if err != nil {
		return nil, err
	}
	defs, _ := obj["$defs"].(map[string]any)
	switch {
	case obj["$defs"] == nil:
		defs = map[string]any{}
		obj["$defs"] = defs
	case defs == nil:
		return nil, errors.New("the output schema: $defs is not an object")
	}
	if _, taken := defs[bundledResultDef]; taken {
		return nil, fmt.Errorf("the output schema: $defs already has %q", bundledResultDef)
	}
	defs[bundledResultDef] = base
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		return nil, fmt.Errorf("the output schema: %w", err)
	}
	return out.Bytes(), nil
}

// bundledBase is the v1alpha1 Result schema as a $defs entry: no $schema or $id, its own
// refs rebased under #/$defs/factory-result.
func bundledBase() (map[string]any, error) {
	gen, err := v1alpha1Schemas()
	if err != nil {
		return nil, err
	}
	v, err := decodeSchema(gen[v1alpha1.ResultSchemaFile])
	if err != nil {
		return nil, fmt.Errorf("the Result schema: %w", err)
	}
	base, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the Result schema is not an object")
	}
	delete(base, "$schema")
	delete(base, "$id")
	var bad []string
	walkRefs(base, func(ref string) string {
		if !strings.HasPrefix(ref, "#") {
			bad = append(bad, ref)
			return ref
		}
		local, _, ok := rebaseRef(v1alpha1.SchemaBaseURL+v1alpha1.ResultSchemaFile+ref, v1alpha1.SchemaBaseURL+v1alpha1.ResultSchemaFile)
		if !ok {
			bad = append(bad, ref)
			return ref
		}
		return local
	})
	if len(bad) > 0 {
		return nil, fmt.Errorf("the Result schema has a $ref that can't be bundled: %s", strings.Join(bad, ", "))
	}
	return base, nil
}

// rebaseRef maps a $ref to the base schema (base, base#, base#/pointer) to its bundled
// location. isBase reports whether ref names the base; ok is false for a fragment that is
// not a JSON pointer (an anchor), which bundling can't rebase.
func rebaseRef(ref, base string) (local string, isBase, ok bool) {
	rest, found := strings.CutPrefix(ref, base)
	if !found || (rest != "" && rest[0] != '#') {
		return "", false, false
	}
	frag := strings.TrimPrefix(rest, "#")
	if frag != "" && frag[0] != '/' {
		return "", true, false
	}
	return "#/$defs/" + bundledResultDef + frag, true, true
}

// walkRefs calls f on every "$ref" string of a schema and replaces it by f's result. It
// doesn't descend into keywords whose values are data (const, enum, default, examples), and
// reads the keys of the keywords that map names to subschemas (properties, $defs, …) as
// names, not keywords.
func walkRefs(v any, f func(string) string) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			switch {
			case dataKeywords[k]:
			case schemaMaps[k]:
				if m, ok := t[k].(map[string]any); ok {
					for _, n := range sortedKeys(m) {
						walkRefs(m[n], f)
					}
				}
			case k == "$ref":
				if s, ok := t[k].(string); ok {
					t[k] = f(s)
				}
			default:
				walkRefs(t[k], f)
			}
		}
	case []any:
		for _, e := range t {
			walkRefs(e, f)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeSchema decodes one JSON document, numbers kept as written.
func decodeSchema(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

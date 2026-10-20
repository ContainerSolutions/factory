package claudecode

import (
	"fmt"
	"sort"
	"strings"
)

// draft2019Keywords exist only in drafts 2019-09 and 2020-12: a draft-07 validator would
// ignore them, so a schema using one can't be converted without changing its meaning.
var draft2019Keywords = map[string]bool{
	"prefixItems": true, "unevaluatedItems": true, "unevaluatedProperties": true,
	"dependentRequired": true, "dependentSchemas": true, "minContains": true, "maxContains": true,
	"$anchor": true, "$dynamicRef": true, "$dynamicAnchor": true, "$recursiveRef": true, "$recursiveAnchor": true,
}

// dataKeywords hold instance data, not subschemas.
var dataKeywords = map[string]bool{"const": true, "enum": true, "default": true, "examples": true}

// nameMaps map names to subschemas: their keys are names, not keywords.
var nameMaps = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependencies": true}

// toDraft7 converts a (self-contained, draft 2020-12) Result schema to the draft-07 dialect
// claude's --json-schema validator takes: no top-level $schema or $id, every $defs renamed
// definitions with the #/$defs/ segments of $ref values rewritten to match, and a $ref with
// sibling keywords wrapped in allOf (draft-07 ignores a $ref's siblings). Everything else
// (const, if/then, oneOf, null) means the same in draft-07. A schema that uses a draft
// 2019/2020-only keyword is refused, naming them, rather than validated differently.
func toDraft7(doc map[string]any) error {
	found := map[string]bool{}
	scanKeywords(doc, found)
	if len(found) > 0 {
		names := make([]string, 0, len(found))
		for k := range found {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("the Result schema uses keywords draft-07 lacks: %s", strings.Join(names, ", "))
	}
	delete(doc, "$schema")
	delete(doc, "$id")
	convertSchema(doc)
	return nil
}

// scanKeywords records the draft 2019/2020-only keywords of a schema.
func scanKeywords(v any, found map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			switch {
			case dataKeywords[k]:
			case nameMaps[k]:
				if m, ok := c.(map[string]any); ok {
					for _, s := range m {
						scanKeywords(s, found)
					}
				}
			default:
				if draft2019Keywords[k] {
					found[k] = true
				}
				scanKeywords(c, found)
			}
		}
	case []any:
		for _, e := range t {
			scanKeywords(e, found)
		}
	}
}

// convertSchema rewrites one schema (and its subschemas) in place.
func convertSchema(v any) {
	switch t := v.(type) {
	case map[string]any:
		if d, ok := t["$defs"]; ok {
			delete(t, "$defs")
			t["definitions"] = d
		}
		if r, ok := t["$ref"].(string); ok {
			t["$ref"] = draft7Ref(r)
			if len(t) > 1 {
				ref := map[string]any{"$ref": t["$ref"]}
				delete(t, "$ref")
				if all, ok := t["allOf"].([]any); ok {
					t["allOf"] = append(all, ref) // appended: pointers into allOf keep their index
				} else {
					t["allOf"] = []any{ref}
				}
			}
		}
		for k, c := range t {
			switch {
			case dataKeywords[k]:
			case nameMaps[k]:
				if m, ok := c.(map[string]any); ok {
					for _, s := range m {
						convertSchema(s)
					}
				}
			default:
				convertSchema(c)
			}
		}
	case []any:
		for _, e := range t {
			convertSchema(e)
		}
	}
}

// draft7Ref rewrites a local ref's $defs segments: #/$defs/a/$defs/b →
// #/definitions/a/definitions/b. A segment that names a property or a definition (it follows
// properties, $defs, …) is a name and kept.
func draft7Ref(r string) string {
	if !strings.HasPrefix(r, "#/") {
		return r
	}
	segs := strings.Split(r[2:], "/")
	keyword := true
	for i, s := range segs {
		if keyword && s == "$defs" {
			segs[i] = "definitions"
		}
		keyword = !keyword || !nameMaps[segs[i]]
	}
	return "#/" + strings.Join(segs, "/")
}

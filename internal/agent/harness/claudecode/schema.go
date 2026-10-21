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

// rootCombinators can't be at the root of the schema claude gets: claude-code makes
// --json-schema the input_schema of its StructuredOutput tool, and the Messages API wants a
// tool's input_schema to be a plain object schema. A TaskSpec Result schema without a root
// "type" failed the first live run with `API Error: 400
// tools.7.custom.input_schema.type: Field required`, and the API refuses allOf, anyOf and
// oneOf at a tool schema's top level ("input_schema does not support oneOf, allOf, or anyOf
// at the top level"). Nested, they are fine.
var rootCombinators = map[string]bool{
	"allOf": true, "anyOf": true, "oneOf": true, "not": true,
	"if": true, "then": true, "else": true, "$ref": true,
}

// foldable are the keywords of an allOf member (or the definition a member $refs) that
// objectRoot can move to the root. A member with any other keyword is dropped whole.
var foldable = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"title": true, "description": true, "$comment": true, "definitions": true,
}

// annotations are copied to the root from the first folded member that has them, when the
// root has none of its own.
var annotations = []string{"title", "description", "$comment"}

// objectRoot rewrites a draft-07 Result schema (toDraft7's output) in place so its root is
// a plain object schema: "type": "object", none of rootCombinators. A TaskSpec Result
// schema is {"allOf": [{"$ref": <the bundled base Result>}, {if/then…}], "properties": …}:
//
//   - each top-level allOf member that is a $ref to a root definition (the base) or a plain
//     object schema (only foldable keywords) is folded into the root: its properties are
//     merged (a name both have becomes {"allOf": [member's, root's]}), its required lists
//     joined, its additionalProperties kept (several become one allOf), its annotations
//     taken when the root has none; its own definitions stay where they are, so the
//     #/definitions/… refs into them still resolve;
//   - the rest (if/then/else, anyOf, oneOf, not, any other allOf member) is dropped.
//
// The result accepts every instance the input accepts (it only drops constraints, and an
// additionalProperties now sees the merged properties), so it never stops the model from
// giving a valid Result; what it drops (the TaskSpecs' if/then rules) the pipeline still
// checks against the full schema, with its repair turn, and the dispatcher on complete. The
// schema claude gets is only the first line of defence.
//
// A root whose "type" isn't "object", or a $ref into the root's own keywords (other than
// #/definitions/…, which stay put), is refused: the rewrite would change what it means.
func objectRoot(doc map[string]any) error {
	if t, ok := doc["type"]; ok && t != "object" {
		return fmt.Errorf("the Result schema's root has type %v: a structured output must be an object", t)
	}
	var bad []string
	localRefs(doc, func(r string) {
		if strings.HasPrefix(r, "#") && !strings.HasPrefix(r, "#/definitions/") {
			bad = append(bad, r)
		}
	})
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("the Result schema has a $ref into its root, which claude's copy must restructure: %s", strings.Join(bad, ", "))
	}
	var members []map[string]any
	all, _ := doc["allOf"].([]any)
	if r, ok := doc["$ref"]; ok {
		all = append([]any{map[string]any{"$ref": r}}, all...)
	}
	for _, m := range all {
		if s := foldMember(doc, m); s != nil {
			members = append(members, s)
		}
	}
	for k := range rootCombinators {
		delete(doc, k)
	}
	own := map[string]any{}
	for _, k := range []string{"properties", "required", "additionalProperties"} {
		if v, ok := doc[k]; ok {
			own[k] = v
		}
	}
	members = append(members, own) // the root's own come last: {"allOf": [base's, TaskSpec's]}

	props := map[string][]any{}
	var names, required []string
	seen := map[string]bool{}
	var additional []any
	for _, m := range members {
		if p, ok := m["properties"].(map[string]any); ok {
			for _, n := range sortedNames(p) {
				if props[n] == nil {
					names = append(names, n)
				}
				props[n] = append(props[n], p[n])
			}
		}
		if r, ok := m["required"].([]any); ok {
			for _, e := range r {
				if s, ok := e.(string); ok && !seen[s] {
					seen[s] = true
					required = append(required, s)
				}
			}
		}
		if a, ok := m["additionalProperties"]; ok {
			additional = append(additional, a)
		}
	}
	for _, k := range annotations {
		for _, m := range members {
			if _, has := doc[k]; has {
				break
			}
			if v, ok := m[k]; ok {
				doc[k] = v
			}
		}
	}
	doc["type"] = "object"
	if len(names) > 0 {
		merged := make(map[string]any, len(names))
		for _, n := range names {
			merged[n] = allOf(props[n])
		}
		doc["properties"] = merged
	}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, s := range required {
			r[i] = s
		}
		doc["required"] = r
	}
	if len(additional) > 0 {
		doc["additionalProperties"] = allOf(additional)
	}
	return nil
}

// foldMember is the object schema an allOf member contributes to the root: the member, or
// the root definition it $refs, when it has only foldable keywords and an object type (or
// none); nil when it can't be folded and is dropped.
func foldMember(doc map[string]any, m any) map[string]any {
	s, ok := m.(map[string]any)
	if !ok {
		return nil
	}
	if r, ok := s["$ref"].(string); ok {
		if len(s) != 1 {
			return nil
		}
		s = definition(doc, r)
	}
	if s == nil || (s["type"] != nil && s["type"] != "object") {
		return nil
	}
	for k := range s {
		if !foldable[k] {
			return nil
		}
	}
	return s
}

// definition is the root definition #/definitions/<name> names, nil for any other ref.
func definition(doc map[string]any, ref string) map[string]any {
	name, ok := strings.CutPrefix(ref, "#/definitions/")
	if !ok || strings.Contains(name, "/") {
		return nil
	}
	name = strings.NewReplacer("~1", "/", "~0", "~").Replace(name)
	defs, _ := doc["definitions"].(map[string]any)
	d, _ := defs[name].(map[string]any)
	return d
}

// allOf is the one schema, else {"allOf": schemas}.
func allOf(schemas []any) any {
	if len(schemas) == 1 {
		return schemas[0]
	}
	return map[string]any{"allOf": schemas}
}

// localRefs calls f on every $ref string of a schema, skipping data keywords and reading
// name maps' keys as names.
func localRefs(v any, f func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			switch {
			case dataKeywords[k]:
			case nameMaps[k]:
				if m, ok := c.(map[string]any); ok {
					for _, s := range m {
						localRefs(s, f)
					}
				}
			case k == "$ref":
				if s, ok := c.(string); ok {
					f(s)
				}
			default:
				localRefs(c, f)
			}
		}
	case []any:
		for _, e := range t {
			localRefs(e, f)
		}
	}
}

func sortedNames(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

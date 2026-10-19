package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// draft202012 is the meta-schema a bundle declares.
const draft202012 = "https://json-schema.org/draft/2020-12/schema"

// bundleSchema returns the catalog schema at p as one self-contained document, for a reader
// that has no catalog (the agent, through the API's getTaskOutputSchema):
//
//   - p's raw bytes when it refers to no other catalog schema (the usual case: a TaskSpec's
//     schema refers only to the v1alpha1 Result, by its $id);
//   - otherwise a compound document (JSON Schema 2020-12 §9.3): $defs embed p and every
//     catalog schema it reaches, each with its catalog URL as $id (unless it has one), and
//     the root is a $ref to p, so the relative $refs resolve as they do at load.
//
// References to the v1alpha1 schemas stay references by their $id: every compiler that
// validates Results (the dispatcher's and the agent's) knows them.
func bundleSchema(sources map[string][]byte, p string) ([]byte, error) {
	docs := map[string]any{}
	queue := []string{p}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := docs[cur]; ok {
			continue
		}
		raw, ok := sources[cur]
		if !ok {
			return nil, fmt.Errorf("schema %s: not in the catalog", cur)
		}
		doc, err := decodeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", cur, err)
		}
		docs[cur] = doc
		for _, ref := range schemaRefs(doc) {
			if dep, ok := catalogRef(cur, ref); ok && dep != cur {
				if _, known := sources[dep]; known {
					queue = append(queue, dep)
				}
			}
		}
	}
	if len(docs) == 1 {
		return sources[p], nil
	}
	defs := make(map[string]any, len(docs))
	paths := make([]string, 0, len(docs))
	for path := range docs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		doc := docs[path]
		if m, ok := doc.(map[string]any); ok {
			if _, has := m["$id"]; !has {
				m["$id"] = assetBaseURL + path
			}
		}
		defs[path] = doc
	}
	b, err := json.Marshal(map[string]any{"$schema": draft202012, "$ref": assetBaseURL + p, "$defs": defs})
	if err != nil {
		return nil, fmt.Errorf("bundle schema %s: %w", p, err)
	}
	return b, nil
}

// decodeJSON decodes one JSON document, keeping numbers as written.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// schemaRefs lists the $ref and $dynamicRef values anywhere in doc.
func schemaRefs(doc any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && (k == "$ref" || k == "$dynamicRef") {
					out = append(out, s)
					continue
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	return out
}

// catalogRef resolves ref against the catalog schema at from and returns the catalog path it
// points to (without its fragment); false when it points outside the catalog.
func catalogRef(from, ref string) (string, bool) {
	base, err := url.Parse(assetBaseURL + from)
	if err != nil {
		return "", false
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(r)
	abs.Fragment = ""
	s := abs.String()
	if !strings.HasPrefix(s, assetBaseURL) {
		return "", false
	}
	return strings.TrimPrefix(s, assetBaseURL), true
}

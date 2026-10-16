package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// YAML is read as YAML 1.2 (go.yaml.in/yaml/v3): `on`, `yes` and `no` are strings, and only
// true/false are booleans. Each document goes YAML node → generic value → JSON → strict
// encoding/json decode, so the json tags are the only field names and unknown fields are
// errors everywhere, including inside custom UnmarshalJSON methods (strictJSON).

// maxYAMLNodes bounds the nodes a document may expand to through aliases.
const maxYAMLNodes = 1 << 20

// yamlDocument is one non-empty document of a stream.
type yamlDocument struct {
	line int // 1-based line of the document's root node
	json []byte
}

// yamlDocuments reads a YAML stream (documents separated by `---`, optionally ended by `...`)
// and converts each non-empty document to JSON. It stops at the first error; docs holds the
// documents read before it.
func yamlDocuments(data []byte) (docs []yamlDocument, err error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return docs, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null" {
			continue // empty, or comments only
		}
		budget := maxYAMLNodes
		v, err := yamlValue(root, &budget)
		if err != nil {
			return docs, &lineError{line: root.Line, err: err}
		}
		j, err := json.Marshal(v)
		if err != nil {
			return docs, &lineError{line: root.Line, err: err}
		}
		docs = append(docs, yamlDocument{line: root.Line, json: j})
	}
}

// lineError carries the line of the document an error happened in.
type lineError struct {
	line int
	err  error
}

func (e *lineError) Error() string { return e.err.Error() }
func (e *lineError) Unwrap() error { return e.err }

// yamlValue converts a node to the value encoding/json would decode from the same JSON:
// map[string]any, []any, string, bool, numbers or nil.
func yamlValue(n *yaml.Node, budget *int) (any, error) {
	if *budget--; *budget < 0 {
		return nil, errors.New("document too large (alias expansion)")
	}
	switch n.Kind {
	case yaml.AliasNode:
		return yamlValue(n.Alias, budget)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		lines := make(map[string]int, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			for k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				if k.ShortTag() == "!!merge" {
					return nil, fmt.Errorf("line %d: merge keys (<<) are not supported", k.Line)
				}
				return nil, fmt.Errorf("line %d: mapping key %q (%s) is not a string; quote it", k.Line, k.Value, k.ShortTag())
			}
			if first, dup := lines[k.Value]; dup {
				return nil, fmt.Errorf("line %d: mapping key %q already defined at line %d", k.Line, k.Value, first)
			}
			lines[k.Value] = k.Line
			v, err := yamlValue(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		l := make([]any, len(n.Content))
		for i, c := range n.Content {
			v, err := yamlValue(c, budget)
			if err != nil {
				return nil, err
			}
			l[i] = v
		}
		return l, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str", "!!timestamp": // timestamps stay as written; time fields parse them
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("line %d: %w", n.Line, err)
			}
			return v, nil
		default:
			return nil, fmt.Errorf("line %d: unsupported tag %s", n.Line, n.ShortTag())
		}
	default:
		return nil, fmt.Errorf("line %d: unexpected YAML node", n.Line)
	}
}

// unmarshalYAML strictly decodes a single-document YAML (or JSON) value into v.
func unmarshalYAML(data []byte, v any) error {
	docs, err := yamlDocuments(data)
	if err != nil {
		return err
	}
	if len(docs) != 1 {
		return fmt.Errorf("want exactly one document, got %d", len(docs))
	}
	return strictJSON(docs[0].json, v)
}

// marshalYAML writes v as block-style YAML with 2-space indents, keeping the struct field
// order and quoting only where a plain scalar would read back as another type.
func marshalYAML(v any) ([]byte, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(j, &n); err != nil {
		return nil, err
	}
	resetStyle(&n)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func resetStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		resetStyle(c)
	}
}

package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// segment is one document of a YAML stream: its bytes, padded with blank lines so that every
// line number (yaml.v3's, and v1alpha1.Decode's DocumentError.Line) is the line in the file.
type segment struct {
	start int // 1-based line of the segment's first line
	data  []byte
}

// splitDocuments splits a YAML stream at its document markers: a line that is `---` (alone or
// followed by a space or tab) starts a new document. YAML forbids such a line inside a
// document at column 0, so this is exact for valid streams; a malformed stream still fails
// when its segment is parsed.
func splitDocuments(data []byte) []segment {
	lines := bytes.SplitAfter(data, []byte("\n"))
	var out []segment
	start := 0
	flush := func(end int) {
		if end <= start {
			return
		}
		var b bytes.Buffer
		b.Write(bytes.Repeat([]byte("\n"), start))
		for _, l := range lines[start:end] {
			b.Write(l)
		}
		out = append(out, segment{start: start + 1, data: b.Bytes()})
	}
	for i, l := range lines {
		if i > start && isDocumentStart(l) {
			flush(i)
			start = i
		}
	}
	flush(len(lines))
	return out
}

func isDocumentStart(line []byte) bool {
	line = bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(line, []byte("---")) {
		return false
	}
	rest := line[3:]
	return len(rest) == 0 || rest[0] == ' ' || rest[0] == '\t'
}

// yamlLineRE finds the line number in a yaml.v3 error message ("yaml: line 7: …").
var yamlLineRE = regexp.MustCompile(`line (\d+)`)

// errorLine returns the first line number mentioned in err's message, or fallback.
func errorLine(err error, fallback int) int {
	if m := yamlLineRE.FindStringSubmatch(err.Error()); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// parseSegment parses a segment into its root node; nil for an empty document.
func parseSegment(s segment) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(s.data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null" {
		return nil, nil
	}
	return root, nil
}

// errNotPlain is returned by nodeValue for documents only v1alpha1.Decode should judge
// (merge keys, non-string keys, unusual tags): schema validation is skipped for them.
var errNotPlain = errors.New("document uses YAML features outside plain mappings, lists and scalars")

// nodeValue converts a node to the generic value the JSON Schema validator expects (as
// jsonschema.UnmarshalJSON would, with float64 numbers): maps, lists, strings, bools, nil.
// It follows the YAML 1.2 reading of v1alpha1 (only true/false are booleans).
func nodeValue(n *yaml.Node, budget *int) (any, error) {
	if *budget--; *budget < 0 {
		return nil, errNotPlain
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nodeValue(n.Alias, budget)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				return nil, errNotPlain
			}
			if _, dup := m[k.Value]; dup {
				return nil, errNotPlain
			}
			v, err := nodeValue(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		l := make([]any, len(n.Content))
		for i, c := range n.Content {
			v, err := nodeValue(c, budget)
			if err != nil {
				return nil, err
			}
			l[i] = v
		}
		return l, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str", "!!timestamp":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool":
			var b bool
			if err := n.Decode(&b); err != nil {
				return nil, errNotPlain
			}
			return b, nil
		case "!!int", "!!float":
			var f float64
			if err := n.Decode(&f); err != nil {
				return nil, errNotPlain
			}
			return f, nil
		}
	}
	return nil, errNotPlain
}

// fieldPath is a path into a document: map keys and list indexes.
type fieldPath []string

// at returns a copy of p extended with elems (strings are keys, ints are indexes).
func (p fieldPath) at(elems ...any) fieldPath {
	out := make(fieldPath, len(p), len(p)+len(elems))
	copy(out, p)
	for _, e := range elems {
		switch v := e.(type) {
		case int:
			out = append(out, "["+strconv.Itoa(v)+"]")
		case string:
			out = append(out, v)
		case fieldPath:
			out = append(out, v...)
		default:
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

// String renders the path as spec.actions[0].dispatch.
func (p fieldPath) String() string {
	var b strings.Builder
	for _, e := range p {
		if strings.HasPrefix(e, "[") {
			b.WriteString(e)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(e)
	}
	return b.String()
}

// parsePath splits a rendered path ("spec.actions[0].dispatch.queue") back into elements.
// Keys that contain dots or brackets can't round-trip; lookups then stop at the deepest
// element found.
func parsePath(s string) fieldPath {
	var out fieldPath
	for _, part := range strings.Split(s, ".") {
		for part != "" {
			i := strings.IndexByte(part, '[')
			switch {
			case i < 0:
				out = append(out, part)
				part = ""
			case i > 0:
				out = append(out, part[:i])
				part = part[i:]
			default:
				j := strings.IndexByte(part, ']')
				if j < 0 {
					out = append(out, part)
					part = ""
					continue
				}
				out = append(out, part[:j+1])
				part = part[j+1:]
			}
		}
	}
	return out
}

// pointerPath converts JSON Schema instance location tokens to a fieldPath.
func pointerPath(tokens []string, root *yaml.Node) fieldPath {
	var out fieldPath
	n := root
	for _, t := range tokens {
		for n != nil && n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		if n != nil && n.Kind == yaml.SequenceNode {
			if i, err := strconv.Atoi(t); err == nil {
				out = out.at(i)
				n = childAt(n, out[len(out)-1])
				continue
			}
		}
		out = append(out, t)
		n = childAt(n, t)
	}
	return out
}

// lineOf returns the line of the node at path in root: the key's line for a map entry, the
// element's line for a list element. When the path doesn't exist (e.g. a missing field), it
// returns the line of the deepest element that does.
func lineOf(root *yaml.Node, path fieldPath) int {
	if root == nil {
		return 0
	}
	line := root.Line
	n := root
	for _, e := range path {
		for n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		switch n.Kind {
		case yaml.MappingNode:
			found := false
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == e {
					line = n.Content[i].Line
					n = n.Content[i+1]
					found = true
					break
				}
			}
			if !found {
				return line
			}
		case yaml.SequenceNode:
			idx, ok := indexOf(e)
			if !ok || idx >= len(n.Content) {
				return line
			}
			n = n.Content[idx]
			line = n.Line
		default:
			return line
		}
	}
	return line
}

func childAt(n *yaml.Node, e string) *yaml.Node {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == e {
				return n.Content[i+1]
			}
		}
	case yaml.SequenceNode:
		if idx, ok := indexOf(e); ok && idx < len(n.Content) {
			return n.Content[idx]
		}
	}
	return nil
}

func indexOf(e string) (int, bool) {
	if !strings.HasPrefix(e, "[") || !strings.HasSuffix(e, "]") {
		return 0, false
	}
	i, err := strconv.Atoi(e[1 : len(e)-1])
	return i, err == nil && i >= 0
}

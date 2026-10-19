package effector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// maxInput bounds a text or JSON argument read from a file or stdin.
const maxInput = 1 << 20

// Kind is a parameter's type.
type Kind int

// Parameter kinds.
const (
	// String is a string; a JSON number is accepted too (PR numbers).
	String Kind = iota
	// Bool is a flag.
	Bool
	// List is a list of strings: an MCP array (or a comma-separated string), a repeatable
	// and comma-separated CLI flag.
	List
	// Text is a body: an MCP string; on the CLI --<flag> <text> or --<flag>-file <path>
	// ("-" is stdin).
	Text
	// JSON is a JSON value: the MCP value itself; on the CLI --<flag> <json> or
	// --<flag>-file <path>.
	JSON
)

// Param is one argument of an op: an MCP property named Name and a CLI flag named Flag
// (kebab-case Name by default), plus --<flag>-file for Text and JSON.
type Param struct {
	Name     string
	Flag     string
	Kind     Kind
	Required bool
	Enum     []string
	Help     string
	// Schema is a JSON param's JSON Schema.
	Schema map[string]any
}

// CLIFlag is the param's CLI flag name.
func (p Param) CLIFlag() string {
	if p.Flag != "" {
		return p.Flag
	}
	return kebab(p.Name)
}

// kebab turns camelCase into kebab-case.
func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsonSchema is the param's JSON Schema in the MCP tool's input schema.
func (p Param) jsonSchema() map[string]any {
	var s map[string]any
	switch p.Kind {
	case Bool:
		s = map[string]any{"type": "boolean"}
	case List:
		s = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case JSON:
		s = map[string]any{}
		for k, v := range p.Schema {
			s[k] = v
		}
	default:
		s = map[string]any{"type": "string"}
	}
	if len(p.Enum) > 0 {
		enum := make([]any, len(p.Enum))
		for i, v := range p.Enum {
			enum[i] = v
		}
		s["enum"] = enum
	}
	if p.Help != "" {
		s["description"] = p.Help
	}
	return s
}

// fileKey is the args key the CLI's --<flag>-file sets for a Text or JSON param.
func fileKey(name string) string { return name + "\x00file" }

// args are an op's parsed arguments: string, bool, []string, or any (JSON) by param name.
type args map[string]any

func (a args) str(name string) string {
	s, _ := a[name].(string)
	return s
}

func (a args) flag(name string) bool {
	b, _ := a[name].(bool)
	return b
}

func (a args) list(name string) []string {
	l, _ := a[name].([]string)
	return l
}

// parse checks raw arguments against the op's params: unknown names, types, required
// params and enums. raw holds MCP values (strings, numbers, booleans, arrays, objects) or
// what the CLI's flags set (strings, booleans, []any, json.RawMessage, fileKey paths).
// stdin is read for a "-" file (the CLI only).
func (o *Op) parse(raw map[string]any, stdin io.Reader) (args, error) {
	known := map[string]*Param{}
	for i := range o.Params {
		p := &o.Params[i]
		known[p.Name] = p
		if p.Kind == Text || p.Kind == JSON {
			known[fileKey(p.Name)] = p
		}
	}
	var unknown []string
	for k := range raw {
		if known[k] == nil {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, usagef("%s: unknown argument %s (known: %s)", o.Name(), strings.Join(unknown, ", "), strings.Join(o.paramNames(), ", "))
	}
	out := args{}
	for _, p := range o.Params {
		v, err := p.value(raw, stdin)
		if err != nil {
			return nil, usagef("%s: %s: %v", o.Name(), p.Name, err)
		}
		if v == nil {
			if p.Required {
				return nil, usagef("%s: %s is required", o.Name(), p.describe())
			}
			continue
		}
		if s, ok := v.(string); ok && len(p.Enum) > 0 && !slices.Contains(p.Enum, s) {
			return nil, usagef("%s: %s must be one of %s, not %q", o.Name(), p.Name, strings.Join(p.Enum, ", "), s)
		}
		out[p.Name] = v
	}
	return out, nil
}

func (o *Op) paramNames() []string {
	out := make([]string, len(o.Params))
	for i, p := range o.Params {
		out[i] = p.Name
	}
	return out
}

// describe names the param for a message: its MCP name and CLI flag.
func (p Param) describe() string {
	if p.Kind == Text || p.Kind == JSON {
		return fmt.Sprintf("%s (--%s or --%s-file)", p.Name, p.CLIFlag(), p.CLIFlag())
	}
	return fmt.Sprintf("%s (--%s)", p.Name, p.CLIFlag())
}

// value is the param's parsed value, nil when absent (or empty).
func (p Param) value(raw map[string]any, stdin io.Reader) (any, error) {
	v, has := raw[p.Name]
	file, hasFile := raw[fileKey(p.Name)]
	if has && hasFile {
		return nil, fmt.Errorf("give the value or a file, not both")
	}
	if hasFile {
		path, ok := file.(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("the file must be a path")
		}
		b, err := readInput(path, stdin)
		if err != nil {
			return nil, err
		}
		if p.Kind == Text {
			return emptyNil(string(b)), nil
		}
		return decodeJSON(b)
	}
	if !has || v == nil {
		return nil, nil
	}
	switch p.Kind {
	case Bool:
		switch b := v.(type) {
		case bool:
			return b, nil
		case string:
			x, err := strconv.ParseBool(b)
			if err != nil {
				return nil, fmt.Errorf("not a boolean: %q", b)
			}
			return x, nil
		}
		return nil, fmt.Errorf("not a boolean")
	case List:
		var out []string
		add := func(s string) {
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
		switch l := v.(type) {
		case string:
			add(l)
		case []string:
			for _, s := range l {
				add(s)
			}
		case []any:
			for _, x := range l {
				s, ok := x.(string)
				if !ok {
					return nil, fmt.Errorf("not a list of strings")
				}
				add(s)
			}
		default:
			return nil, fmt.Errorf("not a list of strings")
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	case JSON:
		if r, ok := v.(json.RawMessage); ok {
			return decodeJSON(r)
		}
		return v, nil
	}
	// String and Text; a String may be an integer (a PR number).
	if s, ok := v.(string); ok {
		return emptyNil(s), nil
	}
	if p.Kind == String {
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return strconv.FormatInt(i, 10), nil
			}
		case float64:
			if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
				return strconv.FormatInt(int64(n), 10), nil
			}
		case int:
			return strconv.Itoa(n), nil
		}
	}
	return nil, fmt.Errorf("not a string")
}

func emptyNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("more than one JSON value")
	}
	return v, nil
}

// readInput reads a file, or stdin for "-", up to maxInput bytes.
func readInput(path string, stdin io.Reader) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		if stdin == nil {
			return nil, fmt.Errorf(`"-" (stdin) works on the CLI only`)
		}
		r = stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInput {
		return nil, fmt.Errorf("over %d bytes", maxInput)
	}
	return b, nil
}

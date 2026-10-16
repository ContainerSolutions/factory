package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

// Duration is a duration as written in the catalog ("30s", "10m", "7d"). The original string
// is kept so documents round-trip exactly; Parse gives the time.Duration.
type Duration string

// Parse returns the duration. On top of time.ParseDuration it accepts a leading whole number
// of days: "7d", "1d12h".
func (d Duration) Parse() (time.Duration, error) {
	s := string(d)
	if s == "" {
		return 0, fmt.Errorf("duration: empty")
	}
	var days time.Duration
	if i := strings.IndexByte(s, 'd'); i >= 0 {
		n, err := strconv.ParseUint(s[:i], 10, 63)
		if err != nil || i == 0 {
			return 0, fmt.Errorf("duration %q: invalid number of days", s)
		}
		if n > uint64(math.MaxInt64/int64(24*time.Hour)) {
			return 0, fmt.Errorf("duration %q: out of range", s)
		}
		days = time.Duration(n) * 24 * time.Hour
		s = s[i+1:]
		if s == "" {
			return days, nil
		}
		if s[0] == '-' || s[0] == '+' {
			return 0, fmt.Errorf("duration %q: sign after days", string(d))
		}
	}
	rest, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", string(d), err)
	}
	if rest > math.MaxInt64-days {
		return 0, fmt.Errorf("duration %q: out of range", string(d))
	}
	return days + rest, nil
}

// String returns the duration as written.
func (d Duration) String() string { return string(d) }

// JSONSchema describes Duration as a string.
func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Description: `A Go duration ("90s", "15m", "1h30m"), optionally prefixed by whole days ("7d", "1d12h").`,
	}
}

// StringOrList is a field that takes one string (often a pattern, e.g. "*") or a list of
// strings. It remembers which form it was decoded from so it marshals back the same way.
type StringOrList struct {
	Values []string
	list   bool
}

// Scalar returns a StringOrList written as a single string.
func Scalar(s string) StringOrList { return StringOrList{Values: []string{s}} }

// List returns a StringOrList written as a list.
func List(values ...string) StringOrList {
	return StringOrList{Values: append([]string{}, values...), list: true}
}

// IsList reports whether the value was written (or built) as a list.
func (s StringOrList) IsList() bool { return s.list }

// IsZero reports whether the value is unset, so `omitzero` drops it.
func (s StringOrList) IsZero() bool { return s.Values == nil && !s.list }

// MarshalJSON writes a string when the value was a single string, a list otherwise.
func (s StringOrList) MarshalJSON() ([]byte, error) {
	if !s.list && len(s.Values) == 1 {
		return json.Marshal(s.Values[0])
	}
	if s.Values == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(s.Values)
}

// UnmarshalJSON accepts a string or a list of strings.
func (s *StringOrList) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*s = Scalar(v)
		return nil
	}
	var vs []string
	if err := json.Unmarshal(data, &vs); err != nil {
		return fmt.Errorf("want a string or a list of strings: %w", err)
	}
	if vs == nil {
		return errors.New("want a string or a list of strings, got null")
	}
	*s = StringOrList{Values: vs, list: true}
	return nil
}

// JSONSchema describes StringOrList as a string or a list of strings.
func (StringOrList) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string"},
		{Type: "array", Items: &jsonschema.Schema{Type: "string"}},
	}}
}

// ExprOr holds either an expression string or a typed value. It is used where the catalog
// writes a template that catalog load substitutes ("{{ params.harness }}"), or a "=CEL"
// value evaluated at runtime, in place of a structure. A string must look like one of the
// two (ExprPattern), so a bare value such as `harness: codex` is an error rather than an
// expression; checking the expression itself is the catalog loader's job.
type ExprOr[T any] struct {
	Expr  string
	Value T
}

// ExprPattern is what an expression string must match: it starts with "=" (CEL) or contains
// a "{{ … }}" template. The schema uses the same pattern.
const ExprPattern = `^=|\{\{[\s\S]*\}\}`

var exprRE = regexp.MustCompile(ExprPattern)

// IsExpression reports whether s is an expression string (matches ExprPattern).
func IsExpression(s string) bool { return exprRE.MatchString(s) }

// IsExpr reports whether the field holds an expression.
func (e ExprOr[T]) IsExpr() bool { return e.Expr != "" }

// MarshalJSON writes the expression string, or the value.
func (e ExprOr[T]) MarshalJSON() ([]byte, error) {
	if e.Expr != "" {
		return json.Marshal(e.Expr)
	}
	return json.Marshal(e.Value)
}

// UnmarshalJSON accepts a non-empty string (the expression) or the value, decoded strictly.
func (e *ExprOr[T]) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if !IsExpression(s) {
			return fmt.Errorf(`%q is not an expression: a string here must be a "{{ params.x }}" template or start with "=" (CEL); write the value as an object instead`, s)
		}
		var zero T
		*e = ExprOr[T]{Expr: s, Value: zero}
		return nil
	}
	var v T
	if err := strictJSON(data, &v); err != nil {
		return err
	}
	*e = ExprOr[T]{Value: v}
	return nil
}

// JSONSchema describes ExprOr as oneOf [string, T].
func (ExprOr[T]) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{Anonymous: true, DoNotReference: true}
	inner := r.ReflectFromType(reflect.TypeFor[T]())
	inner.Version = ""
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string", Pattern: ExprPattern, Description: `An expression: "{{ params.x }}" (substituted at catalog load) or "=CEL" (evaluated at runtime).`},
		inner,
	}}
}

func ptr[T any](v T) *T { return &v }

// strictJSON decodes data into v and rejects unknown fields. Custom UnmarshalJSON methods use
// it, because json.Decoder.DisallowUnknownFields doesn't reach inside them.
func strictJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("unexpected data after the value")
	}
	return nil
}

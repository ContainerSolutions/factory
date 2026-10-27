// Package patterns compiles the patterns allowed in harness config (design/03-domain-model.md
// §Patterns in harness config) and resolves a constraint, a request and an agent's offer into
// one concrete harness config.
//
// The package is pure: no I/O, no clock, no global mutable state.
package patterns

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// RegexPrefix opts a pattern into Go RE2 syntax, e.g. `re:^gpt-5(\.\d+)?-codex$`. The regex
// is used as written: it is not anchored for you.
const RegexPrefix = "re:"

// Pattern matches strings. Patterns are immutable and safe for concurrent use.
type Pattern interface {
	// Match reports whether s matches the pattern. Matching is case-sensitive.
	Match(s string) bool
	// String returns the pattern as written.
	String() string
}

// PatternError reports a pattern that does not compile.
type PatternError struct {
	Pattern string // the offending pattern (or list element) as written
	Reason  string // what is wrong with it
	Err     error  // the underlying regexp error, if any
}

func (e *PatternError) Error() string {
	return fmt.Sprintf("invalid pattern %q: %s", e.Pattern, e.Reason)
}

// Unwrap returns the underlying regexp error, if any.
func (e *PatternError) Unwrap() error { return e.Err }

// Compile compiles v, which is a string, a []string or a v1alpha1.StringOrList.
//
// A string is one of:
//   - `*`: matches anything;
//   - `re:<regex>`: a Go RE2 regex, anchored as written;
//   - a glob, if it contains `*`, `?`, `{`, `}` or `\`: `*` matches any run of characters,
//     `?` exactly one, `{a,b}` either alternative (alternatives may nest and contain globs),
//     `\` escapes the next character; the glob must match the whole string;
//   - otherwise an exact string.
//
// A list matches when any element does; earlier elements are preferred (see Resolve).
// Empty strings, empty lists, invalid UTF-8, bad regexes and unbalanced braces are reported as
// a *PatternError.
func Compile(v any) (Pattern, error) {
	switch v := v.(type) {
	case string:
		return compileString(v)
	case []string:
		return compileList(v)
	case v1alpha1.StringOrList:
		if v.IsList() {
			return compileList(v.Values)
		}
		if len(v.Values) != 1 {
			return nil, &PatternError{Pattern: strings.Join(v.Values, ","), Reason: "want one string"}
		}
		return compileString(v.Values[0])
	default:
		return nil, &PatternError{Pattern: fmt.Sprint(v), Reason: fmt.Sprintf("unsupported type %T", v)}
	}
}

// Validate reports whether v compiles; it is Compile for callers that only check the catalog.
func Validate(v any) error {
	_, err := Compile(v)
	return err
}

// Any returns the pattern that matches every string (`*`).
func Any() Pattern { return anyPattern{} }

func compileString(s string) (Pattern, error) {
	switch {
	case s == "":
		return nil, &PatternError{Pattern: s, Reason: "empty pattern"}
	case !utf8.ValidString(s):
		return nil, &PatternError{Pattern: s, Reason: "invalid UTF-8"}
	case s == "*":
		return anyPattern{}, nil
	case strings.HasPrefix(s, RegexPrefix):
		expr := strings.TrimPrefix(s, RegexPrefix)
		if expr == "" {
			return nil, &PatternError{Pattern: s, Reason: "empty regex"}
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, &PatternError{Pattern: s, Reason: "bad regex", Err: err}
		}
		return regexPattern{src: s, re: re}, nil
	case strings.ContainsAny(s, `*?{}\`):
		expr, err := globToRegexp(s)
		if err != nil {
			return nil, err
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, &PatternError{Pattern: s, Reason: "bad glob", Err: err}
		}
		return regexPattern{src: s, re: re}, nil
	default:
		return exactPattern(s), nil
	}
}

func compileList(vs []string) (Pattern, error) {
	if len(vs) == 0 {
		return nil, &PatternError{Pattern: "[]", Reason: "empty list"}
	}
	l := listPattern{elems: make([]Pattern, len(vs))}
	for i, v := range vs {
		p, err := compileString(v)
		if err != nil {
			return nil, err
		}
		l.elems[i] = p
	}
	return l, nil
}

// globToRegexp translates a glob into an anchored RE2 expression.
func globToRegexp(glob string) (string, error) {
	var b strings.Builder
	b.WriteString(`(?s)^(?:`)
	depth := 0
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '{':
			depth++
			b.WriteString(`(?:`)
		case '}':
			if depth == 0 {
				return "", &PatternError{Pattern: glob, Reason: "unbalanced braces: unexpected '}'"}
			}
			depth--
			b.WriteString(`)`)
		case ',':
			if depth > 0 {
				b.WriteString(`|`)
			} else {
				b.WriteString(`,`)
			}
		case '\\':
			if i+1 == len(glob) {
				return "", &PatternError{Pattern: glob, Reason: "trailing escape"}
			}
			_, size := utf8.DecodeRuneInString(glob[i+1:])
			b.WriteString(regexp.QuoteMeta(glob[i+1 : i+1+size]))
			i += size
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if depth != 0 {
		return "", &PatternError{Pattern: glob, Reason: "unbalanced braces: missing '}'"}
	}
	b.WriteString(`)$`)
	return b.String(), nil
}

type anyPattern struct{}

func (anyPattern) Match(string) bool { return true }
func (anyPattern) String() string    { return "*" }

type exactPattern string

func (p exactPattern) Match(s string) bool { return string(p) == s }
func (p exactPattern) String() string      { return string(p) }

// regexPattern backs both globs and `re:` regexes; src is the pattern as written.
type regexPattern struct {
	src string
	re  *regexp.Regexp
}

func (p regexPattern) Match(s string) bool { return p.re.MatchString(s) }
func (p regexPattern) String() string      { return p.src }

// listPattern matches when any element does; its order is a preference order.
type listPattern struct{ elems []Pattern }

func (l listPattern) Match(s string) bool { return l.rank(s) >= 0 }

func (l listPattern) String() string {
	parts := make([]string, len(l.elems))
	for i, e := range l.elems {
		parts[i] = e.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// rank is the index of the first element matching s, or -1.
func (l listPattern) rank(s string) int {
	for i, e := range l.elems {
		if e.Match(s) {
			return i
		}
	}
	return -1
}

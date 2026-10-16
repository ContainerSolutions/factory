package cel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ExprPrefix starts a runtime CEL value: "=event.object.ref".
const ExprPrefix = "="

// CELSource reports whether s is a "=CEL" value and returns its CEL source (s without the
// "="). A literal string that starts with "=" is written with a backslash, `\=` (in YAML
// double quotes, "\\=..."); it is not CEL.
func CELSource(s string) (src string, ok bool) {
	if strings.HasPrefix(s, ExprPrefix) {
		return s[len(ExprPrefix):], true
	}
	return "", false
}

// Unescape returns the literal a string stands for: one leading backslash is dropped from a
// string that is backslashes followed by "=" (`\=x` → `=x`, `\\=x` → `\=x`). Other strings are
// returned unchanged. EvalTree applies it to every literal string leaf.
func Unescape(s string) string {
	if strings.HasPrefix(s, `\`) && strings.HasPrefix(strings.TrimLeft(s, `\`), ExprPrefix) {
		return s[1:]
	}
	return s
}

// ProgramIndex holds the compiled "=" leaves of a catalog, keyed by CEL source (without the
// "="). It is filled by CompileTree at catalog load and read-only afterwards, so it is safe to
// share between goroutines.
type ProgramIndex map[string]*Program

// CompileTree compiles every "=CEL" string leaf of v (maps, lists and scalars, as decoded
// from YAML) as a Value and adds it to idx. Every failing leaf is reported: the result joins
// one *CompileError per leaf, with its Path set (map keys sorted).
func (e *Env) CompileTree(v any, idx ProgramIndex) error {
	var errs []error
	walkLeaves(v, "", func(path, s string) {
		src, ok := CELSource(s)
		if !ok {
			return
		}
		p, err := e.Compile(src, Value)
		if err != nil {
			var ce *CompileError
			if errors.As(err, &ce) {
				c := *ce
				c.Path = path
				err = &c
			}
			errs = append(errs, err)
			return
		}
		idx[src] = p
	})
	return errors.Join(errs...)
}

// walkLeaves calls fn for every string leaf of v, in a stable order.
func walkLeaves(v any, path string, fn func(path, s string)) {
	switch x := v.(type) {
	case string:
		fn(path, x)
	case map[string]any:
		for _, k := range sortedKeys(x) {
			walkLeaves(x[k], joinKey(path, k), fn)
		}
	case []any:
		for i, e := range x {
			walkLeaves(e, joinIndex(path, i), fn)
		}
	}
}

// EvalTree returns a copy of v in which every "=CEL" leaf is replaced by its value and every
// literal string is unescaped (Unescape); other values pass through unchanged. The programs
// come from progs. v itself is never modified.
//
// The first failing leaf (map keys sorted) stops the walk: its *EvalError has Path set, and is
// ErrNotCompiled when the leaf is not in progs. The rule engine skips the whole rule's actions
// on any error (LLD §Evaluation).
func EvalTree(ctx context.Context, v any, a Activation, progs ProgramIndex) (any, error) {
	return evalTree(ctx, v, "", a, progs)
}

func evalTree(ctx context.Context, v any, path string, a Activation, progs ProgramIndex) (any, error) {
	switch x := v.(type) {
	case string:
		src, ok := CELSource(x)
		if !ok {
			return Unescape(x), nil
		}
		p := progs[src]
		if p == nil {
			return nil, &EvalError{Code: CodeEval, Expr: src, Path: path, Message: "expression is not in the program index", Err: ErrNotCompiled}
		}
		out, err := p.EvalValue(ctx, a)
		if err != nil {
			var ee *EvalError
			if errors.As(err, &ee) {
				c := *ee
				c.Path = path
				return nil, &c
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return out, nil
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := make(map[string]any, len(x))
		for _, k := range sortedKeys(x) {
			ev, err := evalTree(ctx, x[k], joinKey(path, k), a, progs)
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	case []any:
		if x == nil {
			return x, nil
		}
		out := make([]any, len(x))
		for i, e := range x {
			ev, err := evalTree(ctx, e, joinIndex(path, i), a, progs)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	default:
		return v, nil
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

func joinKey(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func joinIndex(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

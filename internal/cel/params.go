package cel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// ErrParamType: a substituted value doesn't fit the field it lands in, e.g. a string param in
// dispatch.harness, which wants an object or an expression.
var ErrParamType = errors.New("substituted value has the wrong type")

var (
	placeholderRE = regexp.MustCompile(`\{\{([\s\S]*?)\}\}`)
	paramRefRE    = regexp.MustCompile(`^\s*params\.([A-Za-z_][A-Za-z0-9_]*)\s*$`)
)

// MergeParams returns the effective params of a rule in a RuleSet: the rule's declared params
// (with their defaults), then the RuleSet's defaults, then the RuleSet's overrides for the
// rule; later layers win, a whole top-level value at a time (catalog LLD: rule params ←
// ruleset defaults ← ruleset overrides[rule]). A default for a param the rule doesn't declare
// is ignored; an override for one is a *ParamError with ErrUnknownParam (one per key, joined).
// The inputs are not modified.
func MergeParams(declared, defaults, overrides map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(declared))
	for k, v := range declared {
		out[k] = v
	}
	for k, v := range defaults {
		if _, ok := declared[k]; ok {
			out[k] = v
		}
	}
	var errs []error
	for _, k := range sortedKeys(overrides) {
		if _, ok := declared[k]; !ok {
			errs = append(errs, &ParamError{Param: k, Message: fmt.Sprintf("override %q is not a declared param of the rule", k), Err: ErrUnknownParam})
			continue
		}
		out[k] = overrides[k]
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// Substitute returns a copy of v in which every "{{ params.<name> }}" in a string leaf is
// replaced:
//
//   - a string that is exactly one placeholder becomes the param's value, keeping its type
//     (harness: "{{ params.harness }}" becomes the harness object);
//   - otherwise each placeholder is replaced by the param formatted as text (strings as they
//     are, numbers and bools in their YAML form); a map, list or null param there is an
//     error (ErrInterpolate).
//
// Substituted text is not scanned again. Map keys are not substituted. A placeholder that is
// not "params.<name>" is ErrPlaceholder; an undeclared name is ErrUnknownParam. Every failing
// leaf is reported (joined *ParamError, Path set); v is not modified.
func Substitute(v any, params map[string]any) (any, error) {
	return substitute(v, "", params)
}

func substitute(v any, path string, params map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return substituteString(x, path, params)
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := make(map[string]any, len(x))
		var errs []error
		for _, k := range sortedKeys(x) {
			ev, err := substitute(x[k], joinKey(path, k), params)
			errs = append(errs, err)
			out[k] = ev
		}
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		return out, nil
	case []any:
		if x == nil {
			return x, nil
		}
		out := make([]any, len(x))
		var errs []error
		for i, e := range x {
			ev, err := substitute(e, joinIndex(path, i), params)
			errs = append(errs, err)
			out[i] = ev
		}
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return v, nil
	}
}

func substituteString(s, path string, params map[string]any) (any, error) {
	locs := placeholderRE.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s, nil
	}
	resolve := func(inner string) (string, any, error) {
		m := paramRefRE.FindStringSubmatch(inner)
		if m == nil {
			return "", nil, &ParamError{Path: path, Message: fmt.Sprintf("placeholder {{%s}} is not {{ params.<name> }}", inner), Err: ErrPlaceholder}
		}
		val, ok := params[m[1]]
		if !ok {
			return m[1], nil, &ParamError{Path: path, Param: m[1], Message: fmt.Sprintf("{{ params.%s }}: no such param", m[1]), Err: ErrUnknownParam}
		}
		return m[1], val, nil
	}
	if len(locs) == 1 && locs[0][0] == 0 && locs[0][1] == len(s) {
		_, val, err := resolve(s[locs[0][2]:locs[0][3]])
		if err != nil {
			return nil, err
		}
		return copyTree(val), nil
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		name, val, err := resolve(s[l[2]:l[3]])
		if err != nil {
			return nil, err
		}
		text, ok := formatParam(val)
		if !ok {
			return nil, &ParamError{Path: path, Param: name, Message: fmt.Sprintf("{{ params.%s }} is %s and cannot be embedded in a string", name, describe(val)), Err: ErrInterpolate}
		}
		b.WriteString(s[last:l[0]])
		b.WriteString(text)
		last = l[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// formatParam formats a scalar param for in-string interpolation.
func formatParam(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(x), true
	case json.Number:
		return x.String(), true
	default:
		return "", false
	}
}

func describe(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "a map"
	case []any:
		return "a list"
	default:
		return fmt.Sprintf("a %T", v)
	}
}

// copyTree deep-copies maps and lists, so substituted rules don't share param values.
func copyTree(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = copyTree(e)
		}
		return out
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyTree(e)
		}
		return out
	default:
		return v
	}
}

// SubstituteRule returns a copy of r whose spec.actions have had Substitute applied with
// params (normally MergeParams' result), decoded back strictly into v1alpha1 types and
// validated. So harness: "{{ params.harness }}" becomes an ExprOr holding the object, and a
// param of the wrong type for its field is a *ParamError with ErrParamType. Only the actions
// are substituted (LLD: "any string in a Rule's actions"); the other fields of the copy are
// shared with r, which is not modified.
func SubstituteRule(r *v1alpha1.Rule, params map[string]any) (*v1alpha1.Rule, error) {
	const path = "spec.actions"
	raw, err := json.Marshal(r.Spec.Actions)
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", path, err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", path, err)
	}
	tree, err = substitute(tree, path, params)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", path, err)
	}
	var actions []v1alpha1.Action
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&actions); err != nil {
		return nil, &ParamError{Path: path, Message: "after substitution: " + err.Error(), Err: fmt.Errorf("%w: %w", ErrParamType, err)}
	}
	out := *r
	out.Spec.Actions = actions
	if err := out.Validate(); err != nil {
		return nil, &ParamError{Path: path, Message: "after substitution: " + err.Error(), Err: fmt.Errorf("%w: %w", ErrParamType, err)}
	}
	return &out, nil
}

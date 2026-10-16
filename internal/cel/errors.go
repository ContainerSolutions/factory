package cel

import (
	"errors"
	"fmt"
	"strings"
)

// Code classifies an error for decisions and metrics ("error: <code>: <msg>").
type Code string

// Error codes.
const (
	// CodeParse: the expression is not valid CEL syntax.
	CodeParse Code = "cel.parse"
	// CodeCheck: the expression does not type-check against the environment (unknown variable
	// or function, wrong argument types, a where that cannot return bool).
	CodeCheck Code = "cel.check"
	// CodeEval: evaluation failed (missing key, wrong type, a function error).
	CodeEval Code = "cel.eval"
	// CodeLimit: evaluation exceeded the cost limit or the evaluation timeout.
	CodeLimit Code = "cel.limit"
	// CodeCanceled: the caller's context was done before evaluation finished.
	CodeCanceled Code = "cel.canceled"
)

// Sentinel causes, matched with errors.Is on an *EvalError.
var (
	// ErrMissing: a map key, attribute or list index is absent ("no such key", "index out of
	// bounds").
	ErrMissing = errors.New("missing key or attribute")
	// ErrType: an operator or function got a value of the wrong type ("no such overload"), or
	// a where returned something other than a bool, or a value is not a YAML value.
	ErrType = errors.New("wrong type")
	// ErrCostLimit: the evaluation cost went over the environment's cost limit.
	ErrCostLimit = errors.New("cost limit exceeded")
	// ErrTimeout: the evaluation ran longer than the environment's timeout.
	ErrTimeout = errors.New("evaluation timeout")
	// ErrNotCompiled: EvalTree found a "=" leaf that is not in the ProgramIndex.
	ErrNotCompiled = errors.New("expression not compiled")
)

// Issue is one parse or check problem, at a 1-based line and column of the expression
// (0 when cel-go gives no position).
type Issue struct {
	Line, Column int
	Message      string
}

func (i Issue) String() string {
	if i.Line == 0 {
		return i.Message
	}
	return fmt.Sprintf("%d:%d: %s", i.Line, i.Column, i.Message)
}

// CompileError reports an expression that does not parse (CodeParse) or type-check
// (CodeCheck).
type CompileError struct {
	Code Code
	// Expr is the CEL source, without the "=" prefix of a value leaf.
	Expr string
	// Path is the leaf's path in the tree given to CompileTree ("inputs.issue"); empty for
	// Compile.
	Path   string
	Issues []Issue
}

func (e *CompileError) Error() string {
	msgs := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		msgs[i] = is.String()
	}
	return fmt.Sprintf("%s%s: %q: %s", pathPrefix(e.Path), e.Code, e.Expr, strings.Join(msgs, "; "))
}

// EvalError reports an evaluation that failed. Err is the cause: one of the sentinels above
// (ErrMissing, ErrType, ErrCostLimit, ErrTimeout, ErrNotCompiled), the caller's context error
// for CodeCanceled, or cel-go's error otherwise.
type EvalError struct {
	Code Code
	Expr string
	// Path is the leaf's path in the tree given to EvalTree; empty for EvalBool/EvalValue.
	Path string
	// Line and Column locate the failing sub-expression (1-based; 0 when unknown).
	Line, Column int
	Message      string
	Err          error
}

func (e *EvalError) Error() string {
	pos := ""
	if e.Line > 0 {
		pos = fmt.Sprintf(" at %d:%d", e.Line, e.Column)
	}
	return fmt.Sprintf("%s%s: %q%s: %s", pathPrefix(e.Path), e.Code, e.Expr, pos, e.Message)
}

// Unwrap returns the cause.
func (e *EvalError) Unwrap() error { return e.Err }

// Sentinel causes of a *ParamError, matched with errors.Is.
var (
	// ErrUnknownParam: a placeholder or an override names a param the rule doesn't declare.
	ErrUnknownParam = errors.New("unknown param")
	// ErrPlaceholder: a "{{ … }}" that is not "{{ params.<name> }}".
	ErrPlaceholder = errors.New("invalid placeholder")
	// ErrInterpolate: a param embedded in a longer string is a map, a list or null.
	ErrInterpolate = errors.New("param cannot be interpolated into a string")
)

// ParamError reports a failed "{{ params.x }}" substitution or params merge.
type ParamError struct {
	// Path is where in the tree the failure is ("actions[0].dispatch.queue"); empty for a
	// merge error.
	Path string
	// Param is the param name, when known.
	Param   string
	Message string
	Err     error
}

func (e *ParamError) Error() string {
	return fmt.Sprintf("%sparams: %s", pathPrefix(e.Path), e.Message)
}

// Unwrap returns the sentinel cause.
func (e *ParamError) Unwrap() error { return e.Err }

// FieldError reports a field that the environment's allow-list doesn't document, e.g. a typo
// such as event.release.tagg.
type FieldError struct {
	Expr string
	// Field is the selection that is not allowed, e.g. "event.release.tagg" or
	// "item.pr.approvedBy[].identty".
	Field        string
	Line, Column int
}

func (e *FieldError) Error() string {
	pos := ""
	if e.Line > 0 {
		pos = fmt.Sprintf(" at %d:%d", e.Line, e.Column)
	}
	return fmt.Sprintf("cel.field: %q%s: unknown field %s", e.Expr, pos, e.Field)
}

func pathPrefix(p string) string {
	if p == "" {
		return ""
	}
	return p + ": "
}

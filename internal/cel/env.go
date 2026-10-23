// Package cel is the CEL environment of the factory's rules and the runtime expressions of
// the catalog (design/workstreams/dispatcher-core/LLD/cel-and-expressions.md):
//
//   - Env declares the typed environment (event, actor, item, run, approval, provider, params,
//     inputs, task, now) and the custom functions, and compiles expressions once (cached).
//   - Program evaluates a where (EvalBool) or a value (EvalValue) under the cost limit and
//     the evaluation timeout, returning typed errors (never panicking).
//   - CompileTree and EvalTree handle "=CEL" leaves in YAML trees (dispatch.inputs, set).
//   - MergeParams, Substitute and SubstituteRule do the "{{ params.x }}" substitution of
//     catalog load.
//
// The package is pure: no I/O and no clock. `now` comes from the caller in the Activation.
package cel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/interpreter"
)

// Limits (LLD §Compilation and limits).
const (
	// DefaultCostLimit is the runtime cost limit of one evaluation (cel.CostLimit).
	DefaultCostLimit uint64 = 10_000
	// DefaultTimeout bounds one evaluation's wall-clock time.
	DefaultTimeout = 50 * time.Millisecond
	// DefaultInterruptCheckFrequency is how many comprehension iterations run between checks
	// of the evaluation context (cel.InterruptCheckFrequency).
	DefaultInterruptCheckFrequency uint = 100
)

// Variable names of the environment.
const (
	VarEvent    = "event"
	VarActor    = "actor"
	VarItem     = "item"
	VarRun      = "run"
	VarApproval = "approval"
	VarProvider = "provider"
	VarParams   = "params"
	VarInputs   = "inputs"
	VarTask     = "task"
	VarNow      = "now"
)

// mapVars are the variables declared as map(string, dyn).
var mapVars = []string{VarEvent, VarActor, VarItem, VarRun, VarApproval, VarProvider, VarParams, VarInputs, VarTask}

// ExprKind is what an expression must produce.
type ExprKind int

const (
	// Where is a rule's where: it must produce a bool. An empty where is always true.
	Where ExprKind = iota + 1
	// Value is a "=CEL" value (action values, TaskSpec fields): any YAML value.
	Value
)

func (k ExprKind) String() string {
	switch k {
	case Where:
		return "where"
	case Value:
		return "value"
	default:
		return fmt.Sprintf("ExprKind(%d)", int(k))
	}
}

// Option configures an Env.
type Option func(*options)

type options struct {
	costLimit uint64
	timeout   time.Duration
	interrupt uint
}

// WithCostLimit sets the runtime cost limit of one evaluation (default DefaultCostLimit).
func WithCostLimit(n uint64) Option { return func(o *options) { o.costLimit = n } }

// WithTimeout sets the wall-clock limit of one evaluation (default DefaultTimeout).
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithInterruptCheckFrequency sets how often comprehensions check the evaluation context
// (default DefaultInterruptCheckFrequency).
func WithInterruptCheckFrequency(n uint) Option { return func(o *options) { o.interrupt = n } }

// Env is the CEL environment. It is safe for concurrent use. Compiled programs are cached
// per Env, keyed by source and kind; the cache is never evicted, so long-lived processes
// create one Env per catalog snapshot.
type Env struct {
	env  *cel.Env
	opts options

	mu    sync.RWMutex
	cache map[cacheKey]*Program
}

type cacheKey struct {
	kind ExprKind
	src  string
}

// NewEnv returns the environment of the LLD: the variables as map(string, dyn) (now is a
// timestamp), the CEL standard library and the custom functions item.count, item.lastRun,
// hash, semver.bump and glob, and cel-go's two-variable comprehensions (all, exists,
// existsOne, transformList, transformMap and transformMapEntry with an index or key and a
// value: a rule builds a map from a list with list.transformMapEntry(i, v, {k: v})). Numbers
// compare across int and double.
func NewEnv(opts ...Option) (*Env, error) {
	o := options{costLimit: DefaultCostLimit, timeout: DefaultTimeout, interrupt: DefaultInterruptCheckFrequency}
	for _, opt := range opts {
		opt(&o)
	}
	if o.timeout <= 0 {
		return nil, errors.New("cel: timeout must be positive")
	}
	envOpts := []cel.EnvOption{
		cel.CustomTypeAdapter(yamlAdapter{base: types.DefaultTypeAdapter}),
		cel.CrossTypeNumericComparisons(true),
		cel.Variable(VarNow, cel.TimestampType),
		ext.TwoVarComprehensions(),
	}
	for _, v := range mapVars {
		envOpts = append(envOpts, cel.Variable(v, cel.MapType(cel.StringType, cel.DynType)))
	}
	envOpts = append(envOpts, functions(newGlobCache())...)
	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("cel: new environment: %w", err)
	}
	return &Env{env: env, opts: o, cache: map[cacheKey]*Program{}}, nil
}

// Compile parses and type-checks src as kind, or returns the cached Program. Errors are
// *CompileError (CodeParse or CodeCheck); a Where whose type is known not to be bool is a
// CodeCheck error (a dyn result is checked when evaluated).
func (e *Env) Compile(src string, kind ExprKind) (*Program, error) {
	if kind != Where && kind != Value {
		return nil, fmt.Errorf("cel: invalid expression kind %v", kind)
	}
	key := cacheKey{kind: kind, src: src}
	e.mu.RLock()
	p := e.cache[key]
	e.mu.RUnlock()
	if p != nil {
		return p, nil
	}
	p, err := e.compile(src, kind)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if q := e.cache[key]; q != nil {
		return q, nil
	}
	e.cache[key] = p
	return p, nil
}

func (e *Env) compile(src string, kind ExprKind) (*Program, error) {
	p := &Program{env: e, kind: kind, src: src}
	if strings.TrimSpace(src) == "" {
		if kind == Where {
			return p, nil // always true
		}
		return nil, &CompileError{Code: CodeParse, Expr: src, Issues: []Issue{{Message: "empty expression"}}}
	}
	parsed, iss := e.env.Parse(src)
	if iss.Err() != nil {
		return nil, &CompileError{Code: CodeParse, Expr: src, Issues: issues(iss)}
	}
	checked, iss := e.env.Check(parsed)
	if iss.Err() != nil {
		return nil, &CompileError{Code: CodeCheck, Expr: src, Issues: issues(iss)}
	}
	if kind == Where {
		switch out := checked.OutputType(); out.Kind() {
		case types.BoolKind, types.DynKind, types.AnyKind:
		default:
			return nil, &CompileError{Code: CodeCheck, Expr: src, Issues: []Issue{{Message: fmt.Sprintf("where must return bool, not %s", out)}}}
		}
	}
	prg, err := e.env.Program(checked,
		cel.CostLimit(e.opts.costLimit),
		cel.InterruptCheckFrequency(e.opts.interrupt),
	)
	if err != nil {
		return nil, &CompileError{Code: CodeCheck, Expr: src, Issues: []Issue{{Message: err.Error()}}}
	}
	p.ast, p.prg = checked, prg
	return p, nil
}

func issues(iss *cel.Issues) []Issue {
	errs := iss.Errors()
	out := make([]Issue, 0, len(errs))
	for _, e := range errs {
		line, col := location(e.Location)
		out = append(out, Issue{Line: line, Column: col, Message: e.Message})
	}
	return out
}

// location turns a cel-go location (1-based line, 0-based column) into 1-based line and
// column, or 0, 0 when there is none.
func location(l common.Location) (line, col int) {
	if l == nil || l.Line() <= 0 {
		return 0, 0
	}
	return l.Line(), l.Column() + 1
}

// Activation is the input of one evaluation. The maps hold YAML/JSON values
// (map[string]any, []any, string, bool, numbers, nil); a nil map is an empty one. Now is the
// caller's clock reading.
type Activation struct {
	Event, Actor, Item, Run, Approval, Provider, Params, Inputs, Task map[string]any
	Now                                                               time.Time
}

func (a Activation) vars() map[string]any {
	return map[string]any{
		VarEvent: a.Event, VarActor: a.Actor, VarItem: a.Item, VarRun: a.Run,
		VarApproval: a.Approval, VarProvider: a.Provider, VarParams: a.Params,
		VarInputs: a.Inputs, VarTask: a.Task, VarNow: a.Now,
	}
}

// Program is a compiled expression. It is immutable and safe for concurrent use.
type Program struct {
	env  *Env
	kind ExprKind
	src  string
	ast  *cel.Ast    // nil for an empty where
	prg  cel.Program // nil for an empty where
}

// Source returns the expression as compiled (without a "=" prefix).
func (p *Program) Source() string { return p.src }

// Kind returns what the expression was compiled as.
func (p *Program) Kind() ExprKind { return p.kind }

// EvalBool evaluates a where. A result that is not a bool is an *EvalError with ErrType.
func (p *Program) EvalBool(ctx context.Context, a Activation) (bool, error) {
	if p.prg == nil {
		return true, nil
	}
	v, err := p.eval(ctx, a)
	if err != nil {
		return false, err
	}
	b, ok := v.(types.Bool)
	if !ok {
		return false, &EvalError{Code: CodeEval, Expr: p.src, Message: fmt.Sprintf("result is %s, want bool", v.Type().TypeName()), Err: ErrType}
	}
	return bool(b), nil
}

// EvalValue evaluates the expression and converts the result to a YAML value
// (map[string]any, []any, string, bool, int64, uint64, float64, nil, []byte, time.Time,
// time.Duration).
func (p *Program) EvalValue(ctx context.Context, a Activation) (any, error) {
	if p.prg == nil {
		return true, nil
	}
	v, err := p.eval(ctx, a)
	if err != nil {
		return nil, err
	}
	out, err := toNative(v)
	if err != nil {
		return nil, &EvalError{Code: CodeEval, Expr: p.src, Message: err.Error(), Err: err}
	}
	return out, nil
}

// eval runs the program under the timeout. It is the evaluation boundary: a panic in cel-go
// or a function becomes an *EvalError.
func (p *Program) eval(ctx context.Context, a Activation) (out ref.Val, err error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, &EvalError{Code: CodeCanceled, Expr: p.src, Message: cerr.Error(), Err: cerr}
	}
	ectx, cancel := context.WithTimeout(ctx, p.env.opts.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = &EvalError{Code: CodeEval, Expr: p.src, Message: fmt.Sprintf("internal error: %v", r), Err: fmt.Errorf("panic: %v", r)}
		}
	}()
	v, _, eerr := p.prg.ContextEval(ectx, a.vars())
	if eerr == nil {
		return v, nil
	}
	return nil, p.evalError(ctx, ectx, eerr)
}

// evalError classifies a cel-go evaluation error.
func (p *Program) evalError(ctx, ectx context.Context, err error) *EvalError {
	ee := &EvalError{Code: CodeEval, Expr: p.src, Message: err.Error(), Err: err}
	var te *types.Err
	if errors.As(err, &te) && te.NodeID() != 0 {
		ee.Line, ee.Column = location(p.ast.NativeRep().SourceInfo().GetStartLocation(te.NodeID()))
	}
	var ce interpreter.EvalCancelledError
	switch {
	case errors.As(err, &ce) && ce.Cause == interpreter.CostLimitExceeded:
		ee.Code, ee.Err = CodeLimit, fmt.Errorf("%w: %w", ErrCostLimit, err)
		ee.Message = fmt.Sprintf("cost limit %d exceeded", p.env.opts.costLimit)
	case ctx.Err() != nil:
		ee.Code, ee.Err = CodeCanceled, ctx.Err()
	case ectx.Err() != nil || errors.Is(err, interpreter.InterruptError{}) || errors.As(err, &ce):
		ee.Code, ee.Err = CodeLimit, fmt.Errorf("%w: %w", ErrTimeout, err)
		ee.Message = fmt.Sprintf("evaluation took longer than %s", p.env.opts.timeout)
	default:
		msg := err.Error()
		switch {
		case strings.Contains(msg, "no such key"), strings.Contains(msg, "no such attribute"), strings.Contains(msg, "no such field"),
			strings.Contains(msg, "index out of bounds"):
			ee.Err = fmt.Errorf("%w: %w", ErrMissing, err)
		case strings.Contains(msg, "no such overload"), strings.Contains(msg, "no matching overload"),
			strings.Contains(msg, "type conversion error"), strings.Contains(msg, "unsupported"):
			ee.Err = fmt.Errorf("%w: %w", ErrType, err)
		}
	}
	return ee
}

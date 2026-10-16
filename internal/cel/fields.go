package cel

import (
	"sort"

	"github.com/google/cel-go/checker"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
)

// node is one entry of the field allow-list: an object with documented fields, a list of
// elem, an open map (any key: state, inputs, params, …) or, with nothing set, a scalar.
type node struct {
	fields map[string]*node
	elem   *node
	open   bool
}

func obj(fields map[string]*node) *node { return &node{fields: fields} }
func list(elem *node) *node             { return &node{elem: elem} }
func open() *node                       { return &node{open: true} }

// scalar is a value nothing can be selected from.
var scalar = &node{}

func scalars(names ...string) map[string]*node {
	m := make(map[string]*node, len(names))
	for _, n := range names {
		m[n] = scalar
	}
	return m
}

func with(m map[string]*node, more map[string]*node) map[string]*node {
	for k, v := range more {
		m[k] = v
	}
	return m
}

// allowList is the documented shape of the variables (LLD §Environment). Values whose shape
// the LLD doesn't spell out (links, versions, run.agent, …) are open.
var allowList = func() map[string]*node {
	pr := obj(with(scalars("draft", "head", "unresolvedThreads"), map[string]*node{
		"author":     obj(scalars("isFactory")),
		"labels":     list(scalar),
		"checks":     obj(scalars("state", "settled")),
		"approvedBy": list(obj(scalars("identity", "commit"))),
	}))
	return map[string]*node{
		VarEvent: obj(with(scalars("id", "type", "source", "subject", "time", "repo", "text", "label"), map[string]*node{
			"object": obj(scalars("kind", "ref", "number")),
			"labels": list(scalar),
			"issue":  obj(with(scalars("body", "title"), map[string]*node{"labels": list(scalar)})),
			"release": obj(with(scalars("tag", "previousTag", "conclusion", "train"), map[string]*node{
				"semver": obj(scalars("major", "minor", "patch", "prerelease", "bump")),
			})),
			"review":   obj(scalars("state", "commit")),
			"workflow": obj(scalars("name", "conclusion")),
			"train": obj(with(scalars("month", "manifest", "dryRun"), map[string]*node{
				"repos": list(obj(with(scalars("name", "bump"), map[string]*node{"versions": open()}))),
			})),
		})),
		VarActor: obj(scalars("id", "tier", "isBot", "isFactory", "identity")),
		VarItem: obj(with(scalars("id"), map[string]*node{
			"state":    open(),
			"counters": open(),
			"runs":     open(),
			"links":    open(),
			"reporter": obj(scalars("id", "tier")),
			"pr":       pr,
			"prs":      list(pr),
			"deploy":   obj(scalars("state", "sha", "url", "containsMerge")),
		})),
		VarRun: obj(with(scalars("id", "spec", "outcome", "status"), map[string]*node{
			"inputs":  open(),
			"result":  open(),
			"agent":   open(),
			"harness": obj(scalars("name", "model")),
		})),
		VarApproval: obj(with(scalars("name", "phase"), map[string]*node{"by": open()})),
		VarProvider: obj(with(scalars("name", "type"), map[string]*node{"labels": open()})),
		VarParams:   open(),
		VarInputs:   open(),
		VarTask:     obj(with(scalars("id"), map[string]*node{"target": obj(scalars("repo"))})),
	}
}()

// Variables returns the environment variables the expression references, sorted (e.g. for
// "now in a where needs a schedule" and "TaskSpec fields only use inputs and task").
func (p *Program) Variables() []string {
	if p.ast == nil {
		return nil
	}
	w := &fieldWalker{p: p, vars: map[string]bool{}}
	w.visit(p.ast.NativeRep().Expr(), nil)
	out := make([]string, 0, len(w.vars))
	for v := range w.vars {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// UnknownFields returns the selections the field allow-list doesn't document, such as
// event.release.tagg or item.pr.approvedBy[].identty, in source order. Selections into open
// maps (item.state, run.inputs, params, …) are never reported; comprehension variables take
// the element shape of the list they range over.
func (p *Program) UnknownFields() []*FieldError {
	if p.ast == nil {
		return nil
	}
	w := &fieldWalker{p: p, vars: map[string]bool{}}
	w.visit(p.ast.NativeRep().Expr(), nil)
	return w.errs
}

type binding struct {
	n    *node
	path string
}

type scope struct {
	parent *scope
	name   string
	b      binding
}

func (s *scope) lookup(name string) (binding, bool) {
	for ; s != nil; s = s.parent {
		if s.name == name {
			return s.b, true
		}
	}
	return binding{}, false
}

type fieldWalker struct {
	p    *Program
	vars map[string]bool
	errs []*FieldError
}

// visit walks e and returns the allow-list node of its value (nil when untracked).
func (w *fieldWalker) visit(e ast.Expr, sc *scope) binding {
	switch e.Kind() {
	case ast.IdentKind:
		name := e.AsIdent()
		if b, ok := sc.lookup(name); ok {
			return b
		}
		if n, ok := allowList[name]; ok {
			w.vars[name] = true
			return binding{n: n, path: name}
		}
		if name == VarNow {
			w.vars[name] = true
		}
		return binding{}
	case ast.SelectKind:
		s := e.AsSelect()
		return w.field(e.ID(), w.visit(s.Operand(), sc), s.FieldName())
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			w.visit(c.Target(), sc)
		}
		args := make([]binding, len(c.Args()))
		for i, a := range c.Args() {
			args[i] = w.visit(a, sc)
		}
		if c.FunctionName() == operators.Index && len(args) == 2 {
			return w.index(e.ID(), args[0], c.Args()[1])
		}
		return binding{}
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			w.visit(el, sc)
		}
	case ast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			w.visit(me.Key(), sc)
			w.visit(me.Value(), sc)
		}
	case ast.StructKind:
		for _, f := range e.AsStruct().Fields() {
			w.visit(f.AsStructField().Value(), sc)
		}
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		rng := w.visit(c.IterRange(), sc)
		var elem binding
		if rng.n != nil {
			switch {
			case rng.n.open:
				elem = binding{n: open(), path: rng.path + "[]"}
			case rng.n.elem != nil:
				elem = binding{n: rng.n.elem, path: rng.path + "[]"}
			}
		}
		w.visit(c.AccuInit(), sc)
		inner := &scope{parent: sc, name: c.AccuVar()}
		if c.HasIterVar2() {
			inner = &scope{parent: &scope{parent: inner, name: c.IterVar()}, name: c.IterVar2(), b: elem}
		} else {
			inner = &scope{parent: inner, name: c.IterVar(), b: elem}
		}
		w.visit(c.LoopCondition(), inner)
		w.visit(c.LoopStep(), inner)
		w.visit(c.Result(), inner)
	}
	return binding{}
}

func (w *fieldWalker) field(id int64, parent binding, name string) binding {
	n := parent.n
	switch {
	case n == nil:
		return binding{}
	case n.open:
		return binding{n: open(), path: parent.path + "." + name}
	}
	if child, ok := n.fields[name]; ok {
		return binding{n: child, path: parent.path + "." + name}
	}
	w.report(id, parent.path+"."+name)
	return binding{}
}

func (w *fieldWalker) index(id int64, parent binding, key ast.Expr) binding {
	n := parent.n
	switch {
	case n == nil:
		return binding{}
	case n.open:
		return binding{n: open(), path: parent.path + "[]"}
	case n.elem != nil:
		return binding{n: n.elem, path: parent.path + "[]"}
	}
	if key.Kind() == ast.LiteralKind {
		if s, ok := key.AsLiteral().(types.String); ok {
			return w.field(id, parent, string(s))
		}
	}
	return binding{}
}

func (w *fieldWalker) report(id int64, field string) {
	line, col := location(w.p.ast.NativeRep().SourceInfo().GetStartLocation(id))
	w.errs = append(w.errs, &FieldError{Expr: w.p.src, Field: field, Line: line, Column: col})
}

// EstimateSizeHint is the size the cost estimate assumes for every list, map and string that
// comes from the activation (cel-go cannot know them statically).
const EstimateSizeHint = 100

type sizeHint struct{}

func (sizeHint) EstimateSize(checker.AstNode) *checker.SizeEstimate {
	return &checker.SizeEstimate{Min: 0, Max: EstimateSizeHint}
}

func (sizeHint) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// EstimateCost returns cel-go's static cost estimate (min, max) assuming every activation
// value has at most EstimateSizeHint elements or characters. The catalog compares max to the
// cost limit (CAT603); at runtime the actual cost is what the limit applies to.
func (p *Program) EstimateCost() (minCost, maxCost uint64, err error) {
	if p.ast == nil {
		return 0, 0, nil
	}
	est, err := p.env.env.EstimateCost(p.ast, sizeHint{})
	if err != nil {
		return 0, 0, err
	}
	return est.Min, est.Max, nil
}

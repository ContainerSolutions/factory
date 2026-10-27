package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/cel"
	"github.com/ContainerSolutions/factory/internal/patterns"
)

// report records one diagnostic at a path relative to whatever the caller is checking.
type report func(p fieldPath, code, msg string)

// ---- Phase 6: CEL ---------------------------------------------------------------------------

// checkCEL compiles every where, every "=" value of the rules' substituted actions and every
// "=" field of the TaskSpecs, and checks them against the field allow-list and cost limit.
func (v *validator) checkCEL() {
	for _, name := range v.ruleNames {
		r := v.rules[name]
		s := r.rule.Spec
		wp := r.path("spec", "where")
		at := func(p fieldPath, code, msg string) { v.errorf(r.doc, wp.at(p), code, "%s", msg) }
		prog, err := v.env.Compile(s.Where, cel.Where)
		if err != nil {
			at(nil, CodeWhere, "does not compile: "+err.Error())
		} else {
			r.where = prog
			v.checkProgram(at, prog)
			if slices.Contains(prog.Variables(), cel.VarNow) && s.Schedule == "" {
				v.warnf(r.doc, wp, CodeNowInWhere, "uses now without a schedule, so the rule isn't deterministic for replays and fixtures")
			}
		}
		if s.Debounce != nil && s.Debounce.Key != "" {
			src := s.Debounce.Key
			if cs, ok := cel.CELSource(src); ok {
				src = cs
			}
			kp := r.path("spec", "debounce", "key")
			v.compileValue(func(p fieldPath, code, msg string) { v.errorf(r.doc, kp.at(p), code, "%s", msg) }, src)
		}
	}
	for _, vw := range v.checkedViews() {
		for i, ca := range vw.actions {
			walkExprs(ca.Tree, actionPath(i, ca.Type), func(p fieldPath, src string) {
				v.compileValue(func(q fieldPath, code, msg string) { v.viewErr(vw, p.at(q), code, "%s", msg) }, src)
			})
		}
	}
	for _, name := range sortedKeys(v.taskSpecs) {
		e := v.taskSpecs[name]
		tree, err := toTree(e.ts.Spec)
		if err != nil {
			v.errorf(e.doc, fieldPath{"spec"}, CodeSchema, "%v", err)
			continue
		}
		m, _ := tree.(map[string]any)
		delete(m, "inputs") // input defaults are literals
		delete(m, "description")
		walkExprs(m, fieldPath{"spec"}, func(p fieldPath, src string) {
			prog := v.compileValue(func(q fieldPath, code, msg string) { v.errorf(e.doc, p.at(q), code, "%s", msg) }, src)
			if prog != nil {
				e.compiled.Expressions[p.String()] = prog
			}
		})
	}
}

// compileValue compiles a "=" value (CAT601), checks it (CAT602, CAT603) and adds it to the
// program index.
func (v *validator) compileValue(at report, src string) *cel.Program {
	prog, err := v.env.Compile(src, cel.Value)
	if err != nil {
		at(nil, CodeExpr, "does not compile: "+err.Error())
		return nil
	}
	v.checkProgram(at, prog)
	v.programs[src] = prog
	return prog
}

// checkProgram reports undocumented fields (CAT602) and costs over the limit (CAT603).
func (v *validator) checkProgram(at report, p *cel.Program) {
	for _, fe := range p.UnknownFields() {
		at(nil, CodeField, fe.Error())
	}
	_, maxCost, err := p.EstimateCost()
	switch {
	case err != nil:
		at(nil, CodeCost, "cost estimate: "+err.Error())
	case maxCost > cel.DefaultCostLimit:
		at(nil, CodeCost, fmt.Sprintf("%q: estimated cost %d is over the limit %d", p.Source(), maxCost, cel.DefaultCostLimit))
	}
}

// walkExprs calls fn for every "=CEL" string leaf of a YAML tree, in a stable order.
func walkExprs(v any, p fieldPath, fn func(p fieldPath, src string)) {
	switch x := v.(type) {
	case string:
		if src, ok := cel.CELSource(x); ok {
			fn(p, src)
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			walkExprs(x[k], p.at(k), fn)
		}
	case []any:
		for i, e := range x {
			walkExprs(e, p.at(i), fn)
		}
	}
}

// ---- Phase 7: dispatch ----------------------------------------------------------------------

// checkDispatch checks dispatch inputs against TaskSpec inputs, input defaults and requires
// labels.
func (v *validator) checkDispatch() {
	for _, name := range sortedKeys(v.taskSpecs) {
		e := v.taskSpecs[name]
		s := e.ts.Spec
		for _, k := range sortedKeys(s.Inputs) {
			if in := s.Inputs[k]; in.Default != nil {
				if msg := checkValue(in, in.Default, false); msg != "" {
					v.errorf(e.doc, fieldPath{"spec", "inputs", k, "default"}, CodeInputValue, "%s", msg)
				}
			}
		}
		if s.Requires != nil {
			v.checkLabels(func(p fieldPath, code, msg string) {
				v.errorf(e.doc, fieldPath{"spec", "requires", "labels"}.at(p), code, "%s", msg)
			}, s.Requires.Labels)
		}
	}
	for _, vw := range v.checkedViews() {
		for i, ca := range vw.actions {
			d := ca.Action.Dispatch
			if d == nil {
				continue
			}
			p := actionPath(i, "dispatch")
			if d.Requires != nil {
				v.checkLabels(func(q fieldPath, code, msg string) { v.viewErr(vw, p.at("requires", "labels").at(q), code, "%s", msg) }, d.Requires.Labels)
			}
			e := v.taskSpecs[d.TaskSpec]
			if e == nil {
				continue // CAT200
			}
			for _, pr := range checkInputs(e.ts.Spec.Inputs, d.Inputs, true) {
				v.viewErr(vw, p.at("inputs", pr.key), pr.code, "%s", pr.msg)
			}
		}
	}
}

var labelRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// checkLabels checks agent label names (CAT703): non-empty, at most 63 characters of
// letters, digits, '-', '_' and '.', starting and ending with a letter or digit.
func (v *validator) checkLabels(at report, labels []string) {
	for i, l := range labels {
		if len(l) > 63 || !labelRE.MatchString(l) {
			at(fieldPath{}.at(i), CodeRequires, fmt.Sprintf("%q is not a valid agent label", l))
		}
	}
}

// inputProblem is one problem with dispatch inputs.
type inputProblem struct {
	key, code, msg string
}

func (p inputProblem) Error() string { return p.key + ": " + p.msg }

// checkInputs checks inputs against the declared inputs. With exprAware, "=CEL" values are
// skipped (they're checked when the rule runs) and escaped literals are unescaped first.
func checkInputs(declared map[string]v1alpha1.InputSpec, given map[string]any, exprAware bool) []inputProblem {
	var out []inputProblem
	for _, k := range sortedKeys(given) {
		in, ok := declared[k]
		if !ok {
			msg := "not a declared input of the TaskSpec, which declares none"
			if len(declared) > 0 {
				msg = fmt.Sprintf("not a declared input of the TaskSpec (declared: %s)", names(declared))
			}
			out = append(out, inputProblem{k, CodeInputUnknown, msg})
			continue
		}
		if msg := checkValue(in, given[k], exprAware); msg != "" {
			out = append(out, inputProblem{k, CodeInputValue, msg})
		}
	}
	for _, k := range sortedKeys(declared) {
		in := declared[k]
		if _, ok := given[k]; !ok && in.Required && in.Default == nil {
			out = append(out, inputProblem{k, CodeInputMissing, "required input missing"})
		}
	}
	return out
}

// checkValue checks one input value against its declaration; "" when it's fine.
func checkValue(in v1alpha1.InputSpec, val any, exprAware bool) string {
	if s, ok := val.(string); ok && exprAware {
		if isExpr(s) {
			return ""
		}
		val = cel.Unescape(s)
	}
	if in.Type != "" && !hasType(in.Type, val) {
		return fmt.Sprintf("want %s, got %s", in.Type, show(val))
	}
	if len(in.Enum) > 0 && !slices.ContainsFunc(in.Enum, func(e any) bool { return reflect.DeepEqual(e, val) }) {
		return fmt.Sprintf("%s is not one of %s", show(val), show(in.Enum))
	}
	return ""
}

func hasType(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	}
	return true
}

func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// ---- Phase 8: harness -----------------------------------------------------------------------

// checkHarness compiles each TaskSpec's harness constraints and default request, then checks
// every dispatch.harness request against its TaskSpec.
func (v *validator) checkHarness() {
	for _, name := range sortedKeys(v.taskSpecs) {
		e := v.taskSpecs[name]
		h := e.ts.Spec.Harness
		hp := fieldPath{"spec", "harness"}
		if h == nil || (len(h.Allowed) == 0 && h.Default == nil) {
			v.errorf(e.doc, hp, CodeNoHarness, "the TaskSpec has neither harness.allowed nor harness.default, so no harness may run it")
			e.constraintsOK = false
			continue
		}
		first := map[string]int{}
		for i, c := range h.Allowed {
			cp := hp.at("allowed", i)
			at := func(p fieldPath, code, msg string) { v.errorf(e.doc, cp.at(p), code, "%s", msg) }
			if j, dup := first[c.Name]; dup {
				at(fieldPath{"name"}, CodeDuplicateHarness, fmt.Sprintf("harness %q is already allowed at harness.allowed[%d]; resolution only uses the first", c.Name, j))
				continue
			}
			first[c.Name] = i
			okModels := checkPattern(at, fieldPath{"models"}, c.Models)
			okEffort := checkPattern(at, fieldPath{"reasoningEffort"}, c.ReasoningEffort)
			if !okModels || !okEffort {
				e.constraintsOK = false
				continue
			}
			con, err := patterns.ConstraintFrom(c)
			if err != nil {
				at(nil, CodePattern, err.Error())
				e.constraintsOK = false
				continue
			}
			e.compiled.Constraints = append(e.compiled.Constraints, con)
			checkEffortValues(at, fieldPath{"reasoningEffort"}, c.ReasoningEffort)
			v.checkCapabilities(at, fieldPath{"reasoningEffort"}, e, c.Name, con.Effort, true)
		}
		if h.Default != nil {
			dp := hp.at("default")
			at := func(p fieldPath, code, msg string) { v.errorf(e.doc, dp.at(p), code, "%s", msg) }
			req := compileRequest(at, *h.Default)
			switch {
			case req == nil:
				if len(h.Allowed) == 0 {
					e.constraintsOK = false
				}
			case len(h.Allowed) == 0:
				e.compiled.Default = req
				e.compiled.Constraints = []patterns.Constraint{derivedConstraint(*req)}
				v.checkRequest(at, e, *req)
			default:
				e.compiled.Default = req
				v.checkRequest(at, e, *req)
			}
		}
	}
	for _, vw := range v.checkedViews() {
		for i := range vw.actions {
			d := vw.actions[i].Action.Dispatch
			if d == nil || d.Harness == nil || d.Harness.IsExpr() {
				continue
			}
			hp := actionPath(i, "dispatch").at("harness")
			at := func(p fieldPath, code, msg string) { v.viewErr(vw, hp.at(p), code, "%s", msg) }
			req := compileRequest(at, d.Harness.Value)
			if req == nil {
				continue
			}
			vw.actions[i].Harness = req
			if e := v.taskSpecs[d.TaskSpec]; e != nil {
				v.checkRequest(at, e, *req)
			}
		}
	}
}

// derivedConstraint is the constraint of a TaskSpec without harness.allowed: its
// harness.default, as a constraint (an unset model or effort matches anything). So nothing
// outside the default runs, as with an explicit allowed list.
func derivedConstraint(def patterns.Request) patterns.Constraint {
	c := patterns.Constraint{Harness: def.Harness, Models: def.Model, Effort: def.Effort}
	if c.Models == nil {
		c.Models = patterns.Any()
	}
	if c.Effort == nil {
		c.Effort = patterns.Any()
	}
	return c
}

// checkPattern reports a harness pattern that doesn't compile (CAT800).
func checkPattern(at report, p fieldPath, val v1alpha1.StringOrList) bool {
	if val.IsZero() {
		return true
	}
	if err := patterns.Validate(val); err != nil {
		at(p, CodePattern, err.Error())
		return false
	}
	return true
}

// compileRequest compiles a harness request; nil (and CAT800) when a pattern doesn't.
func compileRequest(at report, r v1alpha1.HarnessRequest) *patterns.Request {
	if r.Config != nil {
		okModel := checkPattern(at, fieldPath{"config", "model"}, r.Config.Model)
		okEffort := checkPattern(at, fieldPath{"config", "reasoningEffort"}, r.Config.ReasoningEffort)
		if !okModel || !okEffort {
			return nil
		}
		checkEffortValues(at, fieldPath{"config", "reasoningEffort"}, r.Config.ReasoningEffort)
	}
	req, err := patterns.RequestFrom(r)
	if err != nil {
		at(nil, CodePattern, err.Error())
		return nil
	}
	return &req
}

// checkRequest checks a request against its TaskSpec's constraints and the adapters.
func (v *validator) checkRequest(at report, e *taskSpecEntry, req patterns.Request) {
	effort, mapping := req.Effort, true
	if !e.constraintsOK {
		return // CAT800 or CAT805 already
	}
	if len(e.compiled.Constraints) > 0 {
		i := slices.IndexFunc(e.compiled.Constraints, func(c patterns.Constraint) bool { return c.Harness == req.Harness })
		if i < 0 {
			allowed := make([]string, len(e.compiled.Constraints))
			for j, c := range e.compiled.Constraints {
				allowed[j] = c.Harness
			}
			msg := fmt.Sprintf("harness %q is not in TaskSpec %s's harness.allowed (%s)", req.Harness, e.ts.GetName(), strings.Join(allowed, ", "))
			if len(e.ts.Spec.Harness.Allowed) == 0 {
				msg = fmt.Sprintf("harness %q is outside TaskSpec %s's constraint: it has no harness.allowed, so only its harness.default (%s) may run it", req.Harness, e.ts.GetName(), allowed[0])
			}
			at(fieldPath{"name"}, CodeNotSubsumed, msg)
			return
		}
		c := e.compiled.Constraints[i]
		if known := v.opts.KnownModels[req.Harness]; len(known) > 0 {
			if ok, _ := patterns.Subsumes(c, req, known); !ok {
				at(fieldPath{"config", "model"}, CodeNotSubsumed, fmt.Sprintf("no known %s model fits both the request (%s) and TaskSpec %s's constraint (%s); known models: %s",
					req.Harness, patString(req.Model), e.ts.GetName(), patString(c.Models), strings.Join(known, ", ")))
			}
		}
		effort = both{c.Effort, req.Effort}
		if !slices.ContainsFunc(NormalisedEfforts, effort.Match) {
			at(fieldPath{"config", "reasoningEffort"}, CodeNotSubsumed, fmt.Sprintf("no effort fits both the request (%s) and TaskSpec %s's constraint (%s)", patString(req.Effort), e.ts.GetName(), patString(c.Effort)))
			mapping = false // the mapping check would only repeat it
		}
	} else if known := v.opts.KnownModels[req.Harness]; len(known) > 0 && !slices.ContainsFunc(known, func(m string) bool { return matches(req.Model, m) }) {
		at(fieldPath{"config", "model"}, CodeNotSubsumed, fmt.Sprintf("no known %s model matches %s; known models: %s", req.Harness, patString(req.Model), strings.Join(known, ", ")))
	}
	v.checkCapabilities(at, fieldPath{"config", "reasoningEffort"}, e, req.Harness, effort, mapping)
}

// both matches what two patterns both match.
type both [2]patterns.Pattern

func (b both) Match(s string) bool { return matches(b[0], s) && matches(b[1], s) }
func (b both) String() string      { return patString(b[0]) + " ∩ " + patString(b[1]) }

func matches(p patterns.Pattern, s string) bool { return p == nil || p.Match(s) }

func patString(p patterns.Pattern) string {
	if p == nil {
		return "*"
	}
	return p.String()
}

// isPatternString reports whether an effort string is a pattern rather than a value.
func isPatternString(s string) bool {
	return s == "*" || strings.HasPrefix(s, patterns.RegexPrefix) || strings.ContainsAny(s, `*?{}\`)
}

// checkEffortValues checks efforts against the normalised set (CAT802): a value must be in
// it, a pattern must match some of it.
func checkEffortValues(at report, p fieldPath, val v1alpha1.StringOrList) {
	for i, s := range val.Values {
		ep := p
		if val.IsList() {
			ep = p.at(i)
		}
		if isPatternString(s) {
			pat, err := patterns.Compile(s)
			if err == nil && !slices.ContainsFunc(NormalisedEfforts, pat.Match) {
				at(ep, CodeEffort, fmt.Sprintf("effort pattern %q matches none of the normalised efforts (%s)", s, strings.Join(NormalisedEfforts, ", ")))
			}
			continue
		}
		if !slices.Contains(NormalisedEfforts, s) {
			at(ep, CodeEffort, fmt.Sprintf("effort %q is not one of the normalised efforts (%s)", s, strings.Join(NormalisedEfforts, ", ")))
		}
	}
}

// requiredFeatures are the adapter features a TaskSpec needs, each as alternatives.
func requiredFeatures(s v1alpha1.TaskSpecSpec) [][2]string {
	var out [][2]string
	if s.Output != nil && s.Output.Schema != "" {
		out = append(out, [2]string{FeatureStructuredOutput, "output.schema"})
	}
	if s.Effects != nil && len(s.Effects.Allow) > 0 {
		out = append(out, [2]string{FeatureMCP, "effects"})
	}
	if len(s.Skills) > 0 {
		out = append(out, [2]string{FeatureSkillsDir + "|" + FeaturePlugins, "skills"})
	}
	return out
}

// checkCapabilities checks that the harness has a capability descriptor with the features
// the TaskSpec needs (CAT803), and maps one of the efforts allowed (CAT802).
func (v *validator) checkCapabilities(at report, effortPath fieldPath, e *taskSpecEntry, harness string, effort patterns.Pattern, mapping bool) {
	caps, ok := v.capabilitiesFor(harness)
	if !ok {
		known := make([]string, len(v.opts.Capabilities))
		for i, c := range v.opts.Capabilities {
			known[i] = c.Harness
		}
		at(fieldPath{"name"}, CodeCapability, fmt.Sprintf("no capability descriptor for harness %q (known harnesses: %s)", harness, strings.Join(known, ", ")))
		return
	}
	for _, f := range requiredFeatures(e.ts.Spec) {
		if !slices.ContainsFunc(strings.Split(f[0], "|"), func(alt string) bool { return slices.Contains(caps.Features, alt) }) {
			at(fieldPath{"name"}, CodeCapability, fmt.Sprintf("harness %s lacks feature %s, which TaskSpec %s needs for %s", harness, strings.ReplaceAll(f[0], "|", " or "), e.ts.GetName(), f[1]))
		}
	}
	if len(caps.EffortMapping) == 0 || !mapping {
		return
	}
	mapped := sortedKeys(caps.EffortMapping)
	if !slices.ContainsFunc(mapped, func(s string) bool { return matches(effort, s) }) {
		at(effortPath, CodeEffort, fmt.Sprintf("harness %s maps none of the efforts allowed here (%s); it maps %s", harness, patString(effort), strings.Join(mapped, ", ")))
	}
}

// ---- Phase 9: effects -----------------------------------------------------------------------

// writable reports whether a TaskSpec's workspace can be written: a rw repo, or a checkout
// other than read-only (the merge TaskSpec's rebase-only checks out the target repo
// writable).
func writable(s v1alpha1.TaskSpecSpec) bool {
	ws := s.Workspace
	if ws == nil {
		return false
	}
	if slices.ContainsFunc(ws.Repos, func(r v1alpha1.RepoMount) bool { return r.Mode == "rw" }) {
		return true
	}
	return ws.Checkout != "" && ws.Checkout != "read-only"
}

// labelOps are the effect ops whose inline fields carry labels.
var labelOps = []v1alpha1.EffectOp{v1alpha1.OpLabelAdd, v1alpha1.OpLabelRemove}

// checkEffects is phase 9.
func (v *validator) checkEffects() {
	for _, name := range sortedKeys(v.taskSpecs) {
		e := v.taskSpecs[name]
		s := e.ts.Spec
		if s.Effects == nil {
			continue
		}
		for i, op := range s.Effects.Allow {
			p := fieldPath{"spec", "effects", "allow"}.at(i)
			side, known := op.Side()
			switch {
			case !known:
				v.errorf(e.doc, p, CodeUnknownOp, "unknown effect op %q", op)
			case strings.HasPrefix(string(op), "review.") && s.Identity != "reviewer":
				v.errorf(e.doc, p, CodeReviewOp, "%s needs identity: reviewer (identity is %q)", op, s.Identity)
			case side == v1alpha1.SideGit && !writable(s):
				v.errorf(e.doc, p, CodeGitOp, "%s needs a writable workspace: a rw repo, or a checkout other than read-only", op)
			}
		}
		compilePatterns(s.Effects.Labels, func(i int, err error) {
			v.errorf(e.doc, fieldPath{"spec", "effects", "labels"}.at(i), CodeUnknownOp, "%v", err)
		})
	}
	for _, vw := range v.checkedViews() {
		for i, ca := range vw.actions {
			p := actionPath(i, ca.Type)
			switch a := ca.Action; {
			case a.Effect != nil:
				if !a.Effect.Op.Known() {
					v.viewErr(vw, p.at("op"), CodeEffectAction, "unknown effect op %q", a.Effect.Op)
					continue
				}
				if slices.Contains(labelOps, a.Effect.Op) && !labelsArg(a.Effect.Args["labels"]) {
					v.viewErr(vw, p.at("labels"), CodeEffectAction, "%s needs labels: a list of label names (or a \"=\" expression)", a.Effect.Op)
				}
			case a.RequestApproval != nil && vw.set == nil:
				v.viewWarn(vw, p, CodeUnsupportedOp, "requestApproval is accepted but not supported in the PoC (Q18): the engine records it as Unsupported")
			}
		}
	}
}

func labelsArg(v any) bool {
	switch x := v.(type) {
	case string:
		return isExpr(x)
	case []any:
		return len(x) > 0 && !slices.ContainsFunc(x, func(e any) bool { s, ok := e.(string); return !ok || s == "" })
	}
	return false
}

// ---- Phase 10: state ------------------------------------------------------------------------

var stateReadRE = regexp.MustCompile(`item\.state(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*['"]([^'"]+)['"]\s*\])`)

// stateReads are the item.state keys an expression reads, sorted (best effort, textual).
func stateReads(src string) []string {
	set := map[string]bool{}
	for _, m := range stateReadRE.FindAllStringSubmatch(src, -1) {
		set[m[1]+m[2]] = true
	}
	return sortedKeys(set)
}

// checkState is phase 10: set keys (CAT1000) and where reads (CAT1001, warning) against the
// RuleSet's stateKeys, when it declares them.
func (v *validator) checkState() {
	for _, name := range v.ruleSetNames {
		rse := v.ruleSets[name]
		keys := rse.rs.Spec.StateKeys
		if len(keys) == 0 {
			continue
		}
		list := strings.Join(keys, ", ")
		for _, vw := range rse.views {
			if vw.rule == nil {
				continue
			}
			for i, ca := range vw.actions {
				if ca.Action.Set == nil {
					continue
				}
				for _, k := range sortedKeys(ca.Action.Set) {
					if !slices.Contains(keys, k) {
						v.errorf(vw.entry.doc, vw.path(actionPath(i, "set").at(k)), CodeStateKey, "state key %q is not in RuleSet %s's stateKeys (%s)", k, name, list)
					}
				}
			}
			for _, k := range stateReads(vw.rule.Spec.Where) {
				if !slices.Contains(keys, k) {
					v.warnf(vw.entry.doc, vw.path(fieldPath{"spec", "where"}), CodeStateRead, "reads item.state.%s, which is not in RuleSet %s's stateKeys (%s)", k, name, list)
				}
			}
		}
	}
}

// ---- Phase 11: workspace --------------------------------------------------------------------

var inputReadRE = regexp.MustCompile(`\binputs(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*['"]([^'"]+)['"]\s*\])`)

// checkWorkspace is phase 11: at most one rw repo (CAT1100); TaskSpec "=" fields use only
// inputs and task, and only declared inputs (CAT1101).
func (v *validator) checkWorkspace() {
	allowed := []string{cel.VarInputs, cel.VarTask}
	for _, name := range sortedKeys(v.taskSpecs) {
		e := v.taskSpecs[name]
		s := e.ts.Spec
		if ws := s.Workspace; ws != nil {
			rw := 0
			for i, r := range ws.Repos {
				if r.Mode != "rw" {
					continue
				}
				if rw++; rw == 2 {
					v.errorf(e.doc, fieldPath{"spec", "workspace", "repos"}.at(i, "mode"), CodeWorkspaceRW, "a second rw repo: at most one workspace repo may be rw")
				}
			}
		}
		paths := sortedKeys(e.compiled.Expressions)
		for _, ps := range paths {
			prog := e.compiled.Expressions[ps]
			p := parsePath(ps)
			for _, vr := range prog.Variables() {
				if !slices.Contains(allowed, vr) {
					v.errorf(e.doc, p, CodeWorkspaceExpr, "%q uses %s; TaskSpec fields may only use inputs and task", prog.Source(), vr)
				}
			}
			seen := map[string]bool{}
			for _, m := range inputReadRE.FindAllStringSubmatch(prog.Source(), -1) {
				k := m[1] + m[2]
				if _, ok := s.Inputs[k]; !ok && !seen[k] {
					seen[k] = true
					v.errorf(e.doc, p, CodeWorkspaceExpr, "%q uses inputs.%s, which the TaskSpec doesn't declare", prog.Source(), k)
				}
			}
		}
	}
}

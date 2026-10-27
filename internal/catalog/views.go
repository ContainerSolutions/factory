package catalog

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/cel"
)

// view is a rule with one set of params: its own (set == nil), or a RuleSet's merge.
type view struct {
	entry *ruleEntry
	set   *ruleSetEntry
	// params are the merged params; rule is the rule after substitution (nil when it
	// failed); actions are rule's actions compiled.
	params  map[string]any
	rule    *v1alpha1.Rule
	actions []CompiledAction
	// providers is EffectiveRule.Providers (RuleSet views only).
	providers []string
	// shared is true when the RuleSet's params equal the rule's own: the view reuses the
	// rule's own substitution, already checked.
	shared bool
	// note is appended to messages about a RuleSet view, naming the RuleSet.
	note string
}

// path turns a rule-relative path (spec.actions[0]…) into a path in the rule's document.
func (vw *view) path(p fieldPath) fieldPath { return vw.entry.base.at(p) }

func (v *validator) viewErr(vw *view, p fieldPath, code, format string, args ...any) {
	v.errorf(vw.entry.doc, vw.path(p), code, "%s%s", fmt.Sprintf(format, args...), vw.note)
}

func (v *validator) viewWarn(vw *view, p fieldPath, code, format string, args ...any) {
	v.warnf(vw.entry.doc, vw.path(p), code, "%s%s", fmt.Sprintf(format, args...), vw.note)
}

// buildViews is phase 5: params merge (rule params ← RuleSet defaults ← overrides[rule]) and
// {{ params }} substitution, once with the rule's own params and once per RuleSet whose
// params differ.
func (v *validator) buildViews() {
	for _, name := range v.ruleNames {
		r := v.rules[name]
		params, err := cel.MergeParams(r.rule.Spec.Params, nil, nil)
		if err != nil { // can't happen without overrides; keep going with the declared params
			params = r.rule.Spec.Params
		}
		r.own = v.newView(r, nil, params)
	}
	for _, name := range v.ruleSetNames {
		rse := v.ruleSets[name]
		spec := rse.rs.Spec
		for _, r := range rse.selected {
			overrides := spec.Overrides[r.bare]
			params, err := cel.MergeParams(r.rule.Spec.Params, spec.Defaults, overrides)
			if err != nil {
				kept := map[string]any{}
				for k, val := range overrides {
					if _, ok := r.rule.Spec.Params[k]; ok {
						kept[k] = val
					} else {
						v.errorf(rse.doc, fieldPath{"spec", "overrides", r.bare, k}, CodeUnknownOverride, "%q is not a declared param of rule %s (declared: %s)", k, r.name, names(r.rule.Spec.Params))
					}
				}
				params, _ = cel.MergeParams(r.rule.Spec.Params, spec.Defaults, kept)
			}
			var vw *view
			if reflect.DeepEqual(params, r.own.params) {
				vw = &view{entry: r, set: rse, params: r.own.params, rule: r.own.rule, actions: r.own.actions, shared: true}
			} else {
				vw = v.newView(r, rse, params)
			}
			vw.providers = v.effectiveProviders(rse, r)
			rse.views = append(rse.views, vw)
		}
		for _, k := range sortedKeys(spec.Defaults) {
			declared := false
			for _, r := range rse.selected {
				if _, ok := r.rule.Spec.Params[k]; ok {
					declared = true
					break
				}
			}
			if !declared {
				v.warnf(rse.doc, fieldPath{"spec", "defaults", k}, CodeUnusedDefault, "no selected rule declares param %q, so the default is ignored", k)
			}
		}
	}
}

// newView substitutes params into the rule's actions (CAT502 on failure) and compiles them.
func (v *validator) newView(r *ruleEntry, rse *ruleSetEntry, params map[string]any) *view {
	vw := &view{entry: r, set: rse, params: params}
	if rse != nil {
		vw.note = fmt.Sprintf(" (with RuleSet %s's params)", rse.rs.GetName())
	}
	sub, err := cel.SubstituteRule(r.rule, params)
	if err != nil {
		for _, e := range flatten(err) {
			var pe *cel.ParamError
			if errors.As(e, &pe) {
				p := pe.Path
				if p == "" {
					p = "spec.actions"
				}
				v.viewErr(vw, parsePath(p), CodePlaceholder, "%s", pe.Message)
				continue
			}
			v.viewErr(vw, fieldPath{"spec", "actions"}, CodePlaceholder, "%v", e)
		}
		return vw
	}
	vw.rule = sub
	for i, a := range sub.Spec.Actions {
		typ := a.Type()
		tree, err := toTree(a)
		if err != nil {
			v.viewErr(vw, fieldPath{"spec", "actions"}.at(i), CodeSchema, "%v", err)
			continue
		}
		m, _ := tree.(map[string]any)
		vw.actions = append(vw.actions, CompiledAction{Type: typ, Action: a, Tree: m[typ]})
	}
	return vw
}

// flatten returns the leaf errors of (nested) errors.Join results.
func flatten(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok { //nolint:errorlint // walking a Join tree
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []error{err}
}

// checkedViews are the views whose actions are checked: every rule's own view, and the
// RuleSet views with params of their own. Substitution failures (rule == nil) are skipped.
func (v *validator) checkedViews() []*view {
	var out []*view
	for _, name := range v.ruleNames {
		if vw := v.rules[name].own; vw.rule != nil {
			out = append(out, vw)
		}
	}
	for _, name := range v.ruleSetNames {
		for _, vw := range v.ruleSets[name].views {
			if !vw.shared && vw.rule != nil {
				out = append(out, vw)
			}
		}
	}
	return out
}

// actionPath is spec.actions[i].<type>.
func actionPath(i int, typ string) fieldPath { return fieldPath{"spec", "actions"}.at(i, typ) }

// isExpr reports whether s is a runtime "=CEL" value; references in such fields are
// resolved when the rule runs.
func isExpr(s string) bool { _, ok := cel.CELSource(s); return ok }

// checkActionReferences is phase 2 for rule actions, on the substituted views.
func (v *validator) checkActionReferences() {
	for _, vw := range v.checkedViews() {
		for i, ca := range vw.actions {
			p := actionPath(i, ca.Type)
			a := ca.Action
			switch {
			case a.Dispatch != nil:
				v.refTaskSpec(vw, p.at("taskSpec"), a.Dispatch.TaskSpec)
				if q := a.Dispatch.Queue; !isExpr(q) && v.queues[q] == nil {
					v.viewErr(vw, p.at("queue"), CodeUnknownQueue, "Queue %q does not exist", q)
				}
				if t := a.Dispatch.Target; t != nil && t.Provider != "" && !isExpr(t.Provider) && v.providers[t.Provider] == nil {
					v.viewErr(vw, p.at("target", "provider"), CodeUnknownProvider, "Provider %q does not exist", t.Provider)
				}
			case a.Cancel != nil:
				v.refTaskSpec(vw, p.at("taskSpec"), a.Cancel.TaskSpec)
			case a.Notify != nil:
				v.refTemplate(vw, p.at("template"), a.Notify.Template)
				for j, to := range a.Notify.To {
					tp := p.at("to", j)
					if to.Group != "" && v.groups[to.Group] == nil {
						v.viewErr(vw, tp.at("group"), CodeUnknownGroup, "Group %q does not exist", to.Group)
					}
					if to.Via != "" && v.providers[to.Via] == nil {
						v.viewErr(vw, tp.at("via"), CodeUnknownProvider, "Provider %q does not exist", to.Via)
					}
					if to.Channel != nil && v.providers[to.Channel.Provider] == nil {
						v.viewErr(vw, tp.at("channel", "provider"), CodeUnknownProvider, "Provider %q does not exist", to.Channel.Provider)
					}
				}
			case a.RequestApproval != nil:
				if g := a.RequestApproval.Approvers; v.groups[g] == nil {
					v.viewErr(vw, p.at("approvers"), CodeUnknownGroup, "Group %q does not exist", g)
				}
				if t := a.RequestApproval.Template; t != "" {
					v.refTemplate(vw, p.at("template"), t)
				}
			}
		}
	}
}

func (v *validator) refTaskSpec(vw *view, p fieldPath, name string) {
	if !isExpr(name) && v.taskSpecs[name] == nil {
		v.viewErr(vw, p, CodeUnknownTaskSpec, "TaskSpec %q does not exist", name)
	}
}

func (v *validator) refTemplate(vw *view, p fieldPath, name string) {
	if !isExpr(name) && v.templates[name] == nil {
		v.viewErr(vw, p, CodeUnknownTemplate, "Template %q does not exist", name)
	}
}

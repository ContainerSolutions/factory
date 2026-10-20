// Package rules is the rule engine (design/workstreams/dispatcher-core/LLD/rules-engine.md):
// Engine.Evaluate turns one event and a work item snapshot into the ordered decisions the
// rule controller applies, with the record of why each rule did or didn't fire.
//
// The package is pure: no I/O, no goroutines and no wall clock. The time comes from the
// Engine's Clock, the safety rails from the caller's Rails, and everything else from the
// catalog snapshot, the event and the WorkItemView.
package rules

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/cel"
)

// Clock reads the time.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock (tests: a fixed time).
type ClockFunc func() time.Time

// Now calls f.
func (f ClockFunc) Now() time.Time { return f() }

// Engine evaluates rules. Its zero value is ready: without a Clock, `now` (and debounce
// deadlines) is the event's time, which keeps fixtures and replays deterministic. An Engine
// holds no state, so it is safe for concurrent use.
type Engine struct {
	Clock Clock
}

func (e *Engine) now(ev v1alpha1.Event) time.Time {
	if e == nil || e.Clock == nil {
		return ev.Time
	}
	return e.Clock.Now()
}

// candidate is one (RuleSet, rule) pair selected for the event.
type candidate struct {
	set  string
	rule *catalog.EffectiveRule
}

func (c candidate) priority() int { return c.rule.Rule.Spec.Priority }

// Evaluate evaluates ev against the work item wi with the catalog c, judging every decision
// with rails. It never panics on rule errors: a where that fails is a not-matching candidate
// with its error recorded, and an action that fails to materialise drops all of that rule's
// decisions. It returns an error only for a nil catalog or rails, or when ctx is done.
//
// The steps follow the LLD: scope the RuleSets; select the rules whose on and providers
// match; order the candidates by (priority desc, RuleSet name, rule name); then, for each
// one, respect final, dedupe on firing per (event, rule), evaluate the where with the
// RuleSet's params, stage the actions against the state written so far (all or nothing),
// and commit them through the rails.
func (e *Engine) Evaluate(ctx context.Context, c *catalog.Catalog, ev v1alpha1.Event, wi WorkItemView, rails Rails) (Evaluation, error) {
	if c == nil {
		return Evaluation{}, errors.New("rules: nil catalog")
	}
	if rails == nil {
		return Evaluation{}, errors.New("rules: nil rails")
	}
	if err := ctx.Err(); err != nil {
		return Evaluation{}, fmt.Errorf("rules: evaluate event %s: %w", ev.ID, err)
	}
	now := e.now(ev)
	b := newBase(c, ev, wi, now)
	state := make(map[string]any, len(wi.State))
	for k, v := range wi.State {
		state[k] = v
	}
	out := Evaluation{Event: ev.ID, WorkItem: wi.ID, Matches: []Match{}, Decisions: []Decision{}}

	fired := map[string]string{} // rule → the RuleSet it fired through
	stop := false
	for _, cand := range candidates(c, ev, wi) {
		r := cand.rule
		m := Match{RuleSet: cand.set, Rule: r.Name, Priority: cand.priority()}
		if stop {
			m.Result = SuppressedByFinal
			out.Matches = append(out.Matches, m)
			continue
		}
		if via, ok := fired[r.Name]; ok {
			m.Result, m.FiredVia = Duplicate, via
			out.Matches = append(out.Matches, m)
			continue
		}
		act := b.activation(r.Params, state)
		m.Inputs = whereInputs(r.Where, act)
		ok, err := evalWhere(ctx, r.Where, act)
		if cerr := ctx.Err(); cerr != nil {
			return Evaluation{}, fmt.Errorf("rules: evaluate event %s: %w", ev.ID, cerr)
		}
		switch {
		case err != nil:
			m.Result, m.Error = WhereError, err.Error()
			out.Matches = append(out.Matches, m)
			continue // doesn't consume the dedupe
		case !ok:
			m.Result = NotMatched
			out.Matches = append(out.Matches, m)
			continue // doesn't consume the dedupe: a later RuleSet may still fire the rule
		}
		// The where passed: this RuleSet fires the rule, and no other will for this event.
		fired[r.Name] = cand.set
		if r.Rule.Spec.Final {
			stop = true // even when the actions fail below: final fails closed
		}
		staged, aerr := stage(ctx, c, r, b, state, now, wi.ID)
		if cerr := ctx.Err(); cerr != nil {
			return Evaluation{}, fmt.Errorf("rules: evaluate event %s: %w", ev.ID, cerr)
		}
		if aerr != nil {
			m.Result, m.Error = ActionError, aerr.err.Error()
			m.Action = aerr.path
			out.Matches = append(out.Matches, m)
			continue // nothing from this rule is committed
		}
		m.Result = Matched
		out.Matches = append(out.Matches, m)
		for _, intent := range staged {
			var verdict v1alpha1.RailVerdict
			if intent.Unsupported != nil {
				verdict = v1alpha1.RailVerdict{Rail: RailEngine, Verdict: VerdictUnsupported, Reason: intent.Unsupported.Reason}
			} else {
				verdict = rails.Check(intent)
			}
			out.Decisions = append(out.Decisions, Decision{RuleSet: cand.set, Rule: r.Name, Action: intent, Rail: verdict, Inputs: m.Inputs})
			if intent.Set != nil && verdict.Verdict == VerdictAllowed {
				state[intent.Set.Key] = intent.Set.Value
			}
		}
	}
	out.StateAfter = state
	return out, nil
}

// candidates are the (RuleSet, rule) pairs in scope whose on has the event type and whose
// providers have the event's source, in evaluation order.
func candidates(c *catalog.Catalog, ev v1alpha1.Event, wi WorkItemView) []candidate {
	var out []candidate
	for _, name := range sortedKeys(c.RuleSets) {
		rs := c.RuleSets[name]
		if !inScope(rs, ev, wi) {
			continue
		}
		for _, r := range rs.Rules {
			if slices.Contains(r.Rule.Spec.On, ev.Type) && slices.Contains(r.Providers, ev.Source) {
				out = append(out, candidate{set: name, rule: r})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		return cmp.Or(
			cmp.Compare(b.priority(), a.priority()),
			cmp.Compare(a.set, b.set),
			cmp.Compare(a.rule.Name, b.rule.Name),
		)
	})
	return out
}

// CouldFire reports whether any rule could fire for ev against wi: some RuleSet in scope (the
// RuleSet's providers and scope, as Evaluate decides it) has a rule whose on has the event
// type and whose providers have its source. It reads only the catalog, the event and, for a
// factory.* event, wi's links, so the rule controller asks it before observing anything.
func CouldFire(c *catalog.Catalog, ev v1alpha1.Event, wi WorkItemView) bool {
	return c != nil && len(candidates(c, ev, wi)) > 0
}

// ReadsItem reports whether the where of any rule that could fire for ev reads item. When none
// does, which rules fire doesn't depend on the work item (its observations included): an
// evaluation against an unobserved view decides the same.
func ReadsItem(c *catalog.Catalog, ev v1alpha1.Event, wi WorkItemView) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(candidates(c, ev, wi), func(cand candidate) bool {
		w := cand.rule.Where
		return w != nil && slices.Contains(w.Variables(), cel.VarItem)
	})
}

func evalWhere(ctx context.Context, where *cel.Program, act cel.Activation) (bool, error) {
	if where == nil {
		return false, errors.New("where is not compiled")
	}
	return where.EvalBool(ctx, act)
}

// stage materialises all of a fired rule's actions against a staged overlay of the state,
// so later actions see earlier sets; nothing is committed when any action fails. A debounced
// rule stages one Debounce intent that holds them.
func stage(ctx context.Context, c *catalog.Catalog, r *catalog.EffectiveRule, b base, state map[string]any, now time.Time, item string) ([]ActionIntent, *actionError) {
	overlay := make(map[string]any, len(state))
	for k, v := range state {
		overlay[k] = v
	}
	var staged []ActionIntent
	for i, ca := range r.Actions {
		intents, err := materialise(ctx, c, ca, b.activation(r.Params, overlay))
		if err != nil {
			return nil, &actionError{path: "actions[" + strconv.Itoa(i) + "]." + ca.Type, err: err}
		}
		for _, in := range intents {
			if in.Set != nil {
				overlay[in.Set.Key] = in.Set.Value
			}
		}
		staged = append(staged, intents...)
	}
	db := r.Rule.Spec.Debounce
	if db == nil {
		return staged, nil
	}
	d, err := debounce(ctx, c, r, db, b.activation(r.Params, state), now, item)
	if err != nil {
		return nil, &actionError{path: "debounce", err: err}
	}
	d.Then = staged
	return []ActionIntent{{Debounce: d}}, nil
}

// debounce computes the Debounce intent's key and deadline. The key is evaluated with the
// state before the rule's actions.
func debounce(ctx context.Context, c *catalog.Catalog, r *catalog.EffectiveRule, db *v1alpha1.Debounce, act cel.Activation, now time.Time, item string) (*Debounce, error) {
	window, err := db.Window.Parse()
	if err != nil {
		return nil, fmt.Errorf("window: %w", err)
	}
	value := item
	if db.Key != "" {
		src := db.Key
		if s, ok := cel.CELSource(src); ok {
			src = s
		}
		p := c.Programs[src]
		if p == nil {
			return nil, fmt.Errorf("key %q is not compiled", src)
		}
		v, err := p.EvalValue(ctx, act)
		if err != nil {
			return nil, fmt.Errorf("key: %w", err)
		}
		value = keyString(v)
	}
	return &Debounce{Key: r.Name + "/" + value, Until: now.Add(window)}, nil
}

func keyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "null"
	default:
		return fmt.Sprint(x)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

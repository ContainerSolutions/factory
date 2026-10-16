package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// ChangeOp is how one rule, decision or state key changed between two evaluations.
type ChangeOp string

// Change ops.
const (
	ChangeAdded     ChangeOp = "added"
	ChangeRemoved   ChangeOp = "removed"
	ChangeChanged   ChangeOp = "changed"
	ChangeReordered ChangeOp = "reordered"
)

// Change sections: what changed.
const (
	// SectionEvaluation: the event or work item id.
	SectionEvaluation = "evaluation"
	// SectionRule: a (RuleSet, rule) candidate and its match result.
	SectionRule = "rule"
	// SectionDecision: a decision (an action and its rail verdict), or their order.
	SectionDecision = "decision"
	// SectionState: a top-level key of StateAfter.
	SectionState = "state"
)

// Change is one rule-level or decision-level difference between two evaluations of the same
// case (the golden and now, or the base catalog and the head catalog).
type Change struct {
	Section string   `json:"section"`
	Op      ChangeOp `json:"op"`
	// Subject is what changed: "<ruleset>/<rule>" for a rule, "<ruleset>/<rule>: <action>"
	// for a decision, the key for state, the field for the evaluation.
	Subject string `json:"subject"`
	// From and To summarise the old and new value (a match result, a rail verdict, a state
	// value); empty when there is none or when only Details changed.
	From string `json:"from,omitzero"`
	To   string `json:"to,omitzero"`
	// Details are field-level changes ("dispatch.inputs.target: \"pr\" -> \"stack\"").
	Details []string `json:"details,omitzero"`
}

// Lines renders the change for people: a "+", "-" or "~" line, then its details indented.
func (c Change) Lines() []string {
	sym := "~"
	switch c.Op {
	case ChangeAdded:
		sym = "+"
	case ChangeRemoved:
		sym = "-"
	}
	head := sym + " " + c.Section + " " + c.Subject
	sep := ": "
	if c.Section == SectionDecision {
		sep = " " // the values are rail verdicts: "[allowed]"
	}
	switch {
	case c.From != "" && c.To != "":
		head += sep + c.From + " -> " + c.To
	case c.To != "":
		head += sep + c.To
	case c.From != "":
		head += sep + c.From
	}
	out := []string{head}
	for _, d := range c.Details {
		out = append(out, "    "+d)
	}
	return out
}

// DiffGolden compares two evaluations of one case, old then new, rule by rule and decision
// by decision. It returns nil when they are the same.
//
// Rules are paired by (RuleSet, rule). Decisions are paired by (RuleSet, rule, action kind,
// what the action names: TaskSpec, key, template, ref, op) and occurrence, so a changed
// input or verdict reads as a change of that decision rather than a removal and an addition;
// a change of order among the paired decisions is reported once. A decision carries its
// rule's where inputs, so a change of them is reported on the rule only.
func DiffGolden(old, cur GoldenEvaluation) []Change {
	var out []Change
	if old.Event != cur.Event {
		out = append(out, Change{Section: SectionEvaluation, Op: ChangeChanged, Subject: "event", From: old.Event, To: cur.Event})
	}
	if old.WorkItem != cur.WorkItem {
		out = append(out, Change{Section: SectionEvaluation, Op: ChangeChanged, Subject: "workItem", From: old.WorkItem, To: cur.WorkItem})
	}
	matches := diffMatches(old.Matches, cur.Matches)
	out = append(out, matches...)
	out = append(out, diffDecisions(old.Decisions, cur.Decisions, matches)...)
	out = append(out, diffState(old.StateAfter, cur.StateAfter)...)
	return out
}

func diffMatches(old, cur []GoldenMatch) []Change {
	key := func(m GoldenMatch) string { return m.RuleSet + "/" + m.Rule }
	curBy := map[string]GoldenMatch{}
	for _, m := range cur {
		curBy[key(m)] = m
	}
	oldKeys := map[string]bool{}
	var out []Change
	for _, o := range old {
		k := key(o)
		oldKeys[k] = true
		n, ok := curBy[k]
		if !ok {
			out = append(out, Change{Section: SectionRule, Op: ChangeRemoved, Subject: k, From: matchSummary(o)})
			continue
		}
		c := Change{Section: SectionRule, Op: ChangeChanged, Subject: k}
		if from, to := matchSummary(o), matchSummary(n); from != to {
			c.From, c.To = from, to
		}
		if o.Priority != n.Priority {
			c.Details = append(c.Details, fmt.Sprintf("priority: %d -> %d", o.Priority, n.Priority))
		}
		if !slices.Equal(o.Inputs, n.Inputs) {
			c.Details = append(c.Details, "inputs: "+keyList(o.Inputs)+" -> "+keyList(n.Inputs))
		}
		if c.From != "" || len(c.Details) > 0 {
			out = append(out, c)
		}
	}
	for _, n := range cur {
		if !oldKeys[key(n)] {
			out = append(out, Change{Section: SectionRule, Op: ChangeAdded, Subject: key(n), To: matchSummary(n)})
		}
	}
	return out
}

// matchSummary is a match's result with its reason: "error: no such key: x",
// "duplicate via poc-docs", "action-error at actions[1].dispatch: …".
func matchSummary(m GoldenMatch) string {
	s := string(m.Result)
	if m.FiredVia != "" {
		s += " via " + m.FiredVia
	}
	if m.Action != "" {
		s += " at " + m.Action
	}
	if m.Error != "" {
		s += ": " + m.Error
	}
	return s
}

// decisionID identifies a decision for pairing, without its occurrence.
func decisionID(d Decision) string {
	a := d.Action
	var name string
	switch {
	case a.Dispatch != nil:
		name = a.Dispatch.TaskSpec
	case a.Cancel != nil:
		name = a.Cancel.TaskSpec
	case a.Set != nil:
		name = a.Set.Key
	case a.Notify != nil:
		name = a.Notify.Template
	case a.Link != nil:
		name = a.Link.Ref.Ref
	case a.Effect != nil:
		name = string(a.Effect.Op)
	case a.Debounce != nil:
		name = a.Debounce.Key
	case a.Unsupported != nil:
		name = a.Unsupported.Action
	}
	return d.RuleSet + "/" + d.Rule + " " + a.Kind() + " " + name
}

// decisionKeys are the decisions' ids with their occurrence ("…#2" for the second one).
func decisionKeys(ds []GoldenDecision) []string {
	seen := map[string]int{}
	out := make([]string, len(ds))
	for i, d := range ds {
		id := decisionID(d.Decision)
		seen[id]++
		out[i] = id + "#" + strconv.Itoa(seen[id])
	}
	return out
}

func decisionSubject(d GoldenDecision) string {
	return d.RuleSet + "/" + d.Rule + ": " + d.Action.String()
}

func diffDecisions(old, cur []GoldenDecision, matches []Change) []Change {
	// The rules whose change is already on the rule line.
	ruleChanged := map[string]bool{}
	for _, m := range matches {
		ruleChanged[m.Subject] = true
	}
	oldKeys, curKeys := decisionKeys(old), decisionKeys(cur)
	curIdx := map[string]int{}
	for i, k := range curKeys {
		curIdx[k] = i
	}
	oldIdx := map[string]int{}
	var out, added []Change
	var oldOrder, curOrder []string // the paired decisions, in each order
	for i, o := range old {
		k := oldKeys[i]
		oldIdx[k] = i
		j, ok := curIdx[k]
		if !ok {
			out = append(out, Change{Section: SectionDecision, Op: ChangeRemoved, Subject: decisionSubject(o),
				From: verdictSummary(o.Rail), Details: actionDetail(o.Action)})
			continue
		}
		oldOrder = append(oldOrder, k)
		n := cur[j]
		c := Change{Section: SectionDecision, Op: ChangeChanged, Subject: decisionSubject(n)}
		if from, to := verdictSummary(o.Rail), verdictSummary(n.Rail); from != to {
			c.From, c.To = from, to
		}
		c.Details = append(c.Details, leafDiff("", o.Action, n.Action)...)
		if !slices.Equal(o.Inputs, n.Inputs) && !ruleChanged[o.RuleSet+"/"+o.Rule] {
			c.Details = append(c.Details, "inputs: "+keyList(o.Inputs)+" -> "+keyList(n.Inputs))
		}
		if c.From != "" || len(c.Details) > 0 {
			out = append(out, c)
		}
	}
	for j, n := range cur {
		k := curKeys[j]
		if _, ok := oldIdx[k]; !ok {
			added = append(added, Change{Section: SectionDecision, Op: ChangeAdded, Subject: decisionSubject(n),
				To: verdictSummary(n.Rail), Details: actionDetail(n.Action)})
			continue
		}
		curOrder = append(curOrder, k)
	}
	out = append(out, added...)
	if !slices.Equal(oldOrder, curOrder) {
		was, now := make([]string, len(oldOrder)), make([]string, len(curOrder))
		for i := range oldOrder {
			was[i] = decisionSubject(old[oldIdx[oldOrder[i]]])
			now[i] = decisionSubject(cur[curIdx[curOrder[i]]])
		}
		out = append(out, Change{Section: SectionDecision, Op: ChangeReordered, Subject: "order",
			Details: []string{"was: " + strings.Join(was, "; "), "now: " + strings.Join(now, "; ")}})
	}
	return out
}

// verdictSummary is "[allowed]", "[suppressed kill-switch: item has factory:stop]".
func verdictSummary(v v1alpha1.RailVerdict) string {
	s := v.Verdict
	if v.Rail != "" {
		s += " " + v.Rail
	}
	if v.Reason != "" && v.Reason != v.Rail {
		s += ": " + v.Reason
	}
	return "[" + s + "]"
}

// actionDetail is the action's content on one line, for an added or removed decision.
func actionDetail(a ActionIntent) []string {
	return []string{truncate(compact(a), 160)}
}

func diffState(old, cur map[string]any) []Change {
	var out []Change
	keys := slices.Sorted(maps.Keys(old))
	for _, k := range slices.Sorted(maps.Keys(cur)) {
		if _, ok := old[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		o, inOld := old[k]
		n, inCur := cur[k]
		switch {
		case !inCur:
			out = append(out, Change{Section: SectionState, Op: ChangeRemoved, Subject: k, From: truncate(compact(o), 100)})
		case !inOld:
			out = append(out, Change{Section: SectionState, Op: ChangeAdded, Subject: k, To: truncate(compact(n), 100)})
		default:
			from, to := compact(o), compact(n)
			if from == to {
				continue
			}
			c := Change{Section: SectionState, Op: ChangeChanged, Subject: k}
			if len(from) <= 60 && len(to) <= 60 {
				c.From, c.To = from, to
			} else {
				c.Details = leafDiff(k, o, n)
			}
			out = append(out, c)
		}
	}
	return out
}

// leafDiff lists the leaves (scalars, and arrays of scalars) that differ between a and b, as
// "path: old -> new", with "(none)" for a missing leaf.
func leafDiff(prefix string, a, b any) []string {
	la, lb := map[string]string{}, map[string]string{}
	leaves(prefix, tree(a), la)
	leaves(prefix, tree(b), lb)
	paths := slices.Sorted(maps.Keys(la))
	for p := range lb {
		if _, ok := la[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var out []string
	for _, p := range paths {
		va, okA := la[p]
		vb, okB := lb[p]
		if okA && okB && va == vb {
			continue
		}
		if !okA {
			va = "(none)"
		}
		if !okB {
			vb = "(none)"
		}
		out = append(out, p+": "+truncate(va, 80)+" -> "+truncate(vb, 80))
	}
	return out
}

// tree is v as generic JSON (maps, slices, json.Number, strings, bools, nil).
func tree(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out any
	if err := d.Decode(&out); err != nil {
		return string(b)
	}
	return out
}

func leaves(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			out[prefix] = "{}"
			return
		}
		for k, vv := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			leaves(p, vv, out)
		}
	case []any:
		if scalars(x) {
			out[prefix] = compact(x)
			return
		}
		for i, vv := range x {
			leaves(fmt.Sprintf("%s[%d]", prefix, i), vv, out)
		}
	default:
		out[prefix] = compact(x)
	}
}

func scalars(xs []any) bool {
	for _, x := range xs {
		switch x.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

// compact is v as one line of JSON.
func compact(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func keyList(keys []string) string { return "[" + strings.Join(keys, " ") + "]" }

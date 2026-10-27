package rules

import (
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// Evaluation is the result of evaluating one event against one work item: every candidate
// (RuleSet, rule) with its result, and the ordered decisions the rule controller applies.
type Evaluation struct {
	// Event and WorkItem are the ids of the event and of the work item.
	Event    string `json:"event"`
	WorkItem string `json:"workItem"`
	// Matches are every (RuleSet, rule) candidate, in evaluation order, with its result.
	Matches []Match `json:"matches"`
	// Decisions are the actions to apply, in order.
	Decisions []Decision `json:"decisions"`
	// StateAfter is the work item's state after every allowed set (a projection: the
	// controller writes it).
	StateAfter map[string]any `json:"stateAfter"`
}

// MatchResult is what happened to one candidate.
type MatchResult string

// Match results.
const (
	// Matched: the where passed and the rule's actions became decisions.
	Matched MatchResult = "matched"
	// NotMatched: the where returned false.
	NotMatched MatchResult = "not-matched"
	// WhereError: the where failed at runtime (missing key, wrong type, cost limit). The rule
	// does not match; the error is recorded.
	WhereError MatchResult = "error"
	// ActionError: the where passed but an action failed to materialise, so none of the
	// rule's actions became decisions. The rule still counts as fired (dedupe, final).
	ActionError MatchResult = "action-error"
	// SuppressedByFinal: a higher candidate with final: true fired.
	SuppressedByFinal MatchResult = "suppressed-by-final"
	// Duplicate: the rule already fired for this event through another RuleSet (FiredVia).
	Duplicate MatchResult = "duplicate"
)

// Match is one (RuleSet, rule) candidate and its result.
type Match struct {
	RuleSet  string      `json:"ruleSet"`
	Rule     string      `json:"rule"`
	Priority int         `json:"priority,omitzero"`
	Result   MatchResult `json:"result"`
	// FiredVia is the RuleSet the rule fired through (Duplicate only).
	FiredVia string `json:"firedVia,omitzero"`
	// Action is the failing action ("actions[2].dispatch"; ActionError only).
	Action string `json:"action,omitzero"`
	// Error is the error message (WhereError and ActionError).
	Error string `json:"error,omitzero"`
	// Inputs are the values the where saw (bounded), for evaluated candidates.
	Inputs map[string]any `json:"inputs,omitzero"`
}

// Decision is one action to apply, with the RuleSet and rule it came from and the rails'
// verdict on it.
type Decision struct {
	// RuleSet is the one the rule fired through (its params, its where).
	RuleSet string       `json:"ruleSet"`
	Rule    string       `json:"rule"`
	Action  ActionIntent `json:"action"`
	// Rail is the rails' verdict: allowed, suppressed{reason} or deferred{reason}.
	Rail v1alpha1.RailVerdict `json:"rail"`
	// Inputs are the values the where saw (bounded), for the "why" view.
	Inputs map[string]any `json:"inputs,omitzero"`
}

// Fired reports whether the candidate's where passed (whether or not its actions
// materialised).
func (m Match) Fired() bool { return m.Result == Matched || m.Result == ActionError }

// Records returns the work item's decision records for this evaluation ("why did this
// happen?"): one per candidate, with the where inputs, the actions and the rail verdicts of
// a fired rule, and the reason for the others. at is the time the controller applies them.
func (e Evaluation) Records(at time.Time) []v1alpha1.Decision {
	type key struct{ set, rule string }
	byRule := map[key][]Decision{}
	for _, d := range e.Decisions {
		k := key{d.RuleSet, d.Rule}
		byRule[k] = append(byRule[k], d)
	}
	out := make([]v1alpha1.Decision, 0, len(e.Matches))
	for _, m := range e.Matches {
		r := v1alpha1.Decision{
			At: at, Event: e.Event, RuleSet: m.RuleSet, Rule: m.Rule,
			Fired: m.Fired(), Inputs: m.Inputs, Reason: reason(m),
		}
		for _, d := range byRule[key{m.RuleSet, m.Rule}] {
			r.Actions = append(r.Actions, d.Action.String())
			r.Rails = append(r.Rails, d.Rail)
		}
		out = append(out, r)
	}
	return out
}

func reason(m Match) string {
	switch m.Result {
	case NotMatched:
		return "where is false"
	case WhereError:
		return "where error: " + m.Error
	case ActionError:
		return "action error: " + m.Action + ": " + m.Error
	case SuppressedByFinal:
		return "suppressed by a final rule"
	case Duplicate:
		return "already fired through RuleSet " + m.FiredVia
	default:
		return ""
	}
}

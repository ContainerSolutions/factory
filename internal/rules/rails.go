package rules

import (
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Rails judges each decision against the safety rails (design/02-architecture.md §Safety
// rails): kill switches, budgets, the per-item run cap and the single active run. The rule
// controller (RT) implements it for one (event, work item); tests use FakeRails.
//
// Check is called once per decision, in order, and must not do I/O on the engine's behalf
// beyond what the implementation already holds: the engine is pure, so an implementation
// reads a snapshot taken before Evaluate.
type Rails interface {
	Check(a ActionIntent) v1alpha1.RailVerdict
}

// Verdicts (v1alpha1.RailVerdict.Verdict).
const (
	// VerdictAllowed: apply the action.
	VerdictAllowed = "allowed"
	// VerdictSuppressed: don't apply it; recorded as suppressed (kill switch, run cap,
	// active run).
	VerdictSuppressed = "suppressed"
	// VerdictDeferred: apply it held back (budget: the Task is created Pending with the
	// reason).
	VerdictDeferred = "deferred"
	// VerdictUnsupported: the engine doesn't support the action (requestApproval in the PoC,
	// Q18); the rails are not consulted.
	VerdictUnsupported = "unsupported"
)

// Rail names (v1alpha1.RailVerdict.Rail), as in the rules-engine LLD's Rails contract.
const (
	RailKillSwitch = "kill-switch"
	RailRunCap     = "run-cap"
	RailActiveRun  = "active-run"
	RailBudget     = "budget"
	// RailEngine is the "rail" of an Unsupported decision.
	RailEngine = "engine"
)

// Allowed is the verdict that lets an action through.
func Allowed() v1alpha1.RailVerdict { return v1alpha1.RailVerdict{Verdict: VerdictAllowed} }

// Suppressed is a suppressed verdict from rail.
func Suppressed(rail, reason string) v1alpha1.RailVerdict {
	return v1alpha1.RailVerdict{Rail: rail, Verdict: VerdictSuppressed, Reason: reason}
}

// Deferred is a deferred verdict from rail.
func Deferred(rail, reason string) v1alpha1.RailVerdict {
	return v1alpha1.RailVerdict{Rail: rail, Verdict: VerdictDeferred, Reason: reason}
}

// AllowAll is Rails that allows every action (e.g. to ask "would any rule match?").
var AllowAll Rails = allowAll{}

type allowAll struct{}

func (allowAll) Check(ActionIntent) v1alpha1.RailVerdict { return Allowed() }

// FakeRails is a pure Rails for tests and rule fixtures (rails.json): the first trip that
// matches an action gives its verdict, every other action is allowed. It is read-only, so
// it is safe for concurrent use.
type FakeRails struct {
	Trips []RailTrip `json:"trips"`
}

// RailTrip is one tripped rail. Its match fields narrow the actions it applies to; an
// empty field matches anything.
type RailTrip struct {
	// Rail is the tripped rail (RailKillSwitch, RailRunCap, RailActiveRun, RailBudget). The
	// verdict follows the Rails contract: deferred for the budget, suppressed otherwise.
	Rail string `json:"rail"`
	// Reason is the verdict's reason; it defaults to the rail name.
	Reason string `json:"reason,omitzero"`
	// Action is the action kind (dispatch, set, notify, cancel, link, effect, debounce).
	Action string `json:"action,omitzero"`
	// TaskSpec matches dispatch and cancel; Queue matches dispatch; Key matches set.
	TaskSpec string `json:"taskSpec,omitzero"`
	Queue    string `json:"queue,omitzero"`
	Key      string `json:"key,omitzero"`
}

// Check returns the verdict of the first matching trip, or Allowed.
func (f FakeRails) Check(a ActionIntent) v1alpha1.RailVerdict {
	for _, t := range f.Trips {
		if !t.matches(a) {
			continue
		}
		reason := t.Reason
		if reason == "" {
			reason = t.Rail
		}
		if t.Rail == RailBudget {
			return Deferred(t.Rail, reason)
		}
		return Suppressed(t.Rail, reason)
	}
	return Allowed()
}

func (t RailTrip) matches(a ActionIntent) bool {
	if t.Action != "" && t.Action != a.Kind() {
		return false
	}
	var taskSpec, queue, key string
	switch {
	case a.Dispatch != nil:
		taskSpec, queue = a.Dispatch.TaskSpec, a.Dispatch.Queue
	case a.Cancel != nil:
		taskSpec = a.Cancel.TaskSpec
	case a.Set != nil:
		key = a.Set.Key
	}
	return (t.TaskSpec == "" || t.TaskSpec == taskSpec) &&
		(t.Queue == "" || t.Queue == queue) &&
		(t.Key == "" || t.Key == key)
}

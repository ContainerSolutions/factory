package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/cel"
)

// ActionIntent is one materialised action: every "=CEL" value evaluated, ready for the rule
// controller to apply. Exactly one member is set.
type ActionIntent struct {
	Dispatch *Dispatch `json:"dispatch,omitzero"`
	Set      *Set      `json:"set,omitzero"`
	Notify   *Notify   `json:"notify,omitzero"`
	Cancel   *Cancel   `json:"cancel,omitzero"`
	Link     *Link     `json:"link,omitzero"`
	Effect   *Effect   `json:"effect,omitzero"`
	Debounce *Debounce `json:"debounce,omitzero"`
	// Unsupported is an action the engine accepts but doesn't run (requestApproval, Q18).
	Unsupported *Unsupported `json:"unsupported,omitzero"`
}

// Action kinds (ActionIntent.Kind).
const (
	KindDispatch    = "dispatch"
	KindSet         = "set"
	KindNotify      = "notify"
	KindCancel      = "cancel"
	KindLink        = "link"
	KindEffect      = "effect"
	KindDebounce    = "debounce"
	KindUnsupported = "unsupported"
)

// Kind returns the member that is set ("" for none).
func (a ActionIntent) Kind() string {
	switch {
	case a.Dispatch != nil:
		return KindDispatch
	case a.Set != nil:
		return KindSet
	case a.Notify != nil:
		return KindNotify
	case a.Cancel != nil:
		return KindCancel
	case a.Link != nil:
		return KindLink
	case a.Effect != nil:
		return KindEffect
	case a.Debounce != nil:
		return KindDebounce
	case a.Unsupported != nil:
		return KindUnsupported
	default:
		return ""
	}
}

// String is a one-line summary for decision records ("dispatch implement → poc").
func (a ActionIntent) String() string {
	switch {
	case a.Dispatch != nil:
		return fmt.Sprintf("dispatch %s → %s", a.Dispatch.TaskSpec, a.Dispatch.Queue)
	case a.Set != nil:
		return "set " + a.Set.Key
	case a.Notify != nil:
		return "notify " + a.Notify.Template
	case a.Cancel != nil:
		return "cancel " + a.Cancel.TaskSpec
	case a.Link != nil:
		return "link " + a.Link.Ref.Ref
	case a.Effect != nil:
		return "effect " + string(a.Effect.Op)
	case a.Debounce != nil:
		return "debounce " + a.Debounce.Key
	case a.Unsupported != nil:
		return "unsupported " + a.Unsupported.Action
	default:
		return ""
	}
}

// Dispatch creates a Task (Pending) in a queue.
type Dispatch struct {
	TaskSpec string `json:"taskSpec"`
	Queue    string `json:"queue"`
	// Inputs are the evaluated dispatch inputs, checked against the TaskSpec's declared
	// inputs (defaults are not filled in: the controller compiles the Task).
	Inputs   map[string]any           `json:"inputs,omitzero"`
	Harness  *v1alpha1.HarnessRequest `json:"harness,omitzero"`
	Requires *v1alpha1.Requires       `json:"requires,omitzero"`
	Context  []string                 `json:"context,omitzero"`
	Target   *v1alpha1.Target         `json:"target,omitzero"`
}

// Set writes one work-item state key.
type Set struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// Notify sends a templated message.
type Notify struct {
	Template string                 `json:"template"`
	To       []v1alpha1.Destination `json:"to"`
}

// Cancel cancels the work item's active tasks of a TaskSpec.
type Cancel struct {
	TaskSpec string `json:"taskSpec"`
}

// Link adds a provider object to the work item.
type Link struct {
	Ref v1alpha1.ObjectRef `json:"ref"`
}

// Effect is a direct provider effect.
type Effect struct {
	Op     v1alpha1.EffectOp `json:"op"`
	Target string            `json:"target,omitzero"`
	Args   map[string]any    `json:"args,omitzero"`
}

// Debounce holds a debounced rule's actions until Until. The controller keeps one pending
// Debounce per Key and applies Then (after re-checking the rails) at Until; a later match
// with the same Key while one is pending changes nothing (first wins).
type Debounce struct {
	// Key is "<rule>/<value of debounce.key>" (the work item id when the rule has no key),
	// so two rules never share a key.
	Key   string    `json:"key"`
	Until time.Time `json:"until"`
	// Then are the rule's materialised actions, in order. Their sets are not in
	// Evaluation.StateAfter: they apply when the debounce fires.
	Then []ActionIntent `json:"then"`
}

// Unsupported is an action the engine accepts but does not run.
type Unsupported struct {
	// Action is the action type (requestApproval).
	Action string `json:"action"`
	Reason string `json:"reason"`
	// RequestApproval is what the rule asked for.
	RequestApproval *v1alpha1.ApprovalSpec `json:"requestApproval,omitzero"`
}

// unsupportedApproval is the reason of an Unsupported requestApproval.
const unsupportedApproval = "requestApproval is not supported in the PoC (Q18)"

// actionError is a failure to materialise one action.
type actionError struct {
	path string // "actions[1].dispatch"
	err  error
}

func (e *actionError) Error() string { return e.path + ": " + e.err.Error() }
func (e *actionError) Unwrap() error { return e.err }

// materialise evaluates one compiled action into intents (a set yields one per key, in key
// order). act must carry the staged state.
func materialise(ctx context.Context, c *catalog.Catalog, ca catalog.CompiledAction, act cel.Activation) ([]ActionIntent, error) {
	if ca.Type == "requestApproval" {
		var spec *v1alpha1.ApprovalSpec
		if ra := ca.Action.RequestApproval; ra != nil {
			cp := *ra // the catalog is shared and immutable
			spec = &cp
		}
		return []ActionIntent{{Unsupported: &Unsupported{Action: ca.Type, Reason: unsupportedApproval, RequestApproval: spec}}}, nil
	}
	tree, err := cel.EvalTree(ctx, ca.Tree, act, c.Programs)
	if err != nil {
		return nil, err
	}
	switch ca.Type {
	case "dispatch":
		d, err := dispatchIntent(c, tree)
		if err != nil {
			return nil, err
		}
		return []ActionIntent{{Dispatch: d}}, nil
	case "set":
		m, ok := tree.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("set is %T, want a map", tree)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]ActionIntent, 0, len(keys))
		for _, k := range keys {
			out = append(out, ActionIntent{Set: &Set{Key: k, Value: m[k]}})
		}
		return out, nil
	case "notify":
		var n v1alpha1.NotifyAction
		if err := decode(tree, &n); err != nil {
			return nil, err
		}
		if c.Templates[n.Template] == nil {
			return nil, fmt.Errorf("template %q does not exist", n.Template)
		}
		return []ActionIntent{{Notify: &Notify{Template: n.Template, To: n.To}}}, nil
	case "cancel":
		var x v1alpha1.CancelAction
		if err := decode(tree, &x); err != nil {
			return nil, err
		}
		if c.TaskSpecs[x.TaskSpec] == nil {
			return nil, fmt.Errorf("TaskSpec %q does not exist", x.TaskSpec)
		}
		return []ActionIntent{{Cancel: &Cancel{TaskSpec: x.TaskSpec}}}, nil
	case "link":
		ref, err := linkRef(tree)
		if err != nil {
			return nil, err
		}
		return []ActionIntent{{Link: &Link{Ref: ref}}}, nil
	case "effect":
		var x v1alpha1.EffectAction
		if err := decode(tree, &x); err != nil {
			return nil, err
		}
		if !x.Op.Known() {
			return nil, fmt.Errorf("unknown effect op %q", x.Op)
		}
		return []ActionIntent{{Effect: &Effect{Op: x.Op, Target: x.Target, Args: x.Args}}}, nil
	default:
		return nil, fmt.Errorf("unknown action type %q", ca.Type)
	}
}

func dispatchIntent(c *catalog.Catalog, tree any) (*Dispatch, error) {
	var x v1alpha1.DispatchAction
	if err := decode(tree, &x); err != nil {
		return nil, err
	}
	ts := c.TaskSpecs[x.TaskSpec]
	if ts == nil {
		return nil, fmt.Errorf("TaskSpec %q does not exist", x.TaskSpec)
	}
	if c.Queues[x.Queue] == nil {
		return nil, fmt.Errorf("queue %q does not exist", x.Queue)
	}
	d := &Dispatch{TaskSpec: x.TaskSpec, Queue: x.Queue, Requires: x.Requires, Context: x.Context, Target: x.Target}
	if m, ok := tree.(map[string]any); ok {
		// The evaluated values as they are (decode turns every number into a float64).
		d.Inputs, _ = m["inputs"].(map[string]any)
	}
	if errs := ts.ValidateInputs(d.Inputs); len(errs) > 0 {
		return nil, fmt.Errorf("inputs: %w", errors.Join(errs...))
	}
	if h := x.Harness; h != nil {
		if h.IsExpr() {
			return nil, fmt.Errorf("harness is the string %q, want an object", h.Expr)
		}
		req := h.Value
		d.Harness = &req
	}
	return d, nil
}

// linkRef reads link.from: an object ref ({provider, kind, ref}) or a ref string.
func linkRef(tree any) (v1alpha1.ObjectRef, error) {
	m, ok := tree.(map[string]any)
	if !ok {
		return v1alpha1.ObjectRef{}, fmt.Errorf("link is %T, want a map", tree)
	}
	switch from := m["from"].(type) {
	case string:
		if from == "" {
			return v1alpha1.ObjectRef{}, errors.New("link.from is empty")
		}
		return v1alpha1.ObjectRef{Ref: from}, nil
	case map[string]any:
		var ref v1alpha1.ObjectRef
		if err := decode(from, &ref); err != nil {
			return v1alpha1.ObjectRef{}, fmt.Errorf("link.from: %w", err)
		}
		if ref.Ref == "" {
			return v1alpha1.ObjectRef{}, errors.New("link.from has no ref")
		}
		return ref, nil
	default:
		return v1alpha1.ObjectRef{}, fmt.Errorf("link.from is %T, want an object ref or a string", from)
	}
}

// decode converts an evaluated tree into a typed action, rejecting unknown fields.
func decode(tree any, out any) error {
	b, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

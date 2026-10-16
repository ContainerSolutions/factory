package v1alpha1

import (
	"fmt"
	"time"
)

// TypeMeta is embedded in every kind. APIVersion may be omitted in documents and is never
// added on output.
type TypeMeta struct {
	APIVersion string `json:"apiVersion,omitzero"`
	Kind       string `json:"kind"`
}

// GetTypeMeta gives access to the kind and apiVersion of any Object.
func (t *TypeMeta) GetTypeMeta() *TypeMeta { return t }

// ObjectMeta is the metadata of every kind except Task (TaskMeta) and Event (CloudEvents
// attributes).
type ObjectMeta struct {
	Name            string            `json:"name"`
	Labels          map[string]string `json:"labels,omitzero"`
	ResourceVersion string            `json:"resourceVersion,omitzero"` // runtime kinds only
	CreatedAt       time.Time         `json:"createdAt,omitzero"`
}

// Object is any decoded kind.
type Object interface {
	GetTypeMeta() *TypeMeta
	// GetName is metadata.name, or the id for kinds named by id (Task, Event).
	GetName() string
}

// Kind names.
const (
	KindProvider      = "Provider"
	KindGroup         = "Group"
	KindTemplate      = "Template"
	KindRule          = "Rule"
	KindRuleSet       = "RuleSet"
	KindTaskSpec      = "TaskSpec"
	KindQueue         = "Queue"
	KindPool          = "Pool"
	KindEvent         = "Event"
	KindWorkItem      = "WorkItem"
	KindTask          = "Task"
	KindRun           = "Run"
	KindAgent         = "Agent"
	KindEffect        = "Effect"
	KindBudgetCounter = "BudgetCounter"
	KindApproval      = "Approval"
)

var configKinds = []string{
	KindProvider, KindGroup, KindTemplate, KindRule, KindRuleSet, KindTaskSpec, KindQueue, KindPool,
}

var runtimeKinds = []string{
	KindEvent, KindWorkItem, KindTask, KindRun, KindAgent, KindEffect, KindBudgetCounter, KindApproval,
}

// ConfigKinds lists the kinds that live in the catalog, in a fixed order.
func ConfigKinds() []string { return append([]string(nil), configKinds...) }

// RuntimeKinds lists the kinds that live in the dispatcher's store, in a fixed order.
func RuntimeKinds() []string { return append([]string(nil), runtimeKinds...) }

// IsConfigKind reports whether kind is a catalog kind.
func IsConfigKind(kind string) bool {
	for _, k := range configKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// New returns an empty object of the given kind, with its Kind set.
func New(kind string) (Object, error) {
	var o Object
	switch kind {
	case KindProvider:
		o = &Provider{}
	case KindGroup:
		o = &Group{}
	case KindTemplate:
		o = &Template{}
	case KindRule:
		o = &Rule{}
	case KindRuleSet:
		o = &RuleSet{}
	case KindTaskSpec:
		o = &TaskSpec{}
	case KindQueue:
		o = &Queue{}
	case KindPool:
		o = &Pool{}
	case KindEvent:
		o = &Event{}
	case KindWorkItem:
		o = &WorkItem{}
	case KindTask:
		o = &Task{}
	case KindRun:
		o = &Run{}
	case KindAgent:
		o = &Agent{}
	case KindEffect:
		o = &Effect{}
	case KindBudgetCounter:
		o = &BudgetCounter{}
	case KindApproval:
		o = &Approval{}
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownKind, kind)
	}
	o.GetTypeMeta().Kind = kind
	return o, nil
}

// LabelSelector selects objects by their labels, as in Kubernetes.
type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels,omitzero"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitzero"`
}

// LabelSelectorRequirement is one matchExpressions entry.
type LabelSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator" jsonschema:"enum=In,enum=NotIn,enum=Exists,enum=DoesNotExist"`
	Values   []string `json:"values,omitzero"`
}

// SecretRef points at a secret held outside the catalog. Secrets are never inline.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ObjectRef identifies a provider object, e.g. {github-public, issue, compliance-framework/docs#42}.
type ObjectRef struct {
	Provider string `json:"provider,omitzero"`
	Kind     string `json:"kind,omitzero"`
	Ref      string `json:"ref"`
}

// Target says which repo or object the work happens on, when it isn't the event's object.
type Target struct {
	Provider string `json:"provider,omitzero"`
	Repo     string `json:"repo,omitzero"`
	Ref      string `json:"ref,omitzero"`
}

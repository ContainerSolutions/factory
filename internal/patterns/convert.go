package patterns

import (
	"fmt"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// ConstraintFrom compiles a TaskSpec harness constraint. An unset field matches anything.
func ConstraintFrom(c v1alpha1.HarnessConstraint) (Constraint, error) {
	models, err := compileField(c.Models, "models")
	if err != nil {
		return Constraint{}, err
	}
	effort, err := compileField(c.ReasoningEffort, "reasoningEffort")
	if err != nil {
		return Constraint{}, err
	}
	return Constraint{Harness: c.Name, Models: models, Effort: effort}, nil
}

// ConstraintsFrom compiles every constraint of a TaskSpec's harness.allowed, in order.
func ConstraintsFrom(cs []v1alpha1.HarnessConstraint) ([]Constraint, error) {
	out := make([]Constraint, 0, len(cs))
	for i, c := range cs {
		cc, err := ConstraintFrom(c)
		if err != nil {
			return nil, fmt.Errorf("allowed[%d]: %w", i, err)
		}
		out = append(out, cc)
	}
	return out, nil
}

// RequestFrom compiles a harness request. A nil config or an unset field matches anything.
func RequestFrom(r v1alpha1.HarnessRequest) (Request, error) {
	req := Request{Harness: r.Name}
	if r.Config == nil {
		return req, nil
	}
	var err error
	if req.Model, err = compileField(r.Config.Model, "model"); err != nil {
		return Request{}, err
	}
	if req.Effort, err = compileField(r.Config.ReasoningEffort, "reasoningEffort"); err != nil {
		return Request{}, err
	}
	req.Prefer = r.Config.Prefer
	return req, nil
}

// OfferFrom builds an Offer from an agent's probed HarnessOffer.
func OfferFrom(o v1alpha1.HarnessOffer) Offer {
	return Offer{Harness: o.Name, Models: o.Models, Efforts: o.ReasoningEffort, DefaultModel: o.DefaultModel}
}

// HarnessResolved returns r as written into Task.spec.harness.resolved, taking the harness
// version and auth from the offer it was resolved against.
func (r Resolved) HarnessResolved(off v1alpha1.HarnessOffer) v1alpha1.HarnessResolved {
	return v1alpha1.HarnessResolved{
		Name:            r.Harness,
		Version:         off.Version,
		Auth:            off.Auth,
		Model:           r.Model,
		ReasoningEffort: r.Effort,
	}
}

func compileField(v v1alpha1.StringOrList, field string) (Pattern, error) {
	if v.IsZero() {
		return Any(), nil
	}
	p, err := Compile(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return p, nil
}

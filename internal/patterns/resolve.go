package patterns

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// DefaultEffort is the effort chosen when the request does not rank efforts (e.g. `*`) and the
// agent offers it; otherwise the first offered candidate is chosen.
const DefaultEffort = "medium"

// Constraint bounds what one harness may run (from TaskSpec.harness.allowed). A nil Pattern
// matches anything.
type Constraint struct {
	Harness string
	Models  Pattern
	Effort  Pattern
}

// Request asks for a harness config (rule, ruleset or TaskSpec default). A nil Pattern matches
// anything. Prefer lists exact model ids, most preferred first.
type Request struct {
	Harness string
	Model   Pattern
	Effort  Pattern
	Prefer  []string
}

// Offer is what an agent can run with one harness: concrete values, in the agent's order.
type Offer struct {
	Harness      string
	Models       []string
	Efforts      []string
	DefaultModel string
}

// Resolved is the concrete harness config chosen for a task.
type Resolved struct {
	Harness, Model, Effort string
}

// Reason says which step of Resolve found no candidate.
type Reason string

// The reasons a NoMatchError carries.
const (
	ReasonHarness Reason = "harness"
	ReasonModel   Reason = "model"
	ReasonEffort  Reason = "effort"
)

// ErrNoMatch is matched by every *NoMatchError, so errors.Is(err, ErrNoMatch) tells the
// scheduler the agent cannot run the task (no-capable-agent once no agent can).
var ErrNoMatch = errors.New("no match")

// NoMatchError says why an offer cannot satisfy a request within its constraints.
type NoMatchError struct {
	Reason  Reason
	Harness string
}

func (e *NoMatchError) Error() string {
	return fmt.Sprintf("%s: no %s for harness %q satisfies the constraint, request and offer", ErrNoMatch, e.Reason, e.Harness)
}

// Is makes errors.Is(err, ErrNoMatch) true.
func (e *NoMatchError) Is(target error) bool { return target == ErrNoMatch }

// Resolve picks the harness config an agent's offer runs for a request, within the first
// constraint for the requested harness (design/workstreams/dispatcher-core/LLD/patterns.md):
//
//  1. the offer and some constraint must be for the requested harness;
//  2. candidate models = offer ∩ constraint ∩ request, in offer order;
//  3. the model is the first candidate in req.Prefer; else, when req.Model is a list, the
//     candidates matching its earliest matching element are kept; then the offer's default
//     model if it is still a candidate; else the highest version (see compareVersions);
//  4. candidate efforts = offer ∩ constraint ∩ request; when req.Effort is a list, those
//     matching its earliest matching element are kept; then DefaultEffort if it is still a
//     candidate, else the first one in offer order;
//  5. the result.
//
// When a step has no candidate, the error is a *NoMatchError with that step's Reason.
func Resolve(cs []Constraint, req Request, off Offer) (Resolved, error) {
	i := slices.IndexFunc(cs, func(c Constraint) bool { return c.Harness == req.Harness })
	if req.Harness != off.Harness || i < 0 {
		return Resolved{}, &NoMatchError{Reason: ReasonHarness, Harness: req.Harness}
	}
	c := cs[i]

	models := candidates(off.Models, c.Models, req.Model)
	if len(models) == 0 {
		return Resolved{}, &NoMatchError{Reason: ReasonModel, Harness: req.Harness}
	}
	efforts := candidates(off.Efforts, c.Effort, req.Effort)
	if len(efforts) == 0 {
		return Resolved{}, &NoMatchError{Reason: ReasonEffort, Harness: req.Harness}
	}
	return Resolved{
		Harness: req.Harness,
		Model:   chooseModel(models, req, off.DefaultModel),
		Effort:  chooseEffort(efforts, req.Effort),
	}, nil
}

// Subsumes reports whether at least one of the known models (an adapter's capabilities file)
// satisfies both the constraint and the request, and returns the first such model as the
// witness. Catalog validation uses it to flag requests that can never fit their constraint.
func Subsumes(c Constraint, req Request, known []string) (ok bool, witness string) {
	if c.Harness != req.Harness {
		return false, ""
	}
	for _, m := range known {
		if match(c.Models, m) && match(req.Model, m) {
			return true, m
		}
	}
	return false, ""
}

// candidates are the offered values both patterns match, in offer order.
func candidates(offered []string, constraint, request Pattern) []string {
	var out []string
	for _, v := range offered {
		if match(constraint, v) && match(request, v) {
			out = append(out, v)
		}
	}
	return out
}

func chooseModel(models []string, req Request, defaultModel string) string {
	for _, p := range req.Prefer {
		if slices.Contains(models, p) {
			return p
		}
	}
	models = bestRanked(models, req.Model)
	if defaultModel != "" && slices.Contains(models, defaultModel) {
		return defaultModel
	}
	return slices.MinFunc(models, func(a, b string) int {
		if c := compareVersions(b, a); c != 0 { // highest version first
			return c
		}
		return strings.Compare(a, b)
	})
}

func chooseEffort(efforts []string, req Pattern) string {
	efforts = bestRanked(efforts, req)
	if slices.Contains(efforts, DefaultEffort) {
		return DefaultEffort
	}
	return efforts[0]
}

// bestRanked keeps the values matching the earliest matching element of p when p is a list;
// otherwise it returns values unchanged. Every value must match p.
func bestRanked(values []string, p Pattern) []string {
	l, ok := p.(listPattern)
	if !ok {
		return values
	}
	best := slices.MinFunc(values, func(a, b string) int { return l.rank(a) - l.rank(b) })
	r := l.rank(best)
	return slices.DeleteFunc(slices.Clone(values), func(v string) bool { return l.rank(v) != r })
}

func match(p Pattern, s string) bool { return p == nil || p.Match(s) }

// compareVersions orders model ids by their numeric groups: the runs of digits delimited by
// `-`, `.` or the ends of the id (`claude-sonnet-5-5` → [5 5], `gpt-5.1-codex` → [5 1]),
// compared element by element, a missing element sorting lowest. The sign of the result
// orders a and b, as for strings.Compare.
func compareVersions(a, b string) int {
	ka, kb := versionKey(a), versionKey(b)
	for i := range min(len(ka), len(kb)) {
		if c := compareDigits(ka[i], kb[i]); c != 0 {
			return c
		}
	}
	return len(ka) - len(kb)
}

func versionKey(id string) []string {
	var key []string
	for _, f := range strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '.' }) {
		if strings.Trim(f, "0123456789") == "" {
			key = append(key, f)
		}
	}
	return key
}

// compareDigits compares two non-empty digit strings numerically, whatever their length.
func compareDigits(a, b string) int {
	a, b = trimZeros(a), trimZeros(b)
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

func trimZeros(s string) string {
	if t := strings.TrimLeft(s, "0"); t != "" {
		return t
	}
	return "0"
}

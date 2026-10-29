package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
)

// Fixture files (rules-engine LLD §Fixtures). A case is a directory that holds them:
//
//	event.json     a normalised v1alpha1.Event
//	workitem.json  the WorkItemView before
//	rails.json     optional: the tripped rails (FakeRails); without it every action is allowed
//	expected.json  the golden evaluation (Golden), written by `factory rules test --update`
//	rendered/      the golden prompts and Template bodies its decisions render, written by
//	               `factory rules test --update` (not read here)
const (
	FixtureEvent    = "event.json"
	FixtureWorkItem = "workitem.json"
	FixtureRails    = "rails.json"
	FixtureExpected = "expected.json"
	FixtureRendered = "rendered"
)

// Fixture is one recorded case: an event, the work item before it, the tripped rails and
// the golden evaluation.
type Fixture struct {
	// Name is the case's directory, slash-separated and relative to the fixtures root
	// ("poc/train-mock").
	Name     string
	Event    v1alpha1.Event
	WorkItem WorkItemView
	// Rails are the tripped rails of rails.json; nil when the case has none.
	Rails *FakeRails
	// Expected is the content of expected.json; nil when the case has none yet.
	Expected []byte
}

// LoadFixtures reads every case under fsys: each directory holding an event.json, at any
// depth (a case's own subdirectories are not searched). The cases are sorted by name.
func LoadFixtures(fsys fs.FS) ([]Fixture, error) {
	var dirs []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if _, err := fs.Stat(fsys, path.Join(p, FixtureEvent)); err == nil {
			dirs = append(dirs, p)
			return fs.SkipDir
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rules: fixtures: %w", err)
	}
	sort.Strings(dirs)
	out := make([]Fixture, 0, len(dirs))
	for _, d := range dirs {
		f, err := ReadFixture(fsys, d)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// ReadFixture reads the case in the directory dir of fsys. Unknown JSON fields are errors, so
// a fixture can't silently drift from the types.
func ReadFixture(fsys fs.FS, dir string) (Fixture, error) {
	f := Fixture{Name: dir}
	if err := readFixtureJSON(fsys, path.Join(dir, FixtureEvent), &f.Event); err != nil {
		return Fixture{}, err
	}
	if err := readFixtureJSON(fsys, path.Join(dir, FixtureWorkItem), &f.WorkItem); err != nil {
		return Fixture{}, err
	}
	switch err := readFixtureJSON(fsys, path.Join(dir, FixtureRails), &f.Rails); {
	case errors.Is(err, fs.ErrNotExist):
		f.Rails = nil
	case err != nil:
		return Fixture{}, err
	}
	b, err := fs.ReadFile(fsys, path.Join(dir, FixtureExpected))
	switch {
	case err == nil:
		f.Expected = b
	case !errors.Is(err, fs.ErrNotExist):
		return Fixture{}, fmt.Errorf("rules: fixture %s: %w", dir, err)
	}
	return f, nil
}

func readFixtureJSON(fsys fs.FS, file string, v any) error {
	b, err := fs.ReadFile(fsys, file)
	if err != nil {
		return fmt.Errorf("rules: fixture: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("rules: fixture %s: %w", file, err)
	}
	return nil
}

// RailsOrAllow is the case's rails: its FakeRails, or AllowAll when it has none.
func (f Fixture) RailsOrAllow() Rails {
	if f.Rails == nil {
		return AllowAll
	}
	return *f.Rails
}

// Evaluate replays the case against the catalog c. It uses an Engine without a Clock, so
// `now` is the event's time and the result doesn't depend on when it runs.
func (f Fixture) Evaluate(ctx context.Context, c *catalog.Catalog) (Evaluation, error) {
	ev, err := (&Engine{}).Evaluate(ctx, c, f.Event, f.WorkItem, f.RailsOrAllow())
	if err != nil {
		return Evaluation{}, fmt.Errorf("rules: fixture %s: %w", f.Name, err)
	}
	return ev, nil
}

// GoldenEvaluation is an Evaluation as expected.json holds it: the where inputs are
// redacted to their keys, so a golden pins which values a rule read but not the values.
type GoldenEvaluation struct {
	Event      string           `json:"event"`
	WorkItem   string           `json:"workItem"`
	Matches    []GoldenMatch    `json:"matches"`
	Decisions  []GoldenDecision `json:"decisions"`
	StateAfter map[string]any   `json:"stateAfter"`
}

// GoldenMatch is a Match with its inputs redacted to their keys.
type GoldenMatch struct {
	Match
	Inputs []string `json:"inputs,omitzero"`
}

// GoldenDecision is a Decision with its inputs redacted to their keys.
type GoldenDecision struct {
	Decision
	Inputs []string `json:"inputs,omitzero"`
}

// ToGolden redacts ev for a golden file.
func ToGolden(ev Evaluation) GoldenEvaluation {
	g := GoldenEvaluation{
		Event: ev.Event, WorkItem: ev.WorkItem,
		Matches: make([]GoldenMatch, 0, len(ev.Matches)), Decisions: make([]GoldenDecision, 0, len(ev.Decisions)),
		StateAfter: ev.StateAfter,
	}
	for _, m := range ev.Matches {
		keys := sortedKeys(m.Inputs)
		m.Inputs = nil
		g.Matches = append(g.Matches, GoldenMatch{Match: m, Inputs: keys})
	}
	for _, d := range ev.Decisions {
		keys := sortedKeys(d.Inputs)
		d.Inputs = nil
		g.Decisions = append(g.Decisions, GoldenDecision{Decision: d, Inputs: keys})
	}
	return g
}

// Golden is the content of expected.json for ev: ToGolden, indented, with a final newline.
func Golden(ev Evaluation) ([]byte, error) {
	return ToGolden(ev).Marshal()
}

// Marshal encodes g as expected.json holds it.
func (g GoldenEvaluation) Marshal() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		return nil, fmt.Errorf("rules: golden: %w", err)
	}
	return b.Bytes(), nil
}

// ParseGolden decodes an expected.json.
func ParseGolden(b []byte) (GoldenEvaluation, error) {
	var g GoldenEvaluation
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&g); err != nil {
		return GoldenEvaluation{}, fmt.Errorf("rules: golden: %w", err)
	}
	return g, nil
}

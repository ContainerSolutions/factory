package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/rules"
)

// renderedDir is the directory of a case's rendered goldens, next to its expected.json: one
// file per prompt or Template body its decisions render, <decision>-<prompt|template>-<name>.md.
const renderedDir = rules.FixtureRendered

// Rendered golden statuses besides caseOK, caseDiffers, caseMissing and caseUpdated.
const (
	renderStale   = "stale"   // a golden no decision renders
	renderRemoved = "removed" // --update removed a stale golden
)

// badRendered is what text/template prints for a missing field or a value it can't print as
// text (catalog_test.go's checkRendered): a rendered text holding one fails its case.
var badRendered = []string{"<no value>", "<nil>", "%!", "map["}

// renderReport is one rendered golden that doesn't match, or that --update rewrote.
type renderReport struct {
	File   string `json:"file"`
	Status string `json:"status"`
	Note   string `json:"note,omitzero"`
}

// renderSeen are the TaskSpecs and Templates some case renders (--coverage).
type renderSeen struct{ taskSpecs, templates map[string]bool }

// renderCase renders the case's prompts and Templates as the dispatcher does
// (controllers.RenderEvaluation), checks its run result against its TaskSpec's output schema
// (controllers.CheckRunResult) and compares the texts with the goldens under caseDir/rendered,
// rewriting them with update. It returns the goldens that don't match (or that update
// rewrote) and the render and schema failures; an error is an unreadable or unwritable file.
func renderCase(ctx context.Context, c *catalog.Catalog, f rules.Fixture, ev rules.Evaluation, caseDir string, update bool, seen renderSeen) ([]renderReport, []string, error) {
	var failures []string
	if spec, err := controllers.CheckRunResult(c, f.Event); err != nil {
		failures = append(failures, fmt.Sprintf("result of the %s run: %v", spec, err))
	}
	dir := filepath.Join(caseDir, renderedDir)
	want := map[string]bool{} // every golden a decision renders, failed renders included
	var reps []renderReport
	for _, r := range controllers.RenderEvaluation(ctx, c, f.Event, f.WorkItem, ev) {
		label := "TaskSpec " + r.Name
		if r.Kind == controllers.RenderTemplate {
			label = "Template " + r.Name
			seen.templates[r.Name] = true
		} else {
			seen.taskSpecs[r.Name] = true
		}
		name := fmt.Sprintf("%d-%s-%s.md", r.Decision, r.Kind, r.Name)
		want[name] = true
		if r.Err == nil {
			if i := slices.IndexFunc(badRendered, func(b string) bool { return strings.Contains(r.Text, b) }); i >= 0 {
				r.Err = fmt.Errorf("the text contains %q: a missing field or a value printed raw", badRendered[i])
			}
		}
		if r.Err != nil {
			failures = append(failures, label+": "+r.Err.Error())
			continue
		}
		rep, err := renderGolden(dir, name, r.Text, update)
		if err != nil {
			return nil, nil, err
		}
		if rep.Status != caseOK {
			reps = append(reps, rep)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || want[e.Name()] {
			continue
		}
		rep := renderReport{File: renderedDir + "/" + e.Name(), Status: renderStale, Note: "no decision renders it (run with --update)"}
		if update {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return nil, nil, fmt.Errorf("--update: %w", err)
			}
			rep.Status, rep.Note = renderRemoved, ""
		}
		reps = append(reps, rep)
	}
	if update && len(want) == 0 && len(entries) > 0 {
		_ = os.Remove(dir) // only when empty: a directory it doesn't know is kept
	}
	return reps, failures, nil
}

// renderGolden compares text with the golden dir/name and, with update, rewrites a golden that
// differs or is missing.
func renderGolden(dir, name, text string, update bool) (renderReport, error) {
	rep := renderReport{File: renderedDir + "/" + name, Status: caseOK}
	file := filepath.Join(dir, name)
	got, err := os.ReadFile(file) //nolint:gosec // a golden under the fixtures directory
	switch {
	case err == nil && string(got) == text:
		return rep, nil
	case err == nil:
		rep.Status, rep.Note = caseDiffers, firstDiff(string(got), text)
	case errors.Is(err, fs.ErrNotExist):
		rep.Status, rep.Note = caseMissing, "no golden (run with --update)"
	default:
		return rep, err
	}
	if !update {
		return rep, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // goldens are checked-in, world-readable files
		return rep, fmt.Errorf("--update: %w", err)
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil { //nolint:gosec // goldens are checked-in, world-readable files
		return rep, fmt.Errorf("--update: %w", err)
	}
	rep.Note = ""
	if rep.Status == caseMissing {
		rep.Note = "created"
	}
	rep.Status = caseUpdated
	return rep, nil
}

// firstDiff is the first line where a golden (want) and a different text (got) differ.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	line := func(ls []string, i int) string {
		if i < len(ls) {
			return fmt.Sprintf("%q", ls[i])
		}
		return "end of text"
	}
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	return fmt.Sprintf("line %d: - %s, + %s", i+1, line(w, i), line(g, i))
}

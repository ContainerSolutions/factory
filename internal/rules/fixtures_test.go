package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
)

// Fixture format (rules-engine LLD §Fixtures): testdata/cases/<catalog>/<case>/ holds
//
//	event.json     a normalised v1alpha1.Event
//	workitem.json  the WorkItemView before
//	rails.json     optional: the tripped rails (FakeRails)
//	expected.json  the golden Evaluation, written with -update; inputs are redacted to keys
//
// <catalog> is poc (internal/catalog/testdata/poc), poc-merge (poc with the merge rules
// selected) or a test-only catalog under testdata/catalogs.
var update = flag.Bool("update", false, "rewrite the expected.json goldens under testdata/cases")

// fixedNow is the engine clock of every fixture.
var fixedNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

const (
	casesDir   = "testdata/cases"
	pocDir     = "../catalog/testdata/poc"
	catalogDir = "testdata/catalogs"
)

var (
	catalogsMu sync.Mutex
	catalogs   = map[string]*catalog.Catalog{}
)

// loadCatalog loads (once) a fixture catalog by name.
func loadCatalog(t *testing.T, name string) *catalog.Catalog {
	t.Helper()
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	if c := catalogs[name]; c != nil {
		return c
	}
	var fsys fs.FS
	switch name {
	case "poc":
		fsys = os.DirFS(pocDir)
	case "poc-merge":
		fsys = overlay(t, os.DirFS(pocDir), os.DirFS(filepath.Join(catalogDir, name)))
	default:
		fsys = os.DirFS(filepath.Join(catalogDir, name))
	}
	c, errs := catalog.LoadDir(fsys, catalog.Options{})
	if c == nil {
		var b strings.Builder
		for _, e := range errs {
			b.WriteString(e.Error() + "\n")
		}
		t.Fatalf("catalog %s:\n%s", name, b.String())
	}
	catalogs[name] = c
	return c
}

// overlay is base with the files of top added or replaced.
func overlay(t *testing.T, base, top fs.FS) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	for _, fsys := range []fs.FS{base, top} {
		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			out[p] = &fstest.MapFile{Data: b}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

type fixture struct {
	Catalog, Name, Dir string
	Event              v1alpha1.Event
	WorkItem           WorkItemView
	Rails              FakeRails
}

// fixtures returns every case, sorted by catalog then name.
func fixtures(t *testing.T) []fixture {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(casesDir, "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(dirs)
	out := make([]fixture, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, readFixture(t, d))
	}
	return out
}

func readFixture(t *testing.T, dir string) fixture {
	t.Helper()
	f := fixture{Dir: dir, Name: filepath.Base(dir), Catalog: filepath.Base(filepath.Dir(dir))}
	readJSON(t, filepath.Join(dir, "event.json"), &f.Event)
	readJSON(t, filepath.Join(dir, "workitem.json"), &f.WorkItem)
	if _, err := os.Stat(filepath.Join(dir, "rails.json")); err == nil {
		readJSON(t, filepath.Join(dir, "rails.json"), &f.Rails)
	}
	return f
}

// runCase evaluates one case by its path under testdata/cases ("poc/plan-epic").
func runCase(t *testing.T, name string) Evaluation {
	t.Helper()
	return evaluate(t, readFixture(t, filepath.Join(casesDir, filepath.FromSlash(name))))
}

func evaluate(t *testing.T, f fixture) Evaluation {
	t.Helper()
	e := &Engine{Clock: ClockFunc(func() time.Time { return fixedNow })}
	ev, err := e.Evaluate(context.Background(), loadCatalog(t, f.Catalog), f.Event, f.WorkItem, f.Rails)
	if err != nil {
		t.Fatalf("%s: Evaluate: %v", f.Dir, err)
	}
	return ev
}

func readJSON(t *testing.T, file string, v any) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// golden is the evaluation as expected.json holds it: inputs redacted to their keys.
func golden(t *testing.T, ev Evaluation) []byte {
	t.Helper()
	type match struct {
		Match
		Inputs []string `json:"inputs,omitzero"`
	}
	type decision struct {
		Decision
		Inputs []string `json:"inputs,omitzero"`
	}
	out := struct {
		Event      string         `json:"event"`
		WorkItem   string         `json:"workItem"`
		Matches    []match        `json:"matches"`
		Decisions  []decision     `json:"decisions"`
		StateAfter map[string]any `json:"stateAfter"`
	}{Event: ev.Event, WorkItem: ev.WorkItem, Matches: []match{}, Decisions: []decision{}, StateAfter: ev.StateAfter}
	for _, m := range ev.Matches {
		out.Matches = append(out.Matches, match{Match: m, Inputs: sortedKeys(m.Inputs)})
	}
	for _, d := range ev.Decisions {
		out.Decisions = append(out.Decisions, decision{Decision: d, Inputs: sortedKeys(d.Inputs)})
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestFixtures runs every case and compares it with its golden.
func TestFixtures(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(f.Catalog+"/"+f.Name, func(t *testing.T) {
			got := golden(t, evaluate(t, f))
			file := filepath.Join(f.Dir, "expected.json")
			if *update {
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/rules -run TestFixtures -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("evaluation differs from %s (rerun with -update and review the diff):\n%s", file, got)
			}
		})
	}
}

// TestPoCRuleCoverage checks that every rule of the PoC catalog has a fixture where it fires
// and one where its where is false.
func TestPoCRuleCoverage(t *testing.T) {
	c := loadCatalog(t, "poc")
	fired, notFired := map[string]bool{}, map[string]bool{}
	for _, f := range fixtures(t) {
		if f.Catalog != "poc" && f.Catalog != "poc-merge" {
			continue
		}
		for _, m := range evaluate(t, f).Matches {
			switch m.Result {
			case Matched:
				fired[m.Rule] = true
			case NotMatched:
				notFired[m.Rule] = true
			}
		}
	}
	for _, name := range sortedKeys(c.Rules) {
		if strings.Contains(name, "/") {
			continue // inline rules: none in the PoC
		}
		if !fired[name] {
			t.Errorf("rule %s fires in no fixture", name)
		}
		if !notFired[name] {
			t.Errorf("rule %s has no fixture where its where is false", name)
		}
	}
}

// TestFixtureNames keeps the fixture tree tidy: every case has its files and nothing else.
func TestFixtureNames(t *testing.T) {
	allowed := []string{"event.json", "workitem.json", "rails.json", "expected.json"}
	for _, f := range fixtures(t) {
		entries, err := os.ReadDir(f.Dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !slices.Contains(allowed, e.Name()) {
				t.Errorf("%s: unexpected file %s", f.Dir, e.Name())
			}
		}
		if _, err := os.Stat(path.Join(filepath.ToSlash(f.Dir), "expected.json")); errors.Is(err, fs.ErrNotExist) && !*update {
			t.Errorf("%s: no expected.json", f.Dir)
		}
	}
}

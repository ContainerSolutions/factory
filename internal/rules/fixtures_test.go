package rules

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/compliance-framework/factory/internal/catalog"
)

// The cases (format in fixture.go) are testdata/cases/<catalog>/<case>/, and their goldens are
// written with -update. <catalog> is poc (internal/catalog/testdata/poc), poc-merge (poc with
// the merge rules selected) or a test-only catalog under testdata/catalogs. The cases under
// poc are also what `factory rules test` runs in cmd/factory's tests.
var update = flag.Bool("update", false, "rewrite the expected.json goldens under testdata/cases")

// fixedNow is a fixed clock for the engine tests that need one. Fixtures replay at their
// event's time (Fixture.Evaluate).
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

// fixtures returns every case, sorted by catalog then name.
func fixtures(t *testing.T) []Fixture {
	t.Helper()
	fs, err := LoadFixtures(os.DirFS(casesDir))
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// caseCatalog is the catalog a case runs against: the first element of its name.
func caseCatalog(f Fixture) string {
	c, _, _ := strings.Cut(f.Name, "/")
	return c
}

// readFixture reads one case by its path under testdata/cases ("poc/plan-epic").
func readFixture(t *testing.T, name string) Fixture {
	t.Helper()
	f, err := ReadFixture(os.DirFS(casesDir), name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// runCase evaluates one case by its path under testdata/cases ("poc/plan-epic").
func runCase(t *testing.T, name string) Evaluation {
	t.Helper()
	return evaluate(t, readFixture(t, name))
}

func evaluate(t *testing.T, f Fixture) Evaluation {
	t.Helper()
	ev, err := f.Evaluate(context.Background(), loadCatalog(t, caseCatalog(f)))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func golden(t *testing.T, ev Evaluation) []byte {
	t.Helper()
	b, err := Golden(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFixtures runs every case and compares it with its golden.
func TestFixtures(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			ev := evaluate(t, f)
			got := golden(t, ev)
			file := filepath.Join(casesDir, filepath.FromSlash(f.Name), FixtureExpected)
			if *update {
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			if f.Expected == nil {
				t.Fatalf("%s: no %s (run go test ./internal/rules -run TestFixtures -update)", f.Name, FixtureExpected)
			}
			if bytes.Equal(got, f.Expected) {
				return
			}
			want, err := ParseGolden(f.Expected)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			var b strings.Builder
			for _, c := range DiffGolden(want, ToGolden(ev)) {
				for _, l := range c.Lines() {
					b.WriteString("  " + l + "\n")
				}
			}
			t.Errorf("evaluation differs from %s (rerun with -update and review the diff):\n%s", file, b.String())
		})
	}
}

// TestPoCRuleCoverage checks that every rule of the PoC catalog has a fixture where it fires
// and one where its where is false.
func TestPoCRuleCoverage(t *testing.T) {
	c := loadCatalog(t, "poc")
	fired, notFired := map[string]bool{}, map[string]bool{}
	for _, f := range fixtures(t) {
		if c := caseCatalog(f); c != "poc" && c != "poc-merge" {
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
	allowed := []string{FixtureEvent, FixtureWorkItem, FixtureRails, FixtureExpected}
	for _, f := range fixtures(t) {
		dir := filepath.Join(casesDir, filepath.FromSlash(f.Name))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !slices.Contains(allowed, e.Name()) {
				t.Errorf("%s: unexpected file %s", dir, e.Name())
			}
		}
		if f.Expected == nil && !*update {
			t.Errorf("%s: no %s", dir, FixtureExpected)
		}
	}
}

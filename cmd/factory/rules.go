package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/rules"
)

// factory rules test exits like factory validate: 0 when every case matches its goldens (or
// with --update, or --base-catalog without --fail-on-diff), 1 on a mismatch, a missing
// golden, a prompt or Template that doesn't render, a run result its schema refuses (also
// with --update), a base/head difference with --fail-on-diff or an uncovered rule with
// --coverage, and 2 on a usage error, an invalid catalog or an unreadable fixture.
//
// Besides the decisions (expected.json), each case's goldens are the texts its decisions
// render (rendered/, rules_render.go). --base-catalog compares decisions only.

func rulesCommand() command {
	return command{
		name:    "rules",
		summary: "test the rules against recorded cases (rules test)",
		run:     runRules,
	}
}

func rulesUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: factory rules test [--catalog catalog] [--fixtures <catalog>/testdata] [--update] [--base-catalog dir [--fail-on-diff]] [--coverage] [--format text|json]")
}

func runRules(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		rulesUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "test":
		return runRulesTest(ctx, args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		rulesUsage(stdout)
		return exitValid
	}
	fmt.Fprintf(stderr, "factory rules: unknown command %q\n", args[0])
	rulesUsage(stderr)
	return exitUsage
}

// Case statuses.
const (
	caseOK      = "ok"      // matches its golden; with --base-catalog, the same with both catalogs
	caseDiffers = "differs" // differs from its golden
	caseMissing = "missing" // has no expected.json
	caseUpdated = "updated" // --update rewrote (or created) its golden
	caseChanged = "changed" // --base-catalog: the catalogs decide differently
	caseFailed  = "failed"  // a prompt or Template doesn't render, or the run result fails its schema
)

// rulesTestReport is the --format json output.
type rulesTestReport struct {
	Catalog     string       `json:"catalog"`
	BaseCatalog string       `json:"baseCatalog,omitzero"`
	Fixtures    string       `json:"fixtures"`
	Cases       []caseReport `json:"cases"`
	// Total is the number of cases; Differ those that differ from their goldens (or are
	// missing one), or, with --base-catalog, whose decisions change; Updated those --update
	// rewrote; Failed those that don't render or whose run result fails its schema.
	Total    int             `json:"total"`
	Differ   int             `json:"differ"`
	Updated  int             `json:"updated,omitzero"`
	Failed   int             `json:"failed,omitzero"`
	Coverage *coverageReport `json:"coverage,omitzero"`
}

type caseReport struct {
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	Note    string         `json:"note,omitzero"`
	Changes []rules.Change `json:"changes,omitzero"`
	// Rendered are the rendered goldens that don't match, or that --update rewrote.
	Rendered []renderReport `json:"rendered,omitzero"`
	// Failures are the render and run result schema failures.
	Failures []string `json:"failures,omitzero"`
}

// coverageReport lists the rules no case fires. Only the rules some RuleSet selects count:
// the others (e.g. the merge-* rules every PoC RuleSet excludes) can't fire. It also lists
// the TaskSpecs (with a prompt) and the Templates no case renders: a warning, not a failure.
type coverageReport struct {
	Selected    int      `json:"selected"`
	Fired       int      `json:"fired"`
	NeverFired  []string `json:"neverFired"`
	NotSelected []string `json:"notSelected,omitzero"`
	// PromptsNotRendered and TemplatesNotRendered are empty with --base-catalog, which
	// renders nothing.
	PromptsNotRendered   []string `json:"promptsNotRendered,omitzero"`
	TemplatesNotRendered []string `json:"templatesNotRendered,omitzero"`
}

func runRulesTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("factory rules test", flag.ContinueOnError)
	fl.SetOutput(stderr)
	catalogDir := fl.String("catalog", "catalog", "catalog directory")
	fixturesDir := fl.String("fixtures", "", "fixtures directory: every directory under it with an event.json is a case (default <catalog>/testdata)")
	update := fl.Bool("update", false, "rewrite each case's expected.json and rendered/ texts with the current decisions and renders")
	baseDir := fl.String("base-catalog", "", "also evaluate every case with this catalog and print the decision diff from it to --catalog (for catalog PRs); the goldens are not read")
	failOnDiff := fl.Bool("fail-on-diff", false, "with --base-catalog: exit 1 when any case's decisions change")
	coverage := fl.Bool("coverage", false, "list the rules no case fires; exit 1 when a rule some RuleSet selects never fires")
	format := fl.String("format", "text", "output format: text or json")
	fl.Usage = func() {
		rulesUsage(stderr)
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitValid
		}
		return exitUsage
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "factory rules test: "+format+"\n", a...)
		return exitUsage
	}
	switch {
	case fl.NArg() > 0:
		return usageErr("unexpected arguments %v", fl.Args())
	case *format != "text" && *format != "json":
		return usageErr("--format must be text or json, not %q", *format)
	case *update && *baseDir != "":
		return usageErr("--update and --base-catalog don't go together: --base-catalog doesn't read the goldens")
	case *failOnDiff && *baseDir == "":
		return usageErr("--fail-on-diff needs --base-catalog")
	}
	defaultFixtures := *fixturesDir == ""
	if defaultFixtures {
		*fixturesDir = filepath.Join(*catalogDir, "testdata")
	}

	head, code := loadRulesCatalog(*catalogDir, stderr)
	if head == nil {
		return code
	}
	var base *catalog.Catalog
	if *baseDir != "" {
		if base, code = loadRulesCatalog(*baseDir, stderr); base == nil {
			return code
		}
	}

	var cases []rules.Fixture
	switch info, err := os.Stat(*fixturesDir); {
	case err == nil && info.IsDir():
		if cases, err = rules.LoadFixtures(os.DirFS(*fixturesDir)); err != nil {
			return usageErr("%s: %v", *fixturesDir, err)
		}
	case defaultFixtures && errors.Is(err, fs.ErrNotExist):
		// A catalog without fixtures yet: nothing to test.
		fmt.Fprintf(stderr, "factory rules test: no fixtures: %s does not exist\n", *fixturesDir)
	default:
		return usageErr("%s is not a directory", *fixturesDir)
	}

	rep := rulesTestReport{Catalog: *catalogDir, BaseCatalog: *baseDir, Fixtures: *fixturesDir, Total: len(cases), Cases: []caseReport{}}
	fired := map[string]bool{}
	seen := renderSeen{taskSpecs: map[string]bool{}, templates: map[string]bool{}}
	for _, f := range cases {
		ev, err := f.Evaluate(ctx, head)
		if err != nil {
			fmt.Fprintf(stderr, "factory rules test: %v\n", err)
			return exitUsage
		}
		for _, m := range ev.Matches {
			if m.Fired() {
				fired[m.Rule] = true
			}
		}
		var cr caseReport
		if base != nil {
			bev, err := f.Evaluate(ctx, base)
			if err != nil {
				fmt.Fprintf(stderr, "factory rules test: %v\n", err)
				return exitUsage
			}
			cr = caseReport{Name: f.Name, Status: caseOK, Changes: rules.DiffGolden(rules.ToGolden(bev), rules.ToGolden(ev))}
			if len(cr.Changes) > 0 {
				cr.Status = caseChanged
			}
		} else {
			cr, err = goldenCase(f, ev, *fixturesDir, *update)
			if err == nil {
				cr.Rendered, cr.Failures, err = renderCase(ctx, head, f, ev, filepath.Join(*fixturesDir, filepath.FromSlash(f.Name)), *update, seen)
			}
			if err != nil {
				fmt.Fprintf(stderr, "factory rules test: %s: %v\n", f.Name, err)
				return exitUsage
			}
			cr.Status = caseStatus(cr)
		}
		switch cr.Status {
		case caseDiffers, caseMissing, caseChanged:
			rep.Differ++
		case caseUpdated:
			rep.Updated++
		case caseFailed:
			rep.Failed++
		}
		rep.Cases = append(rep.Cases, cr)
	}
	if *coverage {
		rep.Coverage = ruleCoverage(head, fired)
		if base == nil {
			renderCoverage(rep.Coverage, head, seen)
		}
	}

	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "factory rules test: %v\n", err)
			return exitUsage
		}
	} else {
		printRulesTest(stdout, stderr, rep)
	}

	switch {
	case rep.Failed > 0:
		return exitInvalid
	case base == nil && rep.Differ > 0:
		return exitInvalid
	case base != nil && rep.Differ > 0 && *failOnDiff:
		return exitInvalid
	case rep.Coverage != nil && len(rep.Coverage.NeverFired) > 0:
		return exitInvalid
	}
	return exitValid
}

// loadRulesCatalog loads the catalog in dir. When it doesn't load, it prints the diagnostics
// as factory validate does and returns nil with exit code 2.
func loadRulesCatalog(dir string, stderr io.Writer) (*catalog.Catalog, int) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "factory rules test: %s is not a directory\n", dir)
		return nil, exitUsage
	}
	c, errs := catalog.LoadDir(os.DirFS(dir), catalog.Options{})
	if c != nil {
		return c, exitValid
	}
	n := 0
	for _, e := range catalogDiagnostics(dir, errs) {
		fmt.Fprintln(stderr, e.Error())
		if e.Severity == catalog.SeverityError {
			n++
		}
	}
	fmt.Fprintf(stderr, "factory rules test: catalog %s is invalid: %d errors (see factory validate --dir %s)\n", dir, n, dir)
	return nil, exitUsage
}

// goldenCase compares one case's evaluation with its golden and, with update, rewrites the
// golden when it differs.
func goldenCase(f rules.Fixture, ev rules.Evaluation, fixturesDir string, update bool) (caseReport, error) {
	cr := caseReport{Name: f.Name, Status: caseOK}
	got, err := rules.Golden(ev)
	if err != nil {
		return cr, err
	}
	if bytes.Equal(got, f.Expected) {
		return cr, nil
	}
	if f.Expected == nil {
		cr.Status, cr.Note = caseMissing, "no "+rules.FixtureExpected+" (run with --update)"
	} else {
		cr.Status = caseDiffers
		if want, err := rules.ParseGolden(f.Expected); err != nil {
			cr.Note = err.Error()
		} else if cr.Changes = rules.DiffGolden(want, rules.ToGolden(ev)); len(cr.Changes) == 0 {
			cr.Note = rules.FixtureExpected + " differs only in formatting"
		}
	}
	if !update {
		return cr, nil
	}
	file := filepath.Join(fixturesDir, filepath.FromSlash(f.Name), rules.FixtureExpected)
	if err := os.WriteFile(file, got, 0o644); err != nil { //nolint:gosec // goldens are checked-in, world-readable files
		return cr, fmt.Errorf("--update: %w", err)
	}
	if cr.Status == caseMissing {
		cr.Note = "created " + rules.FixtureExpected
	}
	cr.Status = caseUpdated
	return cr, nil
}

// caseStatus is a case's status from its decisions' (goldenCase) and its rendered goldens' and
// failures: a decision mismatch first, then a failure, then a rendered golden.
func caseStatus(cr caseReport) string {
	switch {
	case cr.Status != caseOK && cr.Status != caseUpdated:
		return cr.Status
	case len(cr.Failures) > 0:
		return caseFailed
	}
	if len(cr.Rendered) == 0 {
		return cr.Status
	}
	// Under --update every rendered golden listed was rewritten or removed; else none was.
	if s := cr.Rendered[0].Status; s == caseUpdated || s == renderRemoved {
		return caseUpdated
	}
	return caseDiffers
}

// renderCoverage adds to cov the TaskSpecs with a prompt and the Templates no case rendered.
func renderCoverage(cov *coverageReport, c *catalog.Catalog, seen renderSeen) {
	for name, ts := range c.TaskSpecs {
		if ts.Prompt != nil && ts.Prompt.Template != nil && !seen.taskSpecs[name] {
			cov.PromptsNotRendered = append(cov.PromptsNotRendered, name)
		}
	}
	for name := range c.Templates {
		if !seen.templates[name] {
			cov.TemplatesNotRendered = append(cov.TemplatesNotRendered, name)
		}
	}
	slices.Sort(cov.PromptsNotRendered)
	slices.Sort(cov.TemplatesNotRendered)
}

func ruleCoverage(c *catalog.Catalog, fired map[string]bool) *coverageReport {
	selected := map[string]bool{}
	for _, rs := range c.RuleSets {
		for _, r := range rs.Rules {
			selected[r.Name] = true
		}
	}
	cov := &coverageReport{Selected: len(selected), NeverFired: []string{}}
	for name := range selected {
		if fired[name] {
			cov.Fired++
		} else {
			cov.NeverFired = append(cov.NeverFired, name)
		}
	}
	for name := range c.Rules {
		if !selected[name] {
			cov.NotSelected = append(cov.NotSelected, name)
		}
	}
	slices.Sort(cov.NeverFired)
	slices.Sort(cov.NotSelected)
	return cov
}

// printRulesTest prints the cases that differ, each with its decision diff, on stdout, and
// the summary on stderr.
func printRulesTest(stdout, stderr io.Writer, rep rulesTestReport) {
	for _, cr := range rep.Cases {
		var head string
		switch cr.Status {
		case caseOK:
			continue
		case caseDiffers:
			head = "FAIL " + cr.Name + ": decisions differ from " + rules.FixtureExpected + " (- expected, + now)"
			if len(cr.Changes) == 0 && cr.Note == "" {
				head = "FAIL " + cr.Name + ": rendered texts differ from their goldens under " + renderedDir + "/"
			}
		case caseMissing:
			head = "FAIL " + cr.Name
		case caseUpdated:
			head = "updated " + cr.Name
		case caseChanged:
			head = "changed " + cr.Name + " (- base, + head)"
		case caseFailed:
			head = "FAIL " + cr.Name + ": does not render or its run result fails its schema"
		}
		if cr.Note != "" {
			head += ": " + cr.Note
		}
		fmt.Fprintln(stdout, head)
		for _, c := range cr.Changes {
			for _, l := range c.Lines() {
				fmt.Fprintln(stdout, "  "+l)
			}
		}
		for _, r := range cr.Rendered {
			l := "  " + r.Status + " " + r.File
			if r.Note != "" {
				l += ": " + r.Note
			}
			fmt.Fprintln(stdout, l)
		}
		for _, f := range cr.Failures {
			fmt.Fprintln(stdout, "  error: "+f)
		}
	}
	if cov := rep.Coverage; cov != nil {
		fmt.Fprintf(stdout, "coverage: %d of %d selected rules fire in some case\n", cov.Fired, cov.Selected)
		for _, r := range cov.NeverFired {
			fmt.Fprintln(stdout, "  never fired: "+r)
		}
		for _, r := range cov.NotSelected {
			fmt.Fprintln(stdout, "  not selected by any RuleSet: "+r)
		}
		for _, n := range cov.PromptsNotRendered {
			fmt.Fprintln(stdout, "  warning: no case renders the prompt of TaskSpec "+n)
		}
		for _, n := range cov.TemplatesNotRendered {
			fmt.Fprintln(stdout, "  warning: no case renders Template "+n)
		}
	}
	failed := ""
	if rep.Failed > 0 {
		failed = fmt.Sprintf(", %d failed", rep.Failed)
	}
	switch {
	case rep.BaseCatalog != "":
		fmt.Fprintf(stderr, "%s -> %s: %d of %d cases change their decisions\n", rep.BaseCatalog, rep.Catalog, rep.Differ, rep.Total)
	case rep.Updated > 0:
		fmt.Fprintf(stderr, "%s: %d cases, %d goldens updated%s\n", rep.Fixtures, rep.Total, rep.Updated, failed)
	default:
		fmt.Fprintf(stderr, "%s: %d cases, %d ok, %d differ%s\n", rep.Fixtures, rep.Total, rep.Total-rep.Differ-rep.Failed, rep.Differ, failed)
	}
}

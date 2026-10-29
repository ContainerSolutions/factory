package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the goldens under testdata/rules and testdata/state")

// pocCases are the rule engine's cases for the PoC catalog.
const pocCases = "../../internal/rules/testdata/cases/poc"

// changedPoC is the PoC catalog with review-human-prs changed on purpose: drafts are
// reviewed too, and without the versioning check.
func changedPoC(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(pocDir)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "rules", "review-human", "review-human-prs.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), " && !item.pr.draft", "", 1)
	changed = strings.Replace(changed, "checks: [style, versioning, build]", "checks: [style, build]", 1)
	if changed == string(data) {
		t.Fatal("review-human-prs.yaml: nothing replaced")
	}
	if err := os.WriteFile(file, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// copyCases copies the PoC cases into a temp dir.
func copyCases(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(pocCases)); err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkGolden compares got with testdata/rules/<name>.golden, or rewrites it with -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "rules", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (rerun with -update)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rerun with -update and review the diff):\n%s", file, got)
	}
}

func TestRulesTestPoC(t *testing.T) {
	code, out, errOut := runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", pocCases)
	if code != 0 || out != "" || !strings.Contains(errOut, ": 30 cases, 30 ok, 0 differ") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

// TestRulesTestChangedRule: a deliberately changed rule produces a readable decision diff
// and exit 1.
func TestRulesTestChangedRule(t *testing.T) {
	changed := changedPoC(t)
	code, out, errOut := runFactory(t, "rules", "test", "--catalog", changed, "--fixtures", pocCases)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstderr:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, ": 30 cases, 26 ok, 4 differ") {
		t.Errorf("summary: %q", errOut)
	}
	checkGolden(t, "changed-rule", out)

	// The same as JSON.
	code, out, _ = runFactory(t, "rules", "test", "--catalog", changed, "--fixtures", pocCases, "--format", "json")
	if code != 1 {
		t.Fatalf("json: exit %d", code)
	}
	var rep rulesTestReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Total != 30 || rep.Differ != 4 || len(rep.Cases) != 30 {
		t.Fatalf("report: total %d, differ %d, %d cases", rep.Total, rep.Differ, len(rep.Cases))
	}
	for _, cr := range rep.Cases {
		if cr.Name != "review-human-prs-draft" {
			continue
		}
		if cr.Status != caseDiffers || len(cr.Changes) != 3 || cr.Changes[0].From != "not-matched" || cr.Changes[0].To != "matched" {
			t.Errorf("review-human-prs-draft: %+v", cr)
		}
	}
}

// TestRulesTestBaseCatalog: --base-catalog prints the same change, between the catalogs.
func TestRulesTestBaseCatalog(t *testing.T) {
	changed := changedPoC(t)
	args := []string{"rules", "test", "--catalog", changed, "--base-catalog", pocDir, "--fixtures", pocCases}
	code, out, errOut := runFactory(t, args...)
	if code != 0 {
		t.Fatalf("exit %d, want 0 without --fail-on-diff\nstderr:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, pocDir+" -> "+changed+": 4 of 30 cases change their decisions") {
		t.Errorf("summary: %q", errOut)
	}
	checkGolden(t, "base-catalog", out)

	// The change lines are those of the golden comparison (which also checks the rendered
	// goldens).
	_, golden, _ := runFactory(t, "rules", "test", "--catalog", changed, "--fixtures", pocCases)
	strip := func(s string) string {
		var lines []string
		for l := range strings.SplitSeq(s, "\n") {
			if strings.HasPrefix(l, "  ") && !strings.Contains(l, " "+renderedDir+"/") {
				lines = append(lines, l)
			}
		}
		return strings.Join(lines, "\n")
	}
	if strip(out) != strip(golden) {
		t.Errorf("--base-catalog changes:\n%s\nwant those of the golden diff:\n%s", out, golden)
	}

	if code, _, _ := runFactory(t, append(args, "--fail-on-diff")...); code != 1 {
		t.Errorf("--fail-on-diff: exit %d, want 1", code)
	}
	// No change between a catalog and itself.
	code, out, _ = runFactory(t, "rules", "test", "--catalog", pocDir, "--base-catalog", pocDir, "--fixtures", pocCases, "--fail-on-diff")
	if code != 0 || out != "" {
		t.Errorf("same catalog: exit %d, %q", code, out)
	}
}

// TestRulesTestUpdate: --update rewrites the goldens that differ or are missing, and a
// second run passes with the same files.
func TestRulesTestUpdate(t *testing.T) {
	cases := copyCases(t)
	missing := filepath.Join(cases, "train-mock", "expected.json")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	reformatted := filepath.Join(cases, "out-of-scope", "expected.json")
	orig, err := os.ReadFile(reformatted)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(orig, &v); err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reformatted, compact, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", cases)
	if code != 1 || !strings.Contains(out, "FAIL train-mock: no expected.json (run with --update)") ||
		!strings.Contains(out, "FAIL out-of-scope: decisions differ from expected.json (- expected, + now): expected.json differs only in formatting") {
		t.Fatalf("before --update: exit %d\n%s", code, out)
	}

	code, out, errOut := runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", cases, "--update")
	if code != 0 || !strings.Contains(out, "updated train-mock: created expected.json") || !strings.Contains(out, "updated out-of-scope") ||
		!strings.Contains(errOut, ": 30 cases, 2 goldens updated") {
		t.Fatalf("--update: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, name := range []string{"train-mock", "out-of-scope"} {
		got, err := os.ReadFile(filepath.Join(cases, name, "expected.json"))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(pocCases, name, "expected.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: --update wrote\n%s\nwant\n%s", name, got, want)
		}
	}

	if code, out, _ := runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", cases); code != 0 || out != "" {
		t.Errorf("after --update: exit %d\n%s", code, out)
	}
	// Nothing to update: nothing written.
	if code, out, errOut := runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", cases, "--update"); code != 0 || out != "" || !strings.Contains(errOut, "30 ok") {
		t.Errorf("second --update: exit %d\n%s%s", code, out, errOut)
	}
}

func TestRulesTestCoverage(t *testing.T) {
	code, out, _ := runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", pocCases, "--coverage")
	want := "coverage: 14 of 14 selected rules fire in some case\n" +
		"  not selected by any RuleSet: merge-release-docs\n" +
		"  not selected by any RuleSet: merge-whole-stack\n" +
		"  warning: no case renders the prompt of TaskSpec merge\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}

	// Only the train cases: most rules never fire.
	cases := t.TempDir()
	for _, name := range []string{"train-mock", "train-prod"} {
		if err := os.CopyFS(filepath.Join(cases, name), os.DirFS(filepath.Join(pocCases, name))); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ = runFactory(t, "rules", "test", "--catalog", pocDir, "--fixtures", cases, "--coverage")
	if code != 1 || !strings.Contains(out, "coverage: 1 of 14 selected rules fire in some case\n") || !strings.Contains(out, "  never fired: review-human-prs\n") {
		t.Errorf("partial coverage: exit %d\n%s", code, out)
	}
}

func TestRulesTestErrors(t *testing.T) {
	broken := brokenPoC(t)
	noCatalogFixtures := t.TempDir()
	if err := os.CopyFS(noCatalogFixtures, os.DirFS(pocDir)); err != nil {
		t.Fatal(err)
	}
	badCase := copyCases(t)
	if err := os.WriteFile(filepath.Join(badCase, "train-mock", "event.json"), []byte(`{"kind": "Event", "nope": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string // substring
		stderr string // substring
	}{
		{name: "no subcommand", args: []string{"rules"}, code: 2, stderr: "usage: factory rules test"},
		{name: "unknown subcommand", args: []string{"rules", "run"}, code: 2, stderr: `factory rules: unknown command "run"`},
		{name: "help", args: []string{"rules", "help"}, code: 0, stdout: "usage: factory rules test"},
		{name: "flag help", args: []string{"rules", "test", "-h"}, code: 0, stderr: "-base-catalog"},
		{name: "unknown flag", args: []string{"rules", "test", "--nope"}, code: 2, stderr: "flag provided but not defined: -nope"},
		{name: "arguments", args: []string{"rules", "test", "extra"}, code: 2, stderr: "unexpected arguments [extra]"},
		{name: "format", args: []string{"rules", "test", "--format", "yaml"}, code: 2, stderr: `--format must be text or json, not "yaml"`},
		{name: "update with base", args: []string{"rules", "test", "--update", "--base-catalog", pocDir}, code: 2, stderr: "--update and --base-catalog don't go together"},
		{name: "fail-on-diff without base", args: []string{"rules", "test", "--fail-on-diff"}, code: 2, stderr: "--fail-on-diff needs --base-catalog"},
		{name: "no catalog", args: []string{"rules", "test", "--catalog", "nope"}, code: 2, stderr: "factory rules test: nope is not a directory"},
		{name: "no base catalog", args: []string{"rules", "test", "--catalog", pocDir, "--base-catalog", "nope", "--fixtures", pocCases}, code: 2, stderr: "factory rules test: nope is not a directory"},
		{name: "invalid catalog", args: []string{"rules", "test", "--catalog", broken, "--fixtures", pocCases}, code: 2,
			stderr: filepath.ToSlash(filepath.Join(broken, "queues", "poc.yaml")) + ":"},
		{name: "invalid base catalog", args: []string{"rules", "test", "--catalog", pocDir, "--base-catalog", broken, "--fixtures", pocCases}, code: 2,
			stderr: "factory rules test: catalog " + broken + " is invalid: 1 errors"},
		{name: "no fixtures dir", args: []string{"rules", "test", "--catalog", pocDir, "--fixtures", "nope"}, code: 2, stderr: "factory rules test: nope is not a directory"},
		{name: "default fixtures missing", args: []string{"rules", "test", "--catalog", noCatalogFixtures}, code: 0,
			stderr: "no fixtures: " + filepath.Join(noCatalogFixtures, "testdata") + " does not exist"},
		{name: "bad fixture", args: []string{"rules", "test", "--catalog", pocDir, "--fixtures", badCase}, code: 2, stderr: `unknown field "nope"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runFactory(t, tc.args...)
			if code != tc.code || !strings.Contains(out, tc.stdout) || !strings.Contains(errOut, tc.stderr) {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, out, errOut)
			}
		})
	}
}

// renderCatalog is a small catalog whose two cases render a prompt (opened) and a Template
// (planned), with a TaskSpec and a Template no case renders.
const renderCatalog = "testdata/rules/render"

// copyRenderCatalog copies renderCatalog into a temp dir and applies edit to it.
func copyRenderCatalog(t *testing.T, edit func(dir string) error) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(renderCatalog)); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		if err := edit(dir); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// replaceIn replaces old with new in the file of a catalog copy.
func replaceIn(file, old, new string) func(dir string) error {
	return func(dir string) error {
		p := filepath.Join(dir, file)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := strings.Replace(string(b), old, new, 1)
		if s == string(b) {
			return errors.New(file + ": nothing replaced")
		}
		return os.WriteFile(p, []byte(s), 0o600)
	}
}

func TestRulesTestRender(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(dir string) error
		code   int
		stdout []string // substrings
	}{
		{name: "renders", code: 0},
		{name: "prompt with a bad field", edit: replaceIn("prompts/plan.md.tmpl", ".inputs.depth", ".inputs.dpth"), code: 1, stdout: []string{
			"FAIL opened: does not render or its run result fails its schema\n",
			`  error: TaskSpec plan: the text contains "<no value>": a missing field or a value printed raw` + "\n",
		}},
		{name: "template that fails to execute", edit: replaceIn("templates/planned.yaml", "len .run.result.plan", "len .run.result.plan.steps"), code: 1, stdout: []string{
			"FAIL planned: does not render or its run result fails its schema\n",
			"  error: Template planned: notify: template planned: template: planned:1:",
		}},
		{name: "changed prompt", edit: replaceIn("prompts/plan.md.tmpl", "Plan `", "Plan issue `"), code: 1, stdout: []string{
			"FAIL opened: rendered texts differ from their goldens under rendered/\n",
			"  differs rendered/1-prompt-plan.md: line 3: - \"Plan `acme/docs#7`",
			", + \"Plan issue `acme/docs#7`",
		}},
		{name: "result that fails its schema", edit: replaceIn("testdata/planned/event.json", `"acme/docs#9"`, `9`), code: 1, stdout: []string{
			"FAIL planned: does not render or its run result fails its schema\n",
			"  error: result of the plan run: schema schemas/results/plan.json: ",
			"at '/data/plan/1/issue': got number, want string",
		}},
		{name: "missing golden", edit: func(dir string) error {
			return os.Remove(filepath.Join(dir, "testdata", "planned", "rendered", "0-template-planned.md"))
		}, code: 1, stdout: []string{"  missing rendered/0-template-planned.md: no golden (run with --update)\n"}},
		{name: "stale golden", edit: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "testdata", "opened", "rendered", "9-prompt-old.md"), []byte("old"), 0o600)
		}, code: 1, stdout: []string{"  stale rendered/9-prompt-old.md: no decision renders it (run with --update)\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyRenderCatalog(t, tc.edit)
			code, out, errOut := runFactory(t, "rules", "test", "--catalog", dir)
			if code != tc.code {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, out, errOut)
			}
			for _, s := range tc.stdout {
				if !strings.Contains(out, s) {
					t.Errorf("stdout doesn't contain %q:\n%s", s, out)
				}
			}
			if tc.stdout == nil && out != "" {
				t.Errorf("stdout:\n%s", out)
			}
		})
	}

	// A failure fails the case under --update too, and is in the JSON report.
	dir := copyRenderCatalog(t, replaceIn("prompts/plan.md.tmpl", ".inputs.depth", ".inputs.dpth"))
	if code, out, _ := runFactory(t, "rules", "test", "--catalog", dir, "--update"); code != 1 || !strings.Contains(out, "error: TaskSpec plan:") {
		t.Errorf("--update: exit %d\n%s", code, out)
	}
	_, out, _ := runFactory(t, "rules", "test", "--catalog", dir, "--format", "json")
	var rep rulesTestReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 || rep.Cases[0].Status != caseFailed || len(rep.Cases[0].Failures) != 1 {
		t.Errorf("report: %+v", rep)
	}
}

// TestRulesTestRenderUpdate: --update writes the rendered goldens and removes stale ones, and
// a second run passes with the same files.
func TestRulesTestRenderUpdate(t *testing.T) {
	dir := copyRenderCatalog(t, func(dir string) error {
		if err := os.RemoveAll(filepath.Join(dir, "testdata", "opened", "rendered")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "testdata", "planned", "rendered", "9-prompt-old.md"), []byte("old"), 0o600)
	})
	code, out, errOut := runFactory(t, "rules", "test", "--catalog", dir, "--update")
	want := "updated opened\n  updated rendered/1-prompt-plan.md: created\nupdated planned\n  removed rendered/9-prompt-old.md\n"
	if code != 0 || out != want || !strings.Contains(errOut, ": 2 cases, 2 goldens updated") {
		t.Fatalf("--update: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, f := range []string{"opened/rendered/1-prompt-plan.md", "planned/rendered/0-template-planned.md"} {
		got, err := os.ReadFile(filepath.Join(dir, "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(renderCatalog, "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: --update wrote\n%s\nwant\n%s", f, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "testdata", "planned", "rendered", "9-prompt-old.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale golden: %v", err)
	}
	if code, out, _ := runFactory(t, "rules", "test", "--catalog", dir); code != 0 || out != "" {
		t.Errorf("after --update: exit %d\n%s", code, out)
	}
}

func TestRulesTestRenderCoverage(t *testing.T) {
	code, out, _ := runFactory(t, "rules", "test", "--catalog", renderCatalog, "--coverage")
	want := "coverage: 2 of 2 selected rules fire in some case\n" +
		"  warning: no case renders the prompt of TaskSpec unused\n" +
		"  warning: no case renders Template unused\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

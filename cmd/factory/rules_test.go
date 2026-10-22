package main

import (
	"bytes"
	"encoding/json"
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

	// The change lines are those of the golden comparison.
	_, golden, _ := runFactory(t, "rules", "test", "--catalog", changed, "--fixtures", pocCases)
	strip := func(s string) string {
		var lines []string
		for l := range strings.SplitSeq(s, "\n") {
			if strings.HasPrefix(l, "  ") {
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
		"  not selected by any RuleSet: merge-whole-stack\n"
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

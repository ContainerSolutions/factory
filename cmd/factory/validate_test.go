package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/catalog"
)

const pocDir = "../../internal/catalog/testdata/poc"

func runFactory(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// brokenPoC is the PoC catalog with its queue pointing at a pool that doesn't exist.
func brokenPoC(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(pocDir)); err != nil {
		t.Fatal(err)
	}
	q := filepath.Join(dir, "queues", "poc.yaml")
	data, err := os.ReadFile(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(q, bytes.Replace(data, []byte("pool: local"), []byte("pool: nope"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{nil, {"version"}} {
		code, out, _ := runFactory(t, args...)
		if code != 0 || out != "factory dev\n" {
			t.Errorf("factory %v: exit %d, output %q", args, code, out)
		}
	}
	if code, _, errOut := runFactory(t, "frobnicate"); code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: exit %d, %q", code, errOut)
	}
	if code, out, _ := runFactory(t, "help"); code != 0 || !strings.Contains(out, "validate") {
		t.Errorf("help: exit %d, %q", code, out)
	}
}

func TestValidate(t *testing.T) {
	broken := brokenPoC(t)
	codexOnly := writeFile(t, "caps.json", `[{"harness": "codex", "features": ["structured-output", "mcp", "skills-dir"]}]`)
	tests := []struct {
		name     string
		args     []string
		code     int
		stdout   []string // substrings
		stderr   []string
		noStdout bool
	}{
		{name: "PoC is valid", args: []string{"--dir", pocDir}, code: 0,
			stdout: []string{pocDir + "/rulesets/poc-release-docs.yaml:6: CAT1200 warning: spec.rules: rule docs-on-release-train"},
			stderr: []string{pocDir + ": valid (dir:", "0 errors, 9 warnings"}},
		{name: "strict fails on warnings", args: []string{"--dir", pocDir, "--strict"}, code: 1,
			stdout: []string{"CAT1200 spec.rules"}, stderr: []string{"invalid", "9 errors, 0 warnings"}},
		{name: "errors exit 1", args: []string{"--dir", broken}, code: 1,
			stdout: []string{broken + `/queues/poc.yaml:4: CAT204 spec.pool: Pool "nope" does not exist`}, stderr: []string{"1 errors"}},
		{name: "capabilities override", args: []string{"--dir", pocDir, "--capabilities", codexOnly}, code: 1,
			stdout: []string{`/taskspecs/docs-review.yaml:12: CAT803 spec.harness.default.name: no capability descriptor for harness "claude-code" (known harnesses: codex)`}},
		{name: "known models override", args: []string{"--dir", pocDir, "--known-models", writeFile(t, "models.json", `{"codex": ["gpt-4o"], "claude-code": ["claude-opus-5-5"]}`)}, code: 1,
			stdout: []string{"CAT801 spec.harness.default.config.model: no known codex model fits"}},
		{name: "not a directory", args: []string{"--dir", filepath.Join(broken, "nope")}, code: 2, stderr: []string{"is not a directory"}, noStdout: true},
		{name: "bad format", args: []string{"--dir", pocDir, "--format", "xml"}, code: 2, stderr: []string{"--format must be text or json"}, noStdout: true},
		{name: "bad flag", args: []string{"--frobnicate"}, code: 2, stderr: []string{"flag provided but not defined"}, noStdout: true},
		{name: "extra arguments", args: []string{"--dir", pocDir, "more"}, code: 2, stderr: []string{"unexpected arguments"}, noStdout: true},
		{name: "unreadable capabilities", args: []string{"--dir", pocDir, "--capabilities", filepath.Join(broken, "none.json")}, code: 2, stderr: []string{"--capabilities"}, noStdout: true},
		{name: "invalid known models", args: []string{"--dir", pocDir, "--known-models", writeFile(t, "bad.json", `["codex"]`)}, code: 2, stderr: []string{"--known-models"}, noStdout: true},
		{name: "unfetchable git ref", args: []string{"--git", "file://" + filepath.Join(broken, "no-repo"), "--ref", "main"}, code: 2, stderr: []string{"git fetch"}, noStdout: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runFactory(t, append([]string{"validate"}, tt.args...)...)
			if code != tt.code {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, out, errOut)
			}
			for _, s := range tt.stdout {
				if !strings.Contains(out, s) {
					t.Errorf("stdout lacks %q:\n%s", s, out)
				}
			}
			for _, s := range tt.stderr {
				if !strings.Contains(errOut, s) {
					t.Errorf("stderr lacks %q:\n%s", s, errOut)
				}
			}
			if tt.noStdout && out != "" {
				t.Errorf("unexpected stdout:\n%s", out)
			}
		})
	}
}

func TestValidateJSON(t *testing.T) {
	code, out, _ := runFactory(t, "validate", "--dir", brokenPoC(t), "--format", "json")
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	var rep struct {
		Valid       bool            `json:"valid"`
		Errors      int             `json:"errors"`
		Warnings    int             `json:"warnings"`
		Diagnostics []catalog.Error `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if rep.Valid || rep.Errors != 1 || rep.Warnings != 9 {
		t.Errorf("report = %+v", rep)
	}
	var found bool
	for _, d := range rep.Diagnostics {
		if d.Code == catalog.CodeUnknownPool && d.Line == 4 && strings.HasSuffix(d.File, "/queues/poc.yaml") && d.Object.Name == "poc" && d.Severity == catalog.SeverityError {
			found = true
		}
	}
	if !found {
		t.Errorf("no CAT204 diagnostic for queues/poc.yaml:4 in %+v", rep.Diagnostics)
	}

	code, out, _ = runFactory(t, "validate", "--dir", pocDir, "--format", "json")
	if code != 0 || !strings.Contains(out, `"sha": "dir:`) || !strings.Contains(out, `"valid": true`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

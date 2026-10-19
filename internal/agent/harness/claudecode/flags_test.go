package claudecode

import (
	"slices"
	"testing"
)

func TestRunArgs(t *testing.T) {
	base := []string{"--print", "--output-format", "stream-json", "--verbose"}
	tests := []struct {
		name string
		o    runOptions
		want []string
	}{
		{"read-write, everything", runOptions{
			Model: "claude-opus-5-5", Preamble: "Be headless.", PluginDirs: []string{"/w/skills/a", "/w/skills/b"},
			Settings: "/w/s.json", MCPConfig: "/w/mcp.json", Schema: `{"type":"object"}`, MaxTurns: 30, MaxBudgetUSD: 2.5,
			AddDirs: []string{"/w/out"},
		}, append(slices.Clone(base), "--model", "claude-opus-5-5", "--append-system-prompt", "Be headless.",
			"--plugin-dir", "/w/skills/a", "--plugin-dir", "/w/skills/b", "--settings", "/w/s.json", "--setting-sources", "user",
			"--mcp-config", "/w/mcp.json", "--strict-mcp-config", "--json-schema", `{"type":"object"}`, "--max-turns", "30",
			"--max-budget-usd", "2.5", "--permission-mode", "bypassPermissions", "--add-dir", "/w/out")},
		{"read-only", runOptions{Model: "claude-sonnet-5-5", Settings: "/w/s.json", MCPConfig: "/w/mcp.json", ReadOnly: true},
			append(slices.Clone(base), "--model", "claude-sonnet-5-5", "--settings", "/w/s.json", "--setting-sources", "user",
				"--mcp-config", "/w/mcp.json", "--strict-mcp-config", "--permission-mode", "bypassPermissions",
				"--disallowedTools", "Write,Edit,MultiEdit,NotebookEdit")},
		{"no model: claude's default", runOptions{},
			append(slices.Clone(base), "--setting-sources", "user", "--permission-mode", "bypassPermissions")},
		{"resume", runOptions{Model: "claude-sonnet-5-5", Resume: "sess-1", MaxTurns: repairMaxTurns},
			append(slices.Clone(base), "--model", "claude-sonnet-5-5", "--resume", "sess-1", "--setting-sources", "user",
				"--max-turns", "5", "--permission-mode", "bypassPermissions")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runArgs(tt.o); !slices.Equal(got, tt.want) {
				t.Errorf("runArgs =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestNativeEffort(t *testing.T) {
	for effort, want := range map[string]string{"minimal": "low", "low": "low", "medium": "medium", "high": "high", "max": "high", "turbo": "turbo"} {
		if got := nativeEffort(effort); got != want {
			t.Errorf("nativeEffort(%q) = %q, want %q", effort, got, want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	c := New(Options{}).Capabilities()
	if c.Harness != "claude-code" || c.EffortMapping["max"] != "high" || c.EffortMapping["minimal"] != "low" || !slices.Contains(c.Features, "pre-tool-use-hook") {
		t.Errorf("capabilities %+v", c)
	}
	c.EffortMapping["max"] = "changed"
	if effortMapping["max"] != "high" {
		t.Error("Capabilities shares the mapping")
	}
}

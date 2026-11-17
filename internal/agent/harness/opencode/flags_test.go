package opencode

import (
	"maps"
	"slices"
	"testing"
)

func TestNativeEffort(t *testing.T) {
	t.Parallel()
	for effort, want := range map[string]string{
		"minimal": "low", "low": "low", "medium": "medium", "high": "high", "max": "high", "turbo": "turbo",
	} {
		if got := nativeEffort(effort); got != want {
			t.Errorf("nativeEffort(%q) = %q, want %q", effort, got, want)
		}
	}
}

// TestCapabilities: low, medium and high, and the features the LLD lists.
func TestCapabilities(t *testing.T) {
	t.Parallel()
	c := adapter(t, "").Capabilities()
	if want := map[string]string{"low": "low", "medium": "medium", "high": "high"}; c.Harness != "opencode" || !maps.Equal(c.EffortMapping, want) {
		t.Errorf("capabilities %+v", c)
	}
	if want := []string{"structured-output", "mcp", "skills-dir"}; !slices.Equal(c.Features, want) {
		t.Errorf("features %q, want %q", c.Features, want)
	}
}

// TestRunFlags pins the run command line read from v1.19.6's run.ts, which Run and Repair
// build from these constants: written out here, so a changed constant fails.
func TestRunFlags(t *testing.T) {
	t.Parallel()
	got := []string{cmdRun, flagFormat, formatJSON, flagModel, providerID + "/m", flagSession, "ses_1", flagFile, "prompt.md"}
	want := []string{"run", "--format", "json", "--model", "factory/m", "--session", "ses_1", "--file", "prompt.md"}
	if !slices.Equal(got, want) || !promptOnStdin {
		t.Errorf("run's command line %q (prompt on stdin %v), want %q", got, promptOnStdin, want)
	}
}

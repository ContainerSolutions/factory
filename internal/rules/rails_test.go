package rules

import (
	"slices"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// TestVerdictsInContract checks that the engine's verdicts are exactly the stored rail
// verdicts of the contract (v1alpha1.RailVerdictValues), so the two can't drift.
func TestVerdictsInContract(t *testing.T) {
	ours := []v1alpha1.RailVerdictValue{VerdictAllowed, VerdictSuppressed, VerdictDeferred, VerdictUnsupported}
	want := v1alpha1.RailVerdictValues()
	for _, v := range ours {
		if !slices.Contains(want, v) {
			t.Errorf("verdict %q is not in v1alpha1.RailVerdictValues %v", v, want)
		}
	}
	for _, v := range want {
		if !slices.Contains(ours, v) {
			t.Errorf("v1alpha1 verdict %q has no rules.Verdict constant", v)
		}
	}
	for _, v := range []v1alpha1.RailVerdict{Allowed(), Suppressed(RailRunCap, "r"), Deferred(RailBudget, "r")} {
		if !slices.Contains(want, v.Verdict) {
			t.Errorf("constructor verdict %q is not in v1alpha1.RailVerdictValues", v.Verdict)
		}
	}
}

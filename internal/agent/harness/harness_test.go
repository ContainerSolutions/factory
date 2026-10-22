package harness

import "testing"

func TestIsolationString(t *testing.T) {
	for i, want := range map[Isolation]string{IsolationSandbox: "sandbox", IsolationVM: "vm", 7: "Isolation(7)"} {
		if got := i.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(i), got, want)
		}
	}
}

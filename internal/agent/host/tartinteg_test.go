//go:build tartinteg && darwin

package host

import (
	"context"
	"slices"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/config"
)

// TestCheckThisMac runs host check's steps on the real Mac (go test -tags tartinteg -v): each
// returns a status without error, the VM steps among them. It never runs Init, sudo, or a
// tart command other than list. Real VM builds are run by hand (host init --only basevm,warmbase).
func TestCheckThisMac(t *testing.T) {
	e := &Env{Workspace: config.Workspace{}.WithDefaults("darwin")}
	if err := e.Defaults(); err != nil {
		t.Fatal(err)
	}
	steps, err := Steps(e)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name())
	}
	if !slices.Contains(names, "basevm") || !slices.Contains(names, "warmbase") {
		t.Errorf("steps %v lack basevm and warmbase", names)
	}
	for _, s := range steps {
		st, err := s.Check(context.Background())
		if err != nil || st.State == "" {
			t.Errorf("%s: %+v %v", s.Name(), st, err)
		}
		t.Logf("%-9s %-8s %s", s.Name(), st.State, st.Detail)
	}
}

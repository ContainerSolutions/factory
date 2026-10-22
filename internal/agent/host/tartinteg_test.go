//go:build tartinteg && darwin

package host

import (
	"context"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// TestCheckThisMac runs host check's steps on the real Mac (go test -tags tartinteg -v): each
// returns a status without error. It never runs Init or sudo.
func TestCheckThisMac(t *testing.T) {
	e := &Env{Workspace: config.Workspace{}.WithDefaults("darwin")}
	if err := e.Defaults(); err != nil {
		t.Fatal(err)
	}
	steps, err := Steps(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		st, err := s.Check(context.Background())
		if err != nil || st.State == "" {
			t.Errorf("%s: %+v %v", s.Name(), st, err)
		}
		t.Logf("%-9s %-8s %s", s.Name(), st.State, st.Detail)
	}
}

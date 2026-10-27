package sudocancel_test

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/sudocancel"
	"github.com/compliance-framework/factory/internal/agent/sudocancel/sudocanceltest"
)

func TestApply(t *testing.T) {
	const grace = 2 * time.Second
	sudocanceltest.Check(t, grace, func(ctx context.Context, prog string, args ...string) error {
		cmd := exec.CommandContext(ctx, prog, args...)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		sudocancel.Apply(cmd, grace)
		return cmd.Run()
	})
}

func TestIsSudo(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{"/usr/bin/sudo", []string{"-n", "true"}, true},
		{"/usr/bin/sudo", []string{"/bin/sh", "root.sh"}, true},
		{"/tmp/x/sudo", nil, true},
		{"/usr/bin/sudoedit", nil, false},
		{"/usr/bin/tart", []string{"-n"}, false},
	} {
		if got := sudocancel.IsSudo(exec.Command(tt.name, tt.args...)); got != tt.want {
			t.Errorf("IsSudo(%s %v) = %v, want %v", tt.name, tt.args, got, tt.want)
		}
	}
}

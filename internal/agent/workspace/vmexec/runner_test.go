package vmexec

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/sudocancel/sudocanceltest"
)

// TestExecRunnerCancel: when ctx ends, a sudo call (the kata helper's verbs) gets SIGTERM, which
// sudo relays to the helper, and SIGKILL only after Grace; anything else (tart) is killed.
func TestExecRunnerCancel(t *testing.T) {
	r := ExecRunner{Grace: 2 * time.Second}
	sudocanceltest.Check(t, r.Grace, func(ctx context.Context, prog string, args ...string) error {
		return r.Run(ctx, Cmd{Name: prog, Args: args, Stdout: io.Discard, Stderr: io.Discard})
	})
}

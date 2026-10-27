package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/harness/fake"
	"github.com/ContainerSolutions/factory/internal/agent/harness/harnesstest"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// The VM's out/environment.log is collected and uploaded re-redacted; a run without one
// uploads none.
func TestRunnerEnvironmentLog(t *testing.T) {
	t.Parallel()
	o := runbundle.Outcome{Status: runner.StatusDone, Result: doneResult()}
	shape := "ghs_" + strings.Repeat("A", 36)
	finish := finishWith(o)
	r := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, func(ctx context.Context, r *rig, out string, enc *runbundle.Encoder) error {
		log := "# environment.up\ncloning with " + harnesstest.GitToken + "\nexport TOKEN=" + shape + "\nready\n"
		if err := os.WriteFile(filepath.Join(out, runbundle.EnvironmentLog), []byte(log), 0o600); err != nil {
			t.Fatal(err)
		}
		return finish(ctx, r, out, enc)
	})
	if out := r.run(context.Background()); out.Status != runner.StatusDone {
		t.Fatalf("outcome %+v", out)
	}
	logs := r.e.EnvironmentLogs()
	if len(logs) != 1 {
		t.Fatalf("environment log uploads = %d, want 1", len(logs))
	}
	got := logs[0]
	if strings.Contains(got, harnesstest.GitToken) || strings.Contains(got, shape) ||
		!strings.HasPrefix(got, "# environment.up\ncloning with [REDACTED]\n") || !strings.HasSuffix(got, "ready\n") {
		t.Errorf("environment log %q: want it re-redacted", got)
	}

	plain := newRig(t, v1alpha1.HarnessResolved{Name: fake.Name, Model: "m"}, finishWith(o))
	if out := plain.run(context.Background()); out.Status != runner.StatusDone {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(plain.e.EnvironmentLogs()); n != 0 {
		t.Errorf("environment log uploads without a log = %d, want 0", n)
	}
}

// A link or a special file named environment.log is not read.
func TestReadEnvironmentLog(t *testing.T) {
	dir := t.TempDir()
	if got := readEnvironmentLog(dir); got != nil {
		t.Errorf("no file: %q", got)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("host file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, runbundle.EnvironmentLog)); err != nil {
		t.Fatal(err)
	}
	if got := readEnvironmentLog(dir); got != nil {
		t.Errorf("a symlink was read: %q", got)
	}
}

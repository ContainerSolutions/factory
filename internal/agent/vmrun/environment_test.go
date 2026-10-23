package vmrun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// waitFile waits until p exists.
func waitFile(t *testing.T, p string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(p); err == nil {
			return
		}
	}
	t.Errorf("%s never appeared", p)
}

// A task's environment in the VM workspace: envrun runs its scripts in the workspace, its
// exposes and versions reach the harness, its output is out/environment.log, and its down runs
// after the outcome, bounded by the signals.
func TestRunEnvironment(t *testing.T) {
	t.Parallel()
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(ready.Close)
	for _, tt := range []struct {
		name   string
		up     string
		down   string
		script func(r *vmRig) fake.Script
		status string
		reason string
		steps  []string // the step events from environment.setup on, in order
		downed bool     // down.out was written
		check  func(t *testing.T, r *vmRig)
	}{
		{name: "up", up: "echo \"up for $FACTORY_TASK_ID at $FACTORY_VERSION_UI\"\n", down: "echo down > down.out\n",
			script: func(r *vmRig) fake.Script {
				return fake.Script{Turn: fake.Turn{Result: []byte(goodResult), Do: func(_ context.Context, ws harness.Workspace, _ string) error {
					for _, kv := range []string{"FACTORY_STACK_URL=http://localhost:8000", "FACTORY_VERSION_UI=v1.0.0"} {
						if !slices.Contains(ws.Env, kv) {
							r.t.Errorf("the harness's environment has no %s", kv)
						}
					}
					return nil
				}}}
			}, status: runner.StatusDone, downed: true,
			steps: []string{"environment.setup:ok", "environment.up:ok", "environment.ready:ok", "environment:ok", "skills:ok", "schema:ok", "prompt:ok", "env:ok", "prepare:ok", "harness:ok", "finish:ok", "environment.down:ok"},
			check: func(t *testing.T, r *vmRig) {
				b, err := os.ReadFile(filepath.Join(r.work, runbundle.OutDir, runbundle.EnvironmentLog))
				if err != nil || !strings.Contains(string(b), "up for "+vmTaskID+" at v1.0.0\n") {
					t.Errorf("environment.log: %q %v", b, err)
				}
				if _, err := os.Stat(filepath.Join(r.state, "env", "up.sh")); err != nil {
					t.Errorf("the up script isn't in the state dir: %v", err)
				}
			}},
		{name: "up fails", up: "echo broken >&2\nexit 1\n", down: "echo down > down.out\n",
			script: func(*vmRig) fake.Script { return fake.Script{} }, status: runner.StatusFailed, reason: runner.ReasonEnvironmentError, downed: true,
			steps: []string{"environment.setup:ok", "environment.up:error", "environment:error", "finish:ok", "environment.down:ok"},
			check: func(t *testing.T, r *vmRig) {
				if o := r.outcome(); o.Result == nil || !strings.Contains(o.Result.Summary, "environment-error: environment: up: exit status 1") || len(r.adapter.Prepared()) != 0 {
					t.Errorf("result %+v, prepared %d", o.Result, len(r.adapter.Prepared()))
				}
			}},
		{name: "SIGTERM, then down", up: "true\n", down: "echo down > down.out\n",
			script: func(r *vmRig) fake.Script {
				return fake.Script{Turn: fake.Turn{Block: true, Do: func(context.Context, harness.Workspace, string) error {
					r.sigs <- syscall.SIGTERM
					return nil
				}}}
			}, status: runner.StatusFailed, reason: runner.ReasonCancelled, downed: true,
			steps: []string{"environment.setup:ok", "environment.up:ok", "environment.ready:ok", "environment:ok", "skills:ok", "schema:ok", "prompt:ok", "env:ok", "prepare:ok", "harness:error", "finish:ok", "environment.down:ok"}},
		{name: "SIGTERM twice skips down", up: "true\n", down: "echo down > down.out\n",
			script: func(r *vmRig) fake.Script {
				return fake.Script{Turn: fake.Turn{Block: true, Do: func(ctx context.Context, _ harness.Workspace, _ string) error {
					r.sigs <- syscall.SIGTERM
					<-ctx.Done()
					r.sigs <- syscall.SIGTERM
					for deadline := time.Now().Add(10 * time.Second); !strings.Contains(r.stderr.String(), "a second signal") && time.Now().Before(deadline); {
						time.Sleep(5 * time.Millisecond)
					}
					return nil
				}}}
			}, status: runner.StatusFailed, reason: runner.ReasonCancelled,
			steps: []string{"environment.setup:ok", "environment.up:ok", "environment.ready:ok", "environment:ok", "skills:ok", "schema:ok", "prompt:ok", "env:ok", "prepare:ok", "harness:error", "finish:ok"}},
		{name: "a second SIGTERM cancels a running down", up: "true\n", down: "touch down.started\nsleep 60\necho down > down.out\n",
			script: func(r *vmRig) fake.Script {
				return fake.Script{Turn: fake.Turn{Block: true, Do: func(context.Context, harness.Workspace, string) error {
					r.sigs <- syscall.SIGTERM
					go func() {
						waitFile(r.t, filepath.Join(r.work, "down.started"))
						r.sigs <- syscall.SIGTERM
					}()
					return nil
				}}}
			}, status: runner.StatusFailed, reason: runner.ReasonCancelled,
			steps: []string{"environment.setup:ok", "environment.up:ok", "environment.ready:ok", "environment:ok", "skills:ok", "schema:ok", "prompt:ok", "env:ok", "prepare:ok", "harness:error", "finish:ok", "environment.down:error"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newVMRig(t, func(job *runner.Job) {
				job.Lease.Task.Spec.Environment = &v1alpha1.Environment{
					Setup: "true\n", Up: tt.up, Down: tt.down,
					Ready:   &v1alpha1.ReadyCheck{HTTP: []string{ready.URL + "/health"}},
					Exposes: map[string]string{"FACTORY_STACK_URL": "http://localhost:8000"},
				}
				job.Lease.Task.Spec.Versions = map[string]string{"ui": "v1.0.0"}
			})
			r.adapter.Script = tt.script(r)
			start := time.Now()
			if code := r.main(); code != ExitOK {
				t.Fatalf("exit %d", code)
			}
			if d := time.Since(start); d > 30*time.Second {
				t.Errorf("the run took %s", d)
			}
			if o := r.outcome(); o.Status != tt.status || o.Reason != tt.reason {
				t.Fatalf("outcome %+v %+v", o, o.Result)
			}
			evs := r.events()
			steps := stepNames(evs)
			if i := slices.Index(steps, "environment.setup:ok"); i < 0 || !slices.Equal(steps[i:], tt.steps) {
				t.Errorf("steps %v, want %v from environment.setup on", steps, tt.steps)
			}
			// Only environment.down's step can follow the outcome.
			if i := slices.IndexFunc(evs, func(e runbundle.Event) bool { return e.Type == runbundle.EventOutcome }); i < 0 ||
				slices.ContainsFunc(evs[i+1:], func(e runbundle.Event) bool { return e.Step == nil || e.Step.Name != "environment.down" }) {
				t.Errorf("events after the outcome: %v", eventTypes(evs))
			}
			if _, err := os.Stat(filepath.Join(r.work, "down.out")); (err == nil) != tt.downed {
				t.Errorf("down.out: %v, want written %v", err, tt.downed)
			}
			if tt.check != nil {
				tt.check(t, r)
			}
			for _, dir := range []string{r.work, r.state, r.bundle} {
				scanNoCredential(t, dir)
			}
			scanOutputs(t, r)
		})
	}
}

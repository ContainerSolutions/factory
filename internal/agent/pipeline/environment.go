package pipeline

import (
	"context"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Environment brings up what a task's VM workspace declares in spec.environment (P1 §4,
// workspace-run.md §The environment step): package envrun implements it. Up runs the setup
// and up scripts and waits for ready; it returns the variables to add to the harness's
// environment (the exposes and FACTORY_VERSION_*). ws.Env is the environment the pipeline's
// own git has (commonEnv), not the harness's. Down is best effort: cleanup calls it once
// whenever Up was called, even when Up failed part-way, with no deadline of its own (the
// Environment owns its budget).
type Environment interface {
	Up(ctx context.Context, ws harness.Workspace, t v1alpha1.Task) (map[string]string, error)
	Down(ctx context.Context) error
}

// needsEnvironment reports whether the task declares an environment: spec.environment, or
// component versions resolved at compile time (spec.versions).
func needsEnvironment(t v1alpha1.Task) bool {
	return t.Spec.Environment != nil || len(t.Spec.Versions) > 0
}

// environmentUp is the environment step, for a task that declares one. On the host
// (Options.Environment nil) it doesn't run the scripts, which are written for a VM (root
// through sudo, Docker, ports): it warns, notes it in the transcript, and the run goes on.
func (r *run) environmentUp(ctx context.Context) error {
	if r.p.o.Environment == nil {
		r.log.Warn("the task declares an environment, which only a VM workspace brings up: skipping it")
		r.parts = append(r.parts, factoryLine("environment-skipped", map[string]any{
			"reason": "the environment runs only in a VM workspace",
		}))
		return nil
	}
	ws := r.ws
	ws.Env = envList(r.commonEnv())
	r.envUp = true
	env, err := r.p.o.Environment.Up(ctx, ws, r.task)
	if err != nil {
		return err
	}
	r.envVars = env
	return nil
}

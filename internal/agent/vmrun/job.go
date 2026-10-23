package vmrun

import (
	"maps"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
)

// gitUsername is the username a git credential presents; the gateway sets its own.
const gitUsername = "x-access-token"

// JobFromBundle is the job the VM runs (workspace-run.md §Sequence, step 4): the lease rebuilt
// from the manifest and task.json, its git credentials holding GitAccess's metadata and empty
// tokens, no dispatcher client (API nil), no agent token, and the manifest's dispatcher. The
// pipeline refuses, in gateway mode, a job that carries any credential.
//
// StartedAt is now + remaining − wall, where wall is the task's limits.maxWallClock (else
// pipeline.DefaultWallClock) and remaining, the supervisor's measure of what is left, is
// capped at wall (zero or less: the whole wall). The pipeline's deadline, StartedAt + wall,
// is then now + remaining, with no clock shared between the agent host and the VM. An invalid
// maxWallClock counts as the default here; the pipeline's Run refuses it.
func JobFromBundle(b *runbundle.Bundle, now time.Time, remaining time.Duration) *runner.Job {
	m := b.Manifest
	cred := apiclient.GitCredential{
		Provider: m.Git.Provider, Host: m.Git.Host, Repo: m.Git.Repo, Username: gitUsername,
		Permissions: maps.Clone(m.Git.Permissions),
	}
	var reads []apiclient.GitCredential
	for _, r := range m.Git.Reads {
		reads = append(reads, apiclient.GitCredential{
			Provider: m.Git.Provider, Host: m.Git.Host, Repo: r.Repo, Username: gitUsername,
			ReadRepos: slices.Clone(r.ReadRepos), Permissions: map[string]string{"contents": "read"},
		})
	}
	runID := m.RunID
	wall := wallClock(b)
	if remaining <= 0 || remaining > wall {
		remaining = wall
	}
	return &runner.Job{
		Lease: apiclient.Lease{
			GitCredential: cred, ReadCredentials: reads,
			Lease: apiclient.LeaseInfo{Id: m.LeaseID, Run: &runID, Task: m.TaskID},
			Task:  b.Task,
		},
		Git:        runner.NewCredentials(cred, reads...),
		Dispatcher: m.Dispatcher,
		StartedAt:  now.Add(remaining - wall),
	}
}

// wallClock is the task's limits.maxWallClock, else pipeline.DefaultWallClock.
func wallClock(b *runbundle.Bundle) time.Duration {
	if l := b.Task.Spec.Limits; l != nil && l.MaxWallClock != "" {
		if d, err := l.MaxWallClock.Parse(); err == nil && d > 0 {
			return d
		}
	}
	return pipeline.DefaultWallClock
}

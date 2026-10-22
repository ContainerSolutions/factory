package vmrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/claudecode"
	"github.com/compliance-framework/factory/internal/agent/harness/codex"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// jobBundle is a bundle of a task with maxWallClock wall ("" for none) and harness h.
func jobBundle(wall v1alpha1.Duration, h string) *runbundle.Bundle {
	task := v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: "tsk_1"}, Spec: v1alpha1.TaskRunSpec{
		Harness: &v1alpha1.TaskHarness{Resolved: &v1alpha1.HarnessResolved{Name: h, Model: "gpt-5.5", ReasoningEffort: "high"}},
	}}
	if wall != "" {
		task.Spec.Limits = &v1alpha1.Limits{MaxWallClock: wall}
	}
	return &runbundle.Bundle{Task: task, Manifest: runbundle.Manifest{
		Version: runbundle.Version, RunID: "run_1", LeaseID: "lse_1", TaskID: "tsk_1", Dispatcher: runbundle.DefaultDispatcherURL,
		Git: runbundle.GitAccess{Provider: "github-app", Host: "github.com", Repo: "acme/docs", Permissions: map[string]string{"contents": "write"},
			Reads: []runbundle.GitRead{{Repo: "acme/ro", ReadRepos: []string{"acme/ro2"}}}},
	}}
}

// JobFromBundle's deadline is now + remaining, capped at the task's wall clock; the job holds
// the git metadata with no token, no client and no agent token.
func TestJobFromBundle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		wall      v1alpha1.Duration
		remaining time.Duration
		deadline  time.Duration // from now
	}{
		{"the remaining time", "10m", 4 * time.Minute, 4 * time.Minute},
		{"no remaining time: the whole wall", "10m", 0, 10 * time.Minute},
		{"more than the wall: capped", "10m", time.Hour, 10 * time.Minute},
		{"no maxWallClock: the default", "", 0, pipeline.DefaultWallClock},
		{"an invalid maxWallClock: the default", "soon", 5 * time.Minute, 5 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := jobBundle(tt.wall, "codex")
			job := JobFromBundle(b, now, tt.remaining)
			if got := job.StartedAt.Add(wallClock(b)); !got.Equal(now.Add(tt.deadline)) {
				t.Errorf("deadline %v, want now + %v", got, tt.deadline)
			}
		})
	}

	job := JobFromBundle(jobBundle("10m", "codex"), now, 0)
	cred, reads := job.Git.Get(), job.Git.Reads()
	switch {
	case job.API != nil || job.AgentToken != nil || job.Dispatcher != runbundle.DefaultDispatcherURL:
		t.Errorf("job API %v, agent token set %v, dispatcher %q", job.API, job.AgentToken != nil, job.Dispatcher)
	case cred.Token != "" || cred.Repo != "acme/docs" || cred.Host != "github.com" || cred.Permissions["contents"] != "write":
		t.Errorf("credential %+v", cred)
	case len(reads) != 1 || reads[0].Token != "" || reads[0].Repo != "acme/ro" || reads[0].ReadRepos[0] != "acme/ro2" || reads[0].Permissions["contents"] != "read":
		t.Errorf("reads %+v", reads)
	case job.Lease.GitCredential.Token != "" || len(job.Lease.ReadCredentials) != 1 || job.Lease.ReadCredentials[0].Token != "":
		t.Errorf("lease credentials %+v", job.Lease)
	case job.Lease.Lease.Id != "lse_1" || job.Lease.Lease.Run == nil || *job.Lease.Lease.Run != "run_1" || job.TaskID() != "tsk_1":
		t.Errorf("lease %+v", job.Lease.Lease)
	}
}

// Adapters builds only the task's harness, under IsolationVM; codex gets the bundle's account.
func TestAdapters(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC))
	acct := runbundle.CodexAccount{AccountID: "acct_1", UserID: "user_1", PlanType: "pro"}

	b := jobBundle("", config.HarnessClaudeCode)
	ads, err := Adapters(b, AdapterOptions{Clock: clk})
	if _, ok := ads[config.HarnessClaudeCode].(*claudecode.Adapter); err != nil || !ok || len(ads) != 1 {
		t.Errorf("claude-code: %v %v", ads, err)
	}

	b = jobBundle("", config.HarnessCodex)
	b.Manifest.Codex = &acct
	ads, err = Adapters(b, AdapterOptions{Clock: clk, Binaries: map[string]string{config.HarnessCodex: "/opt/codex"}})
	cx, ok := ads[config.HarnessCodex].(*codex.Adapter)
	if err != nil || !ok || len(ads) != 1 {
		t.Fatalf("codex: %v %v", ads, err)
	}
	// Prepare under IsolationVM writes the placeholder login from the account (no binary runs).
	home := t.TempDir()
	if err := cx.Prepare(context.Background(), harness.Workspace{Root: home, Home: home, WorkDir: home}, b.Task); err != nil {
		t.Fatalf("codex Prepare: %v", err)
	}
	if auth, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json")); err != nil || !strings.Contains(string(auth), `"acct_1"`) {
		t.Errorf("auth.json %s %v", auth, err)
	}

	for name, mk := range map[string]func() *runbundle.Bundle{
		"codex without an account": func() *runbundle.Bundle { return jobBundle("", config.HarnessCodex) },
		"an unknown harness":       func() *runbundle.Bundle { return jobBundle("", "gemini") },
		"no resolved harness": func() *runbundle.Bundle {
			b := jobBundle("", "codex")
			b.Task.Spec.Harness = nil
			return b
		},
	} {
		if ads, err := Adapters(mk(), AdapterOptions{Clock: clk}); err == nil {
			t.Errorf("%s: %v", name, ads)
		}
	}
}

func TestHarnessBins(t *testing.T) {
	t.Parallel()
	h := harnessBins{}
	for _, s := range []string{"codex=/opt/codex", "claude-code=/opt/claude"} {
		if err := h.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.String(); got != "claude-code=/opt/claude,codex=/opt/codex" {
		t.Errorf("String %q", got)
	}
	for _, s := range []string{"codex", "codex=", "gemini=/g"} {
		if err := h.Set(s); err == nil {
			t.Errorf("Set(%q) accepted", s)
		}
	}
}

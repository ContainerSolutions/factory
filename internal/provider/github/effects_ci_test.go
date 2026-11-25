package github_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
)

// PR #60's head (seedEffects) and the workflows of its runs.
const (
	head60     = "abc123"
	ciWorkflow = 67470646
	preview    = 379691137
	lint       = 11
)

// wfRun is a workflow run on PR #60's head; id orders the runs in time too.
func wfRun(id int64, workflow int64, number int, status, conclusion string) githubtest.WorkflowRun {
	return githubtest.WorkflowRun{ID: id, WorkflowID: workflow, RunNumber: number, HeadSHA: head60, Status: status, Conclusion: conclusion,
		Created: t0.Add(time.Duration(id) * time.Minute)}
}

func ciRerun(key string) provider.EffectRequest {
	r := effectReq(v1alpha1.OpCIRerun, provider.KindPR, docs+"#60", "author", nil)
	if key != "" {
		r.IdempotencyKey = key
	}
	return r
}

// reruns are the runs re-run so far, by ID: true for rerun-failed-jobs, false for a whole
// re-run.
func reruns(s *githubtest.Server) map[int64]bool {
	out := map[int64]bool{}
	for _, r := range s.WorkflowRuns(docs) {
		if r.Attempt > 1 {
			out[r.ID] = r.FailedJobsOnly
		}
	}
	return out
}

// TestApplyCIRerun: ci.rerun re-runs the latest failed run of each workflow on the PR's head,
// skipping green and running ones and superseded runs, a cancelled run whole; nothing to
// re-run is an outcome; the same key on the same head is a replay, a new head is re-run again.
func TestApplyCIRerun(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedEffects(e.srv)
	e.srv.AddWorkflowRuns(docs,
		wfRun(1, ciWorkflow, 10, "completed", "failure"), // superseded by 2
		wfRun(2, ciWorkflow, 11, "completed", "failure"), // re-run: failed jobs
		wfRun(3, preview, 20, "completed", "cancelled"),  // re-run whole
		wfRun(4, lint, 30, "completed", "success"),       // green
		wfRun(5, 12, 40, "in_progress", ""),              // still running
		wfRun(6, 13, 50, "completed", "timed_out"),       // re-run: failed jobs
		wfRun(7, 14, 60, "completed", "startup_failure"), // re-run: failed jobs
		wfRun(8, 15, 70, "completed", "neutral"),         // not a failure
		githubtest.WorkflowRun{ID: 9, WorkflowID: 16, RunNumber: 1, HeadSHA: "0ther0", Status: "completed", Conclusion: "failure"}, // another commit
	)
	c := e.connect(t)
	res, err := c.Apply(ctx, ciRerun(""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.Outcome != "" || res.Object == nil || res.Object.Ref != docs+"#60" || res.URL != githubtest.WebURL+"/"+docs+"/pull/60/checks" {
		t.Errorf("result %+v (object %+v)", res, res.Object)
	}
	want := map[int64]bool{2: true, 3: false, 6: true, 7: true}
	if got := reruns(e.srv); !maps.Equal(got, want) {
		t.Errorf("re-runs %v, want %v", got, want)
	}
	for _, tr := range e.srv.TokenRequests() {
		if tr.InstallationID != 1 || !maps.Equal(tr.Permissions, map[string]string{"actions": "write", "pull_requests": "read"}) {
			t.Errorf("token request %+v, want the author's actions: write, pull_requests: read", tr)
		}
	}

	// The same key on the same head: a replay that posts nothing, even once the runs failed again.
	for _, r := range e.srv.WorkflowRuns(docs) {
		if r.Attempt > 1 {
			e.srv.SetWorkflowRun(docs, r.ID, func(r *githubtest.WorkflowRun) { r.Status, r.Conclusion = "completed", "failure" })
		}
	}
	posts := e.srv.Count(githubtest.RouteRerunFailedJobs) + e.srv.Count(githubtest.RouteRerun)
	again, err := c.Apply(ctx, ciRerun(""))
	if err != nil || !again.Replayed || again.URL != res.URL || again.Outcome != "" {
		t.Errorf("replay = %+v, %v", again, err)
	}
	if n := e.srv.Count(githubtest.RouteRerunFailedJobs) + e.srv.Count(githubtest.RouteRerun); n != posts {
		t.Errorf("the replay posted %d re-runs", n-posts)
	}

	// Another key is another effect: the runs that failed again are re-run.
	if res, err := c.Apply(ctx, ciRerun("eff_other")); err != nil || res.Replayed {
		t.Errorf("another key = %+v, %v", res, err)
	}
	if n := e.srv.Count(githubtest.RouteRerunFailedJobs) + e.srv.Count(githubtest.RouteRerun); n != posts+4 {
		t.Errorf("another key posted %d re-runs, want 4", n-posts)
	}

	// A new head with the first key: its failed run is re-run.
	e.srv.UpdateObject(docs, 60, func(o *githubtest.Object) { o.Head = "def456" })
	e.srv.AddWorkflowRuns(docs, githubtest.WorkflowRun{ID: 10, WorkflowID: ciWorkflow, RunNumber: 12, HeadSHA: "def456", Status: "completed", Conclusion: "failure"})
	if res, err := c.Apply(ctx, ciRerun("")); err != nil || res.Replayed || res.Outcome != "" {
		t.Errorf("new head = %+v, %v", res, err)
	}
	if got := reruns(e.srv); !got[10] {
		t.Errorf("the new head's run wasn't re-run: %v", got)
	}
}

// Nothing to re-run (every latest run green or running, or no runs at all) is the
// nothing-to-rerun outcome, posts nothing, and isn't remembered: once a run fails, the same
// key re-runs it.
func TestApplyCIRerunNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedEffects(e.srv)
	c := e.connect(t)
	if res, err := c.Apply(ctx, ciRerun("")); err != nil || res.Outcome != github.OutcomeNothingToRerun || res.Replayed {
		t.Fatalf("no runs: %+v, %v", res, err)
	}
	e.srv.AddWorkflowRuns(docs,
		wfRun(1, ciWorkflow, 10, "completed", "failure"), wfRun(2, ciWorkflow, 11, "completed", "success"), // superseded failure
		wfRun(3, preview, 20, "queued", ""), wfRun(4, lint, 30, "completed", "skipped"))
	if res, err := c.Apply(ctx, ciRerun("")); err != nil || res.Outcome != github.OutcomeNothingToRerun || res.Replayed {
		t.Fatalf("green and running: %+v, %v", res, err)
	}
	if n := e.srv.Count(githubtest.RouteRerunFailedJobs) + e.srv.Count(githubtest.RouteRerun); n != 0 {
		t.Errorf("posted %d re-runs", n)
	}
	e.srv.SetWorkflowRun(docs, 3, func(r *githubtest.WorkflowRun) { r.Status, r.Conclusion = "completed", "failure" })
	if res, err := c.Apply(ctx, ciRerun("")); err != nil || res.Outcome != "" || res.Replayed || !reruns(e.srv)[3] {
		t.Errorf("after a failure: %+v, %v, re-runs %v", res, err, reruns(e.srv))
	}
}

// The runs list is read page by page.
func TestApplyCIRerunPages(t *testing.T) {
	e := newEnv(t)
	seedEffects(e.srv)
	e.srv.SetPageSize(2)
	e.srv.AddWorkflowRuns(docs, wfRun(1, 1, 1, "completed", "success"), wfRun(2, 2, 1, "completed", "success"),
		wfRun(3, 3, 1, "completed", "success"), wfRun(4, 4, 1, "completed", "failure"))
	if _, err := e.connect(t).Apply(context.Background(), ciRerun("")); err != nil {
		t.Fatal(err)
	}
	if got := reruns(e.srv); !got[4] || len(got) != 1 {
		t.Errorf("re-runs %v, want run 4", got)
	}
}

// TestApplyCIRerunRefused: a closed or merged PR and a non-PR target are ErrInvalid; an App
// without Actions: write is ErrInvalid naming it; GitHub's 403, 404 and 5xx map as the other
// ops' do. Nothing is re-run.
func TestApplyCIRerunRefused(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		seed func(*githubtest.Server)
		req  provider.EffectRequest
		is   error
		msg  string
	}{
		{name: "closed PR", seed: func(s *githubtest.Server) {
			s.UpdateObject(docs, 60, func(o *githubtest.Object) { o.State = "closed" })
		},
			req: ciRerun(""), is: provider.ErrInvalid, msg: "is closed"},
		{name: "merged PR", seed: func(s *githubtest.Server) {
			s.UpdateObject(docs, 60, func(o *githubtest.Object) { o.State = "merged" })
		},
			req: ciRerun(""), is: provider.ErrInvalid, msg: "is merged"},
		{name: "an issue target", req: effectReq(v1alpha1.OpCIRerun, provider.KindIssue, docs+"#57", "author", nil), is: provider.ErrInvalid},
		{name: "an issue number", req: effectReq(v1alpha1.OpCIRerun, "", docs+"#57", "author", nil), is: provider.ErrNotFound},
		{name: "a missing PR", req: effectReq(v1alpha1.OpCIRerun, provider.KindPR, docs+"#999", "author", nil), is: provider.ErrNotFound},
		{name: "an installation without actions: write", seed: func(s *githubtest.Server) {
			s.SetInstallationPermissions(1, map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read", "actions": "read"})
		}, req: ciRerun(""), is: provider.ErrInvalid, msg: "Actions: write"},
		{name: "the reviewer App, which has no Actions", req: effectReq(v1alpha1.OpCIRerun, provider.KindPR, docs+"#60", "reviewer", nil),
			is: provider.ErrInvalid, msg: "Actions: write"},
		{name: "GitHub's 403 on the re-run", seed: func(s *githubtest.Server) {
			s.FailNext(githubtest.RouteRerunFailedJobs, githubtest.Failure{Status: http.StatusForbidden, Message: "Resource not accessible by integration"})
		}, req: ciRerun(""), is: provider.ErrInvalid, msg: "Actions: write"},
		{name: "GitHub's 403 for another reason", seed: func(s *githubtest.Server) {
			s.FailNext(githubtest.RouteRerunFailedJobs, githubtest.Failure{Status: http.StatusForbidden, Message: "Unable to re-run this workflow run because it was created over a month ago"})
		}, req: ciRerun(""), is: provider.ErrInvalid, msg: "over a month ago"},
		{name: "GitHub's 422 on the re-run", seed: func(s *githubtest.Server) {
			s.FailNext(githubtest.RouteRerunFailedJobs, githubtest.Failure{Status: http.StatusUnprocessableEntity, Message: "Validation Failed"})
		}, req: ciRerun(""), is: provider.ErrInvalid},
		{name: "a run gone", seed: func(s *githubtest.Server) {
			s.FailNext(githubtest.RouteRerunFailedJobs, githubtest.Failure{Status: http.StatusNotFound, Message: "Not Found"})
		}, req: ciRerun(""), is: provider.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedEffects(e.srv)
			e.srv.AddWorkflowRuns(docs, wfRun(1, ciWorkflow, 10, "completed", "failure"))
			if tt.seed != nil {
				tt.seed(e.srv)
			}
			_, err := e.connect(t).Apply(ctx, tt.req)
			if !errors.Is(err, tt.is) || !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("Apply = %v, want %v with %q", err, tt.is, tt.msg)
			}
			if got := reruns(e.srv); len(got) != 0 {
				t.Errorf("re-ran %v", got)
			}
		})
	}
}

// A 5xx on a re-run retries the whole op: the run already re-run is queued by then, so only
// the one that failed is posted again.
func TestApplyCIRerunRetries(t *testing.T) {
	e := newEnv(t)
	seedEffects(e.srv)
	e.srv.AddWorkflowRuns(docs, wfRun(1, ciWorkflow, 10, "completed", "failure"), wfRun(2, preview, 20, "completed", "cancelled"))
	c := e.connect(t)
	e.srv.FailNext(githubtest.RouteRerun, githubtest.Failure{Status: http.StatusBadGateway, Message: "Server Error"})
	done := make(chan error, 1)
	go func() {
		_, err := c.Apply(context.Background(), ciRerun(""))
		done <- err
	}()
	e.clk.BlockUntil(1)
	e.clk.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f, w := e.srv.Count(githubtest.RouteRerunFailedJobs), e.srv.Count(githubtest.RouteRerun); f != 1 || w != 2 {
		t.Errorf("rerun-failed-jobs %d, rerun %d; want 1 and 2", f, w)
	}
	if got := reruns(e.srv); !maps.Equal(got, map[int64]bool{1: true, 2: false}) {
		t.Errorf("re-runs %v", got)
	}
}

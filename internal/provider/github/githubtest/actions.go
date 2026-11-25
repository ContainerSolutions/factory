package githubtest

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// WorkflowRun is one GitHub Actions workflow run as the Actions API shows it: its latest
// attempt's Status (queued, in_progress, completed) and Conclusion once completed (success,
// failure, cancelled, …). A re-run (rerun-failed-jobs, rerun) is a new attempt of the same run:
// Attempt goes up and the run is queued again.
type WorkflowRun struct {
	ID         int64
	WorkflowID int64
	RunNumber  int
	Attempt    int
	Event      string
	HeadSHA    string
	Status     string
	Conclusion string
	Created    time.Time
	// FailedJobsOnly records how the run was last re-run: true for rerun-failed-jobs, false
	// for a whole re-run (or never re-run: Attempt is 1).
	FailedJobsOnly bool
}

// rerunnable are the conclusions of a run GitHub lets rerun-failed-jobs re-run: one with a
// failed (or cancelled) job. A green run has none.
var rerunnable = []string{"failure", "timed_out", "cancelled", "startup_failure"}

// AddWorkflowRuns adds workflow runs to repo ("owner/name"); a run's Attempt defaults to 1 and
// its Event to pull_request.
func (s *Server) AddWorkflowRuns(repo string, runs ...WorkflowRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(repo)
	for _, r := range runs {
		if r.Attempt == 0 {
			r.Attempt = 1
		}
		if r.Event == "" {
			r.Event = "pull_request"
		}
		s.runs[key] = append(s.runs[key], r)
	}
}

// WorkflowRuns returns repo's workflow runs, in the order they were added.
func (s *Server) WorkflowRuns(repo string) []WorkflowRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.runs[strings.ToLower(repo)])
}

func (s *Server) routeActions(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs", s.handleWorkflowRuns)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{id}/rerun-failed-jobs", s.handleRerun(RouteRerunFailedJobs, true))
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{id}/rerun", s.handleRerun(RouteRerun, false))
}

func restWorkflowRun(repo string, r WorkflowRun) map[string]any {
	var conclusion any
	if r.Conclusion != "" {
		conclusion = r.Conclusion
	}
	return map[string]any{
		"id": r.ID, "workflow_id": r.WorkflowID, "run_number": r.RunNumber, "run_attempt": r.Attempt, "event": r.Event,
		"head_sha": r.HeadSHA, "status": r.Status, "conclusion": conclusion, "created_at": r.Created.UTC().Format(time.RFC3339),
		"html_url": fmt.Sprintf("%s/%s/actions/runs/%d", WebURL, repo, r.ID),
	}
}

// handleWorkflowRuns lists a repo's workflow runs (actions: read), filtered by head_sha, in
// pages: {total_count, workflow_runs}.
func (s *Server) handleWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteWorkflowRuns)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !allowRepo(w, m, owner, repo, "actions") {
		return
	}
	sha := r.URL.Query().Get("head_sha")
	s.mu.Lock()
	var items []any
	for _, run := range s.runs[strings.ToLower(owner+"/"+repo)] {
		if sha == "" || run.HeadSHA == sha {
			items = append(items, restWorkflowRun(owner+"/"+repo, run))
		}
	}
	s.mu.Unlock()
	lo, hi := s.pageBounds(w, r, len(items))
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(items), "workflow_runs": append([]any{}, items[lo:hi]...)})
}

// handleRerun re-runs a completed workflow run (actions: write): its failed jobs (failedOnly,
// which needs a failed or cancelled run) or all of them. As on GitHub, a run that hasn't
// completed is refused with 403, and a run with no failed job by rerun-failed-jobs with 403.
func (s *Server) handleRerun(route string, failedOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.beginToken(w, r, route)
		if !ok {
			return
		}
		owner, repo := r.PathValue("owner"), r.PathValue("repo")
		if !allowRepoAny(w, m, owner, repo, []string{"actions"}, "write") {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		s.mu.Lock()
		runs := s.runs[strings.ToLower(owner+"/"+repo)]
		i := slices.IndexFunc(runs, func(run WorkflowRun) bool { return run.ID == id })
		status, msg := 0, ""
		switch {
		case i < 0:
			status, msg = http.StatusNotFound, "Not Found"
		case runs[i].Status != "completed":
			status, msg = http.StatusForbidden, "This workflow is already running"
		case failedOnly && !slices.Contains(rerunnable, runs[i].Conclusion):
			status, msg = http.StatusForbidden, "This workflow run has no failed jobs to re-run"
		default:
			runs[i].Attempt++
			runs[i].Status, runs[i].Conclusion, runs[i].FailedJobsOnly = "queued", "", failedOnly
		}
		s.mu.Unlock()
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{})
	}
}

// SetWorkflowRun changes repo's workflow run id with fn (a run finishing, say).
func (s *Server) SetWorkflowRun(repo string, id int64, fn func(*WorkflowRun)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.runs[strings.ToLower(repo)]
	if i := slices.IndexFunc(runs, func(r WorkflowRun) bool { return r.ID == id }); i >= 0 {
		fn(&runs[i])
	}
}

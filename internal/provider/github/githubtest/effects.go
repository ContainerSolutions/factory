package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// WebURL is the base of the html_url links the server returns.
const WebURL = "https://github.test"

// Repo is a repository's settings, as GET /repos/{owner}/{repo} shows them.
type Repo struct {
	AllowMergeCommit bool
	AllowSquashMerge bool
	AllowRebaseMerge bool
	DefaultBranch    string
}

// defaultRepo is a repo nobody configured: every merge method allowed, as on GitHub.
var defaultRepo = Repo{AllowMergeCommit: true, AllowSquashMerge: true, AllowRebaseMerge: true, DefaultBranch: "main"}

// SetRepo sets a repo's settings (default: every merge method, branch main).
func (s *Server) SetRepo(repo string, r Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	s.repoConfig[strings.ToLower(repo)] = r
}

// GraphQL operations of the review-thread effects.
const (
	opThread        = "ApplyThread"
	opThreadReply   = "ApplyThreadReply"
	opResolveThread = "ApplyResolveThread"
)

func (s *Server) routeEffects(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/{owner}/{repo}", s.handleRepo)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues", s.handleListIssues)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", s.handleCreateIssue)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/{number}", s.handleUpdateIssue)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/sub_issues", s.handleAddSubIssue)
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", s.handleCreatePull)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", s.handlePull)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{number}", s.handleUpdatePull)
	mux.HandleFunc("PUT /repos/{owner}/{repo}/pulls/{number}/merge", s.handleMerge)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews", s.handleReviews)
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls/{number}/reviews", s.handleCreateReview)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews/{id}", s.handleReview)
}

// htmlURL is an object's web link.
func htmlURL(o *Object) string {
	if o.PR {
		return fmt.Sprintf("%s/%s/pull/%d", WebURL, o.Repo, o.Number)
	}
	return fmt.Sprintf("%s/%s/issues/%d", WebURL, o.Repo, o.Number)
}

// nullable is s, or JSON null when it is empty (GitHub's empty bodies).
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func restComment(o *Object, c Comment) map[string]any {
	return map[string]any{
		"id": c.ID, "body": c.Body, "user": restUser(c.Author),
		"html_url":  fmt.Sprintf("%s#issuecomment-%d", htmlURL(o), c.ID),
		"issue_url": fmt.Sprintf("https://api.github.invalid/repos/%s/issues/%d", o.Repo, o.Number),
	}
}

// restPull is a PR as the pulls API shows it (under the lock).
func restPull(o *Object) map[string]any {
	state := o.State
	if state == "merged" {
		state = "closed"
	}
	return map[string]any{
		"id": o.ID, "number": o.Number, "state": state, "merged": o.State == "merged", "draft": o.Draft,
		"title": o.Title, "body": nullable(o.Body), "user": restUser(o.Author), "html_url": htmlURL(o),
		"head": map[string]any{"ref": o.HeadRef, "sha": o.Head, "label": strings.Split(o.Repo, "/")[0] + ":" + o.HeadRef},
		"base": map[string]any{"ref": o.Base}, "merge_commit_sha": nullable(o.MergeCommit),
		"labels": labelList(o.Labels),
	}
}

func restReview(o *Object, rv Review) map[string]any {
	return map[string]any{
		"id": rv.ID, "user": restUser(rv.Author), "state": rv.State, "body": rv.Body, "commit_id": rv.CommitID,
		"html_url": fmt.Sprintf("%s/%s/pull/%d#pullrequestreview-%d", WebURL, o.Repo, o.Number, rv.ID),
	}
}

// pathRepo is the route's "owner/repo".
func pathRepo(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }

// decode reads the JSON body into v, or writes 400.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	return true
}

// lookupPR is lookupObject for the pulls routes: an issue is 404 there.
func (s *Server) lookupPR(w http.ResponseWriter, r *http.Request) (*Object, bool) {
	o, ok := s.lookupObject(w, r)
	if ok && !o.PR {
		writeError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return o, ok
}

// nextNumberLocked is the next issue or PR number in repo (they share one sequence).
func (s *Server) nextNumberLocked(repo string) int {
	n := 0
	for _, o := range s.objects {
		if strings.EqualFold(o.Repo, repo) {
			n = max(n, o.Number)
		}
	}
	return n + 1
}

// handleIssueSub serves GET …/issues/{number}/comments, …/issues/{number}/sub_issues and
// …/issues/comments/{id} (number "comments").
func (s *Server) handleIssueSub(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.PathValue("number") == "comments":
		s.handleComment(w, r)
	case r.PathValue("sub") == "comments":
		s.handleComments(w, r)
	case r.PathValue("sub") == "sub_issues":
		s.handleSubIssues(w, r)
	default:
		writeError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteRepo)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	// metadata: read comes with every installation token, so any token on the repo reads it.
	if !strings.EqualFold(m.installation.Owner, owner) || !slices.ContainsFunc(m.repos, func(x string) bool { return strings.EqualFold(x, repo) }) {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.mu.Lock()
	cfg, found := s.repoConfig[strings.ToLower(owner+"/"+repo)]
	s.mu.Unlock()
	if !found {
		cfg = defaultRepo
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"full_name": owner + "/" + repo, "default_branch": cfg.DefaultBranch,
		"allow_merge_commit": cfg.AllowMergeCommit, "allow_squash_merge": cfg.AllowSquashMerge, "allow_rebase_merge": cfg.AllowRebaseMerge,
	})
}

// handleListIssues lists a repo's issues and PRs: state (open, closed, all; default open),
// creator, sort=created and direction (default desc); newest first.
func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteListIssues)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"issues"}, "read") {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	repo := pathRepo(r)
	s.mu.Lock()
	var hits []*Object
	for _, o := range s.objects {
		st := o.State
		if st == "merged" {
			st = "closed"
		}
		switch {
		case !strings.EqualFold(o.Repo, repo),
			state != "all" && st != state,
			q.Get("creator") != "" && !strings.EqualFold(q.Get("creator"), o.Author.Login):
			continue
		}
		hits = append(hits, o)
	}
	sort.Slice(hits, func(i, j int) bool {
		if q.Get("direction") == "asc" {
			return hits[i].Number < hits[j].Number
		}
		return hits[i].Number > hits[j].Number
	})
	items := make([]any, len(hits))
	for i, o := range hits {
		items[i] = restIssue(o)
	}
	s.mu.Unlock()
	s.page(w, r, items)
}

func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteCreateIssue)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"issues"}, "write") {
		return
	}
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Title == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	author := User{Login: s.botLogin(m), Bot: true}
	s.mu.Lock()
	o := &Object{
		Repo: pathRepo(r), Number: s.nextNumberLocked(pathRepo(r)), Title: in.Title, Body: in.Body,
		Labels: slices.Clone(in.Labels), Author: author, State: "open", UpdatedAt: s.clock.Now(),
	}
	s.numberLocked(o)
	s.objects[objectKey(o.Repo, o.Number)] = o
	out := restIssue(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteUpdateIssue)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(o), "write") {
		return
	}
	var in struct {
		Title       *string `json:"title"`
		Body        *string `json:"body"`
		State       *string `json:"state"`
		StateReason *string `json:"state_reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.State != nil && *in.State != "open" && *in.State != "closed" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	if in.StateReason != nil && !slices.Contains([]string{"completed", "not_planned", "duplicate", "reopened"}, *in.StateReason) {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	s.mu.Lock()
	if in.Title != nil {
		o.Title = *in.Title
	}
	if in.Body != nil {
		o.Body = *in.Body
	}
	if in.State != nil && o.State != "merged" {
		o.State = *in.State
	}
	if in.StateReason != nil {
		o.StateReason = *in.StateReason
	}
	o.UpdatedAt = s.clock.Now()
	out := restIssue(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleComment serves GET …/issues/comments/{id}.
func (s *Server) handleComment(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteComment)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("sub"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := pathRepo(r)
	s.mu.Lock()
	var obj *Object
	var c Comment
	for _, o := range s.objects {
		if !strings.EqualFold(o.Repo, repo) {
			continue
		}
		if i := slices.IndexFunc(o.Comments, func(c Comment) bool { return c.ID == id }); i >= 0 {
			obj, c = o, o.Comments[i]
		}
	}
	var out map[string]any
	if obj != nil {
		out = restComment(obj, c)
	}
	s.mu.Unlock()
	if obj == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(obj), "read") {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSubIssues(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteSubIssues)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"issues"}, "read") {
		return
	}
	s.mu.Lock()
	var items []any
	for _, n := range o.SubIssues {
		if sub, found := s.objects[objectKey(o.Repo, n)]; found {
			items = append(items, restIssue(sub))
		}
	}
	s.mu.Unlock()
	if items == nil {
		items = []any{}
	}
	s.page(w, r, items)
}

func (s *Server) handleAddSubIssue(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteAddSubIssue)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"issues"}, "write") {
		return
	}
	var in struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	var sub *Object
	for _, x := range s.objects {
		if x.ID == in.SubIssueID && !x.PR {
			sub = x
		}
	}
	status, msg := 0, ""
	switch {
	case sub == nil || o.PR:
		status, msg = http.StatusNotFound, "Not Found"
	case slices.Contains(o.SubIssues, sub.Number):
		status, msg = http.StatusUnprocessableEntity, "Validation Failed: sub-issue already exists"
	case sub == o:
		status, msg = http.StatusUnprocessableEntity, "Validation Failed: an issue can't be its own sub-issue"
	default:
		o.SubIssues = append(o.SubIssues, sub.Number)
		o.UpdatedAt = s.clock.Now()
	}
	out := restIssue(o)
	s.mu.Unlock()
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// openPulls lists a repo's open PRs from the model, filtered by head ("owner:branch").
func (s *Server) openPulls(w http.ResponseWriter, r *http.Request, repo string) {
	head := r.URL.Query().Get("head")
	owner, branch, _ := strings.Cut(head, ":")
	s.mu.Lock()
	var hits []*Object
	for _, o := range s.objects {
		if !o.PR || o.State != "open" || !strings.EqualFold(o.Repo, repo) {
			continue
		}
		if head != "" && (!strings.EqualFold(owner, strings.Split(o.Repo, "/")[0]) || branch != o.HeadRef) {
			continue
		}
		hits = append(hits, o)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Number > hits[j].Number })
	items := make([]any, len(hits))
	for i, o := range hits {
		items[i] = restPull(o)
	}
	s.mu.Unlock()
	s.page(w, r, items)
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RoutePull)
	if !ok {
		return
	}
	o, ok := s.lookupPR(w, r)
	if !ok || !allowRepo(w, m, r.PathValue("owner"), r.PathValue("repo"), "pull_requests") {
		return
	}
	s.mu.Lock()
	out := restPull(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreatePull(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteCreatePull)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"pull_requests"}, "write") {
		return
	}
	var in struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Title == "" || in.Head == "" || in.Base == "" || in.Head == in.Base {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	repo := pathRepo(r)
	author := User{Login: s.botLogin(m), Bot: true}
	s.mu.Lock()
	for _, o := range s.objects {
		if o.PR && o.State == "open" && strings.EqualFold(o.Repo, repo) && o.HeadRef == in.Head {
			s.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, "Validation Failed: A pull request already exists for "+in.Head+".")
			return
		}
	}
	o := &Object{
		Repo: repo, Number: s.nextNumberLocked(repo), PR: true, Title: in.Title, Body: in.Body, Draft: in.Draft,
		HeadRef: in.Head, Head: "sha-" + in.Head, Base: in.Base, Author: author, State: "open", UpdatedAt: s.clock.Now(),
	}
	s.numberLocked(o)
	s.objects[objectKey(o.Repo, o.Number)] = o
	out := restPull(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleUpdatePull(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteUpdatePull)
	if !ok {
		return
	}
	o, ok := s.lookupPR(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"pull_requests"}, "write") {
		return
	}
	var in struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
		Base  *string `json:"base"`
		State *string `json:"state"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	if in.Title != nil {
		o.Title = *in.Title
	}
	if in.Body != nil {
		o.Body = *in.Body
	}
	if in.Base != nil {
		o.Base = *in.Base
	}
	if in.State != nil && o.State != "merged" {
		o.State = *in.State
	}
	o.UpdatedAt = s.clock.Now()
	out := restPull(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleMerge merges a PR with the method asked for, which the repo must allow, at sha (the
// head, when given). An Object's MergeBlock refuses with 405, as branch protection or a
// ruleset would; a stale sha is 409.
func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteMerge)
	if !ok {
		return
	}
	o, ok := s.lookupPR(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"contents"}, "write") ||
		!allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"pull_requests"}, "write") {
		return
	}
	var in struct {
		MergeMethod string `json:"merge_method"`
		SHA         string `json:"sha"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.MergeMethod == "" {
		in.MergeMethod = "merge"
	}
	s.mu.Lock()
	cfg, found := s.repoConfig[strings.ToLower(o.Repo)]
	if !found {
		cfg = defaultRepo
	}
	allowed := map[string]bool{"merge": cfg.AllowMergeCommit, "squash": cfg.AllowSquashMerge, "rebase": cfg.AllowRebaseMerge}
	status, msg := 0, ""
	switch {
	case o.State == "merged":
		status, msg = http.StatusMethodNotAllowed, "Pull Request is already merged"
	case o.State != "open":
		status, msg = http.StatusMethodNotAllowed, "Pull Request is not mergeable"
	case !allowed[in.MergeMethod]:
		status, msg = http.StatusMethodNotAllowed, in.MergeMethod+" merges are not allowed on this repository."
	case in.SHA != "" && in.SHA != o.Head:
		status, msg = http.StatusConflict, "Head branch was modified. Review and try the merge again."
	case o.MergeBlock == MergeReviewRequired:
		status, msg = http.StatusMethodNotAllowed, "At least 1 approving review is required by reviewers with write access."
	case o.MergeBlock != "":
		status, msg = http.StatusMethodNotAllowed, "Pull Request is not mergeable"
	default:
		o.State, o.MergeMethod, o.MergeCommit = "merged", in.MergeMethod, "merge-"+o.Head
		o.UpdatedAt = s.clock.Now()
	}
	sha := o.MergeCommit
	s.mu.Unlock()
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteReviews)
	if !ok {
		return
	}
	o, ok := s.lookupPR(w, r)
	if !ok || !allowRepo(w, m, r.PathValue("owner"), r.PathValue("repo"), "pull_requests") {
		return
	}
	s.mu.Lock()
	items := make([]any, len(o.Reviews))
	for i, rv := range o.Reviews {
		items[i] = restReview(o, rv)
	}
	s.mu.Unlock()
	s.page(w, r, items)
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteReview)
	if !ok {
		return
	}
	o, ok := s.lookupPR(w, r)
	if !ok || !allowRepo(w, m, r.PathValue("owner"), r.PathValue("repo"), "pull_requests") {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	var out map[string]any
	if i := slices.IndexFunc(o.Reviews, func(rv Review) bool { return rv.ID == id }); i >= 0 {
		out = restReview(o, o.Reviews[i])
	}
	s.mu.Unlock()
	if out == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// reviewStates maps a review's event to its state.
var reviewStates = map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}

func (s *Server) handleCreateReview(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteCreateReview)
	if !ok {
		return
	}
	o, ok := s.lookupPR(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), []string{"pull_requests"}, "write") {
		return
	}
	var in struct {
		CommitID string          `json:"commit_id"`
		Body     string          `json:"body"`
		Event    string          `json:"event"`
		Comments []ReviewComment `json:"comments"`
	}
	if !decode(w, r, &in) {
		return
	}
	state, known := reviewStates[in.Event]
	author := User{Login: s.botLogin(m), Bot: true}
	s.mu.Lock()
	status, msg := 0, ""
	switch {
	case !known:
		status, msg = http.StatusUnprocessableEntity, "Validation Failed: event"
	case in.Event != "APPROVE" && in.Body == "":
		status, msg = http.StatusUnprocessableEntity, "Validation Failed: body is required"
	case in.CommitID != "" && in.CommitID != o.Head:
		status, msg = http.StatusUnprocessableEntity, "Validation Failed: commit_id is not part of the pull request"
	case in.Event == "APPROVE" && strings.EqualFold(o.Author.Login, author.Login):
		status, msg = http.StatusUnprocessableEntity, "Can not approve your own pull request"
	}
	var out map[string]any
	if status == 0 {
		s.commentSeq++
		rv := Review{ID: s.commentSeq, Author: author, State: state, Body: in.Body, CommitID: cmpOr(in.CommitID, o.Head), Comments: slices.Clone(in.Comments)}
		o.Reviews = append(o.Reviews, rv)
		o.UpdatedAt = s.clock.Now()
		out = restReview(o, rv)
	}
	s.mu.Unlock()
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// gqlThreadOp answers the review-thread operations from the model: ApplyThread (the thread,
// its PR and its comments), ApplyThreadReply (adds a comment as the token's App) and
// ApplyResolveThread. The token must cover the thread's repo with pull_requests (write for the
// mutations).
func (s *Server) gqlThreadOp(w http.ResponseWriter, m mintedToken, op string, vars map[string]any) {
	id, _ := vars["thread"].(string)
	s.mu.Lock()
	var obj *Object
	at := -1
	for _, o := range s.objects {
		if i := slices.IndexFunc(o.Threads, func(t Thread) bool { return t.ID == id }); i >= 0 {
			obj, at = o, i
		}
	}
	s.mu.Unlock()
	if obj == nil {
		out := gqlErrors("NOT_FOUND", "Could not resolve to a node with the global id of '"+id+"'")
		out["data"] = map[string]any{"node": nil}
		writeJSON(w, http.StatusOK, out)
		return
	}
	owner, name, _ := strings.Cut(obj.Repo, "/")
	level := "write"
	if op == opThread {
		level = "read"
	}
	if !allowRepoAny(w, m, owner, name, []string{"pull_requests"}, level) {
		return
	}
	author := User{Login: s.botLogin(m), Bot: true}
	s.mu.Lock()
	defer s.mu.Unlock()
	th := &obj.Threads[at]
	commentURL := func(c Comment) string {
		return fmt.Sprintf("%s/%s/pull/%d#discussion_r%d", WebURL, obj.Repo, obj.Number, c.ID)
	}
	switch op {
	case opThread:
		nodes := make([]any, len(th.Comments))
		for i, c := range th.Comments {
			nodes[i] = map[string]any{"author": gqlActor(c.Author), "body": c.Body, "url": commentURL(c)}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"node": map[string]any{
			"__typename": "PullRequestReviewThread", "id": th.ID, "isResolved": th.Resolved,
			"pullRequest": map[string]any{"number": obj.Number, "url": htmlURL(obj), "repository": map[string]any{"nameWithOwner": obj.Repo}},
			"comments":    map[string]any{"nodes": nodes},
		}}})
	case opThreadReply:
		body, _ := vars["body"].(string)
		if body == "" {
			writeJSON(w, http.StatusOK, gqlErrors("UNPROCESSABLE", "body can't be blank"))
			return
		}
		s.commentSeq++
		c := Comment{ID: s.commentSeq, Author: author, Body: body}
		th.Comments = append(th.Comments, c)
		obj.UpdatedAt = s.clock.Now()
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"addPullRequestReviewThreadReply": map[string]any{
			"comment": map[string]any{"url": commentURL(c)},
		}}})
	case opResolveThread:
		th.Resolved = true
		obj.UpdatedAt = s.clock.Now()
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{
			"thread": map[string]any{"isResolved": true},
		}}})
	}
}

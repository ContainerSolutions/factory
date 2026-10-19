package githubtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// User is an account: a person, or an App's bot account (Bot, with the REST login
// "<slug>[bot]"; GraphQL spells it "<slug>").
type User struct {
	Login string
	Bot   bool
}

// Comment is one conversation comment on an issue or a PR, or one comment of a review
// thread.
type Comment struct {
	ID     int64
	Author User
	Body   string
}

// Review is one review of a PR. State is APPROVED, CHANGES_REQUESTED or COMMENTED.
type Review struct {
	ID       int64
	Author   User
	State    string
	Body     string
	CommitID string
	// Comments are the review's inline comments.
	Comments []ReviewComment
}

// ReviewComment is one inline comment of a review.
type ReviewComment struct {
	Path string
	Line int
	Side string
	Body string
}

// Thread is one review thread of a PR, by its GraphQL node ID.
type Thread struct {
	ID       string
	Resolved bool
	Comments []Comment
}

// Object is an issue or a PR in the object model. The REST routes read and change it, and
// GraphQL queries without a canned response are answered from it (labels, author, state,
// comments; a PR's draft, head and base, with no reviews, threads or checks).
type Object struct {
	// Repo is "owner/name".
	Repo   string
	Number int
	PR     bool
	// ID is the REST id (sub-issues are added by it); zero is numbered.
	ID    int64
	Title string
	Body  string
	// State is "open", "closed" or "merged" (PRs); StateReason is why an issue was closed.
	State       string
	StateReason string
	Labels      []string
	Author      User
	UpdatedAt   time.Time
	Draft       bool
	// Head is the PR's head commit, HeadRef its branch; Base is the base branch.
	Head     string
	HeadRef  string
	Base     string
	Comments []Comment
	// SubIssues are the numbers of the issue's sub-issues (in the same repo).
	SubIssues []int
	Reviews   []Review
	Threads   []Thread
	// MergeBlock makes PUT …/merge refuse with 405: MergeReviewRequired or MergeNotMergeable.
	MergeBlock string
	// MergeMethod and MergeCommit are set by a merge.
	MergeMethod string
	MergeCommit string
}

// Merge blocks (Object.MergeBlock).
const (
	MergeReviewRequired = "review-required"
	MergeNotMergeable   = "not-mergeable"
)

func (o Object) clone() Object {
	o.Labels = slices.Clone(o.Labels)
	o.Comments = slices.Clone(o.Comments)
	o.SubIssues = slices.Clone(o.SubIssues)
	o.Reviews = slices.Clone(o.Reviews)
	for i := range o.Reviews {
		o.Reviews[i].Comments = slices.Clone(o.Reviews[i].Comments)
	}
	o.Threads = slices.Clone(o.Threads)
	for i := range o.Threads {
		o.Threads[i].Comments = slices.Clone(o.Threads[i].Comments)
	}
	return o
}

// Deployment is a repo's deployment, with its statuses newest first.
type Deployment struct {
	ID          int64
	SHA         string
	Ref         string
	Environment string
	Statuses    []DeploymentStatus
}

// DeploymentStatus is one status of a deployment.
type DeploymentStatus struct {
	State          string
	EnvironmentURL string
}

// GraphQLRequest is one GraphQL request received.
type GraphQLRequest struct {
	Operation string
	Variables map[string]any
}

func objectKey(repo string, n int) string { return strings.ToLower(repo) + "#" + strconv.Itoa(n) }

// AddObject adds (or replaces) an issue or a PR. Comment IDs of zero are numbered; a zero
// UpdatedAt is the clock's now and an empty State is "open".
func (s *Server) AddObject(o Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o = o.clone()
	if o.State == "" {
		o.State = "open"
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = s.clock.Now()
	}
	s.numberLocked(&o)
	s.objects[objectKey(o.Repo, o.Number)] = &o
}

// numberLocked numbers an object's zero IDs: its own, its comments', reviews' and threads'
// comments'.
func (s *Server) numberLocked(o *Object) {
	if o.ID == 0 {
		s.idSeq++
		o.ID = s.idSeq
	}
	s.idSeq = max(s.idSeq, o.ID)
	number := func(id *int64) {
		if *id == 0 {
			s.commentSeq++
			*id = s.commentSeq
		}
		s.commentSeq = max(s.commentSeq, *id)
	}
	for i := range o.Comments {
		number(&o.Comments[i].ID)
	}
	for i := range o.Reviews {
		number(&o.Reviews[i].ID)
	}
	for i := range o.Threads {
		for j := range o.Threads[i].Comments {
			number(&o.Threads[i].Comments[j].ID)
		}
	}
}

// Object returns a copy of an object in the model.
func (s *Server) Object(repo string, n int) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[objectKey(repo, n)]
	if !ok {
		return Object{}, false
	}
	return o.clone(), true
}

// UpdateObject changes an object in the model out of band (a human labelling it, say).
func (s *Server) UpdateObject(repo string, n int, fn func(*Object)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[objectKey(repo, n)]; ok {
		fn(o)
	}
}

// SetGraphQL sets the canned response of a GraphQL operation ("Observe", "ObserveComments",
// …) for exactly these variables (compared as JSON), e.g. one recorded from GitHub.
func (s *Server) SetGraphQL(op string, vars map[string]any, response []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canned[cannedKey(op, vars)] = slices.Clone(response)
}

// GraphQLRequests returns the GraphQL requests received, in order.
func (s *Server) GraphQLRequests() []GraphQLRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries)
}

func cannedKey(op string, vars map[string]any) string {
	b, _ := json.Marshal(vars) // map keys are sorted
	var norm any
	_ = json.Unmarshal(b, &norm)
	b, _ = json.Marshal(norm)
	return op + "\x00" + string(b)
}

// AddDeployment adds a deployment to repo; the newest is the one added last.
func (s *Server) AddDeployment(repo string, d Deployment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(repo)
	d.Statuses = slices.Clone(d.Statuses)
	s.deployments[key] = append([]Deployment{d}, s.deployments[key]...)
}

// AddAccountRepos names an account's repos, which an installation on all of its repos lists.
func (s *Server) AddAccountRepos(owner string, names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(owner)
	s.accountRepo[key] = append(s.accountRepo[key], names...)
}

func (s *Server) routeObjects(mux *http.ServeMux) {
	mux.HandleFunc("GET /installation/repositories", s.handleInstallationRepos)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", s.handleIssue)
	// One pattern for …/issues/{number}/comments, …/issues/{number}/sub_issues and
	// …/issues/comments/{id}, which ServeMux can't tell apart as separate patterns.
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/{sub}", s.handleIssueSub)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", s.handleCreateComment)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/comments/{id}", s.handleUpdateComment)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/labels", s.handleAddLabels)
	mux.HandleFunc("DELETE /repos/{owner}/{repo}/issues/{number}/labels/{name}", s.handleRemoveLabel)
	mux.HandleFunc("GET /search/issues", s.handleSearch)
	mux.HandleFunc("POST /graphql", s.handleGraphQL)
	mux.HandleFunc("GET /repos/{owner}/{repo}/deployments", s.handleDeployments)
	mux.HandleFunc("GET /repos/{owner}/{repo}/deployments/{id}/statuses", s.handleDeploymentStatuses)
}

// allowRepoAny checks that the token covers owner/repo with one of perms at level or above.
func allowRepoAny(w http.ResponseWriter, m mintedToken, owner, repo string, perms []string, level string) bool {
	ok := strings.EqualFold(m.installation.Owner, owner) &&
		slices.ContainsFunc(m.repos, func(r string) bool { return strings.EqualFold(r, repo) }) &&
		slices.ContainsFunc(perms, func(p string) bool { return rank(m.perms[p]) >= rank(level) })
	if !ok {
		writeError(w, http.StatusForbidden, "Resource not accessible by integration")
	}
	return ok
}

// objectPerms are the permissions that reach an object through the issues API.
func objectPerms(o *Object) []string {
	if o != nil && o.PR {
		return []string{"issues", "pull_requests"}
	}
	return []string{"issues"}
}

// lookupObject returns the object of a repo route, or writes 404.
func (s *Server) lookupObject(w http.ResponseWriter, r *http.Request) (*Object, bool) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	s.mu.Lock()
	o, ok := s.objects[objectKey(r.PathValue("owner")+"/"+r.PathValue("repo"), n)]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return o, true
}

// botLogin is the REST login of the App a token belongs to.
func (s *Server) botLogin(m mintedToken) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	slug, ok := s.slugs[m.installation.AppID]
	if !ok {
		slug = fmt.Sprintf("app-%d", m.installation.AppID)
	}
	return slug + "[bot]"
}

func restUser(u User) map[string]any {
	typ := "User"
	if u.Bot {
		typ = "Bot"
	}
	return map[string]any{"login": u.Login, "type": typ}
}

func labelList(labels []string) []any {
	out := make([]any, len(labels))
	for i, l := range labels {
		out[i] = map[string]any{"name": l}
	}
	return out
}

// restIssue is an object as the issues API shows it (under the lock).
func restIssue(o *Object) map[string]any {
	state := o.State
	if state == "merged" {
		state = "closed"
	}
	out := map[string]any{
		"id": o.ID, "number": o.Number, "state": state, "labels": labelList(o.Labels), "user": restUser(o.Author),
		"updated_at": o.UpdatedAt.UTC().Format(time.RFC3339), "comments": len(o.Comments),
		"title": o.Title, "body": nullable(o.Body), "html_url": htmlURL(o), "state_reason": nullable(o.StateReason),
	}
	if o.PR {
		out["pull_request"] = map[string]any{"url": "https://api.github.invalid/repos/" + o.Repo + "/pulls/" + strconv.Itoa(o.Number)}
	}
	return out
}

func (s *Server) handleInstallationRepos(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteInstallationRepos)
	if !ok {
		return
	}
	in := m.installation
	s.mu.Lock()
	names := m.repos
	if len(names) == 0 {
		names = in.Repos
	}
	if len(names) == 0 {
		names = s.accountRepo[strings.ToLower(in.Owner)]
	}
	names = slices.Clone(names)
	s.mu.Unlock()
	sort.Strings(names)
	lo, hi := s.pageBounds(w, r, len(names))
	repos := make([]any, 0, hi-lo)
	for _, n := range names[lo:hi] {
		repos = append(repos, map[string]any{"name": n, "full_name": in.Owner + "/" + n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(names), "repositories": repos})
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteIssue)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(o), "read") {
		return
	}
	s.mu.Lock()
	out := restIssue(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteComments)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(o), "read") {
		return
	}
	s.mu.Lock()
	items := make([]any, len(o.Comments))
	for i, c := range o.Comments {
		items[i] = restComment(o, c)
	}
	s.mu.Unlock()
	s.page(w, r, items)
}

func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteCreateComment)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(o), "write") {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Body == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	author := User{Login: s.botLogin(m), Bot: true}
	s.mu.Lock()
	s.commentSeq++
	c := Comment{ID: s.commentSeq, Author: author, Body: in.Body}
	o.Comments = append(o.Comments, c)
	o.UpdatedAt = s.clock.Now()
	out := restComment(o, c)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleUpdateComment(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteUpdateComment)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Body == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	s.mu.Lock()
	var obj *Object
	var at int
	for _, o := range s.objects {
		if !strings.EqualFold(o.Repo, repo) {
			continue
		}
		if i := slices.IndexFunc(o.Comments, func(c Comment) bool { return c.ID == id }); i >= 0 {
			obj, at = o, i
		}
	}
	s.mu.Unlock()
	if obj == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(obj), "write") {
		return
	}
	s.mu.Lock()
	obj.Comments[at].Body = in.Body
	obj.UpdatedAt = s.clock.Now()
	out := restComment(obj, obj.Comments[at])
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAddLabels(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteAddLabels)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(o), "write") {
		return
	}
	var in struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Labels) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	s.mu.Lock()
	for _, l := range in.Labels {
		if !slices.ContainsFunc(o.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			o.Labels = append(o.Labels, l)
		}
	}
	o.UpdatedAt = s.clock.Now()
	out := labelList(o.Labels)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRemoveLabel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteRemoveLabel)
	if !ok {
		return
	}
	o, ok := s.lookupObject(w, r)
	if !ok || !allowRepoAny(w, m, r.PathValue("owner"), r.PathValue("repo"), objectPerms(o), "write") {
		return
	}
	name := r.PathValue("name")
	s.mu.Lock()
	i := slices.IndexFunc(o.Labels, func(x string) bool { return strings.EqualFold(x, name) })
	if i >= 0 {
		o.Labels = slices.Delete(o.Labels, i, i+1)
		o.UpdatedAt = s.clock.Now()
	}
	out := labelList(o.Labels)
	s.mu.Unlock()
	if i < 0 {
		writeError(w, http.StatusNotFound, "Label does not exist")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// searchTerm splits a search query into terms, keeping quoted phrases whole.
var searchTerm = regexp.MustCompile(`"[^"]*"|\S+`)

// handleSearch serves the issue search for the qualifiers List uses: repo: (required, and the
// token must cover it), is:open|closed|issue|pr, updated:>=YYYY-MM-DD, and a phrase with
// in:comments (matched case-insensitively in comment bodies). Results are newest first.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteSearch)
	if !ok {
		return
	}
	var (
		repo, state, kind string
		since             time.Time
		phrases           []string
		inComments        bool
	)
	for _, t := range searchTerm.FindAllString(r.URL.Query().Get("q"), -1) {
		switch {
		case strings.HasPrefix(t, `"`):
			phrases = append(phrases, strings.ToLower(strings.Trim(t, `"`)))
		case strings.HasPrefix(t, "repo:"):
			repo = strings.TrimPrefix(t, "repo:")
		case t == "is:open" || t == "is:closed":
			state = strings.TrimPrefix(t, "is:")
		case t == "is:issue" || t == "is:pr":
			kind = strings.TrimPrefix(t, "is:")
		case strings.HasPrefix(t, "updated:>="):
			d, err := time.Parse(time.DateOnly, strings.TrimPrefix(t, "updated:>="))
			if err != nil {
				writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
				return
			}
			since = d
		case t == "in:comments":
			inComments = true
		default:
			writeError(w, http.StatusUnprocessableEntity, "githubtest: unsupported search term "+t)
			return
		}
	}
	owner, name, found := strings.Cut(repo, "/")
	if !found || (len(phrases) > 0 && !inComments) {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	if !allowRepoAny(w, m, owner, name, []string{"issues", "pull_requests"}, "read") {
		return
	}
	s.mu.Lock()
	var hits []*Object
	for _, o := range s.objects {
		st := o.State
		if st == "merged" {
			st = "closed"
		}
		switch {
		case !strings.EqualFold(o.Repo, repo),
			state != "" && st != state,
			kind == "pr" && !o.PR, kind == "issue" && o.PR,
			o.UpdatedAt.Before(since):
			continue
		}
		if len(phrases) == 0 || matchesComments(o, phrases) {
			hits = append(hits, o)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].UpdatedAt.Equal(hits[j].UpdatedAt) {
			return hits[i].UpdatedAt.After(hits[j].UpdatedAt)
		}
		return hits[i].Number > hits[j].Number
	})
	items := make([]any, len(hits))
	for i, o := range hits {
		items[i] = restIssue(o)
	}
	s.mu.Unlock()
	lo, hi := s.pageBounds(w, r, len(items))
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(items), "incomplete_results": false, "items": items[lo:hi]})
}

func matchesComments(o *Object, phrases []string) bool {
	for _, p := range phrases {
		if !slices.ContainsFunc(o.Comments, func(c Comment) bool { return strings.Contains(strings.ToLower(c.Body), p) }) {
			return false
		}
	}
	return true
}

func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteDeployments)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !allowRepo(w, m, owner, repo, "deployments") {
		return
	}
	q := r.URL.Query()
	s.mu.Lock()
	var items []any
	for _, d := range s.deployments[strings.ToLower(owner+"/"+repo)] {
		if (q.Get("environment") != "" && q.Get("environment") != d.Environment) || (q.Get("ref") != "" && q.Get("ref") != d.Ref) {
			continue
		}
		items = append(items, map[string]any{"id": d.ID, "sha": d.SHA, "ref": d.Ref, "environment": d.Environment})
	}
	s.mu.Unlock()
	if items == nil {
		items = []any{}
	}
	s.page(w, r, items)
}

func (s *Server) handleDeploymentStatuses(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteDeploymentStatus)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !allowRepo(w, m, owner, repo, "deployments") {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	var dep *Deployment
	for _, d := range s.deployments[strings.ToLower(owner+"/"+repo)] {
		if d.ID == id {
			dep = &d
		}
	}
	var items []any
	if dep != nil {
		for _, st := range dep.Statuses {
			items = append(items, map[string]any{"state": st.State, "environment": dep.Environment, "environment_url": st.EnvironmentURL})
		}
	}
	s.mu.Unlock()
	if dep == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if items == nil {
		items = []any{}
	}
	s.page(w, r, items)
}

// operationName is the name of a GraphQL query ("query Observe(…)").
var operationName = regexp.MustCompile(`^\s*(?:query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)`)

// handleGraphQL answers a query with its canned response (SetGraphQL), else from the object
// model for the adapter's operations (Observe, ObserveComments, ObserveThreads, ObserveRepo,
// and the review-thread effects: ApplyThread, ApplyThreadReply, ApplyResolveThread).
// A query naming a repo needs a token covering it with issues or pull_requests read (metadata
// read for ObserveRepo).
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteGraphQL)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	match := operationName.FindStringSubmatch(req.Query)
	if match == nil {
		writeJSON(w, http.StatusOK, gqlErrors("", "an operation name is required"))
		return
	}
	op := match[1]
	s.mu.Lock()
	s.queries = append(s.queries, GraphQLRequest{Operation: op, Variables: req.Variables})
	canned, isCanned := s.canned[cannedKey(op, req.Variables)]
	s.mu.Unlock()
	owner, _ := req.Variables["owner"].(string)
	name, _ := req.Variables["name"].(string)
	if owner != "" || name != "" {
		perms := []string{"issues", "pull_requests"}
		if op == "ObserveRepo" {
			perms = []string{"metadata"}
		}
		if !allowRepoAny(w, m, owner, name, perms, "read") {
			return
		}
	}
	if isCanned {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(canned)
		return
	}
	switch op {
	case "Observe":
		n, _ := req.Variables["number"].(float64)
		writeJSON(w, http.StatusOK, s.gqlObserve(owner+"/"+name, int(n)))
	case "ObserveComments":
		id, _ := req.Variables["id"].(string)
		after, _ := req.Variables["after"].(string)
		writeJSON(w, http.StatusOK, s.gqlComments(id, after))
	case "ObserveThreads":
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"node": map[string]any{
			"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": []any{}},
		}}})
	case "ObserveRepo":
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{
			"nameWithOwner": owner + "/" + name, "isArchived": false, "updatedAt": s.clock.Now().UTC().Format(time.RFC3339),
		}}})
	case opThread, opThreadReply, opResolveThread:
		s.gqlThreadOp(w, m, op, req.Variables)
	default:
		writeJSON(w, http.StatusOK, gqlErrors("", "githubtest: no canned response for "+op))
	}
}

func gqlErrors(typ, msg string) map[string]any {
	e := map[string]any{"message": msg}
	if typ != "" {
		e["type"] = typ
	}
	return map[string]any{"errors": []any{e}}
}

func gqlActor(u User) any {
	if u.Login == "" {
		return nil
	}
	if u.Bot {
		return map[string]any{"__typename": "Bot", "login": strings.TrimSuffix(u.Login, "[bot]")}
	}
	return map[string]any{"__typename": "User", "login": u.Login}
}

// gqlCommentPage is a page of an object's comments after cursor ("c<index>"), as GraphQL
// shows it (under the lock).
func (s *Server) gqlCommentPage(o *Object, after string) map[string]any {
	lo := 0
	if n, err := strconv.Atoi(strings.TrimPrefix(after, "c")); err == nil && after != "" {
		lo = min(n, len(o.Comments))
	}
	hi := min(lo+s.pageSize, len(o.Comments))
	nodes := make([]any, 0, hi-lo)
	for _, c := range o.Comments[lo:hi] {
		nodes = append(nodes, map[string]any{"databaseId": c.ID, "author": gqlActor(c.Author), "body": c.Body})
	}
	return map[string]any{
		"totalCount": len(o.Comments),
		"pageInfo":   map[string]any{"hasNextPage": hi < len(o.Comments), "endCursor": "c" + strconv.Itoa(hi)},
		"nodes":      nodes,
	}
}

func (s *Server) gqlObserve(repo string, n int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[objectKey(repo, n)]
	if !ok {
		out := gqlErrors("NOT_FOUND", fmt.Sprintf("Could not resolve to an issue or pull request with the number of %d.", n))
		out["data"] = map[string]any{"repository": map[string]any{"nameWithOwner": repo, "defaultBranchRef": map[string]any{"name": "main"}, "issueOrPullRequest": nil}}
		return out
	}
	labels := make([]any, len(o.Labels))
	for i, l := range o.Labels {
		labels[i] = map[string]any{"name": l}
	}
	obj := map[string]any{
		"id": "obj:" + objectKey(o.Repo, o.Number), "number": o.Number, "updatedAt": o.UpdatedAt.UTC().Format(time.RFC3339),
		"author": gqlActor(o.Author), "labels": map[string]any{"nodes": labels}, "comments": s.gqlCommentPage(o, ""),
	}
	if o.PR {
		var mergedAt any
		if o.State == "merged" {
			mergedAt = o.UpdatedAt.UTC().Format(time.RFC3339)
		}
		obj["__typename"] = "PullRequest"
		obj["state"] = strings.ToUpper(o.State)
		obj["isDraft"] = o.Draft
		obj["headRefOid"] = o.Head
		obj["baseRefName"] = o.Base
		obj["merged"] = o.State == "merged"
		obj["mergedAt"] = mergedAt
		obj["mergeCommit"] = nil
		if o.State == "merged" && o.MergeCommit != "" {
			obj["mergeCommit"] = map[string]any{"oid": o.MergeCommit}
		}
		obj["reviews"] = map[string]any{"nodes": []any{}}
		threads := make([]any, len(o.Threads))
		for i, th := range o.Threads {
			threads[i] = map[string]any{"isResolved": th.Resolved}
		}
		obj["reviewThreads"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": threads}
		obj["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": o.Head, "statusCheckRollup": nil}}}}
	} else {
		obj["__typename"] = "Issue"
		obj["state"] = strings.ToUpper(o.State)
		obj["subIssues"] = map[string]any{"nodes": []any{}}
		obj["closedByPullRequestsReferences"] = map[string]any{"nodes": []any{}}
	}
	return map[string]any{"data": map[string]any{"repository": map[string]any{
		"nameWithOwner": o.Repo, "defaultBranchRef": map[string]any{"name": "main"}, "issueOrPullRequest": obj,
	}}}
}

func (s *Server) gqlComments(id, after string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[strings.TrimPrefix(id, "obj:")]
	if !ok {
		out := gqlErrors("NOT_FOUND", "Could not resolve to a node with the global id of '"+id+"'")
		out["data"] = map[string]any{"node": nil}
		return out
	}
	return map[string]any{"data": map[string]any{"node": map[string]any{"comments": s.gqlCommentPage(o, after)}}}
}

package githubtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Release is a repo's release, as GET /repos/{owner}/{repo}/releases lists it.
type Release struct {
	Tag        string
	Prerelease bool
	Draft      bool
}

// PullRequest is a closed pull request, as GET /repos/{owner}/{repo}/pulls?state=closed lists
// it. A zero MergedAt is a PR closed without merging.
type PullRequest struct {
	Number      int
	MergedAt    time.Time
	MergeCommit string
	Base        string
}

// Delivery is one webhook delivery of an App, as GET /app/hook/deliveries[/{id}] serves it.
type Delivery struct {
	ID          int64
	GUID        string
	DeliveredAt time.Time
	Redelivery  bool
	Event       string
	Action      string
	StatusCode  int
	// Headers and Payload are the request GitHub sent.
	Headers map[string]string
	Payload json.RawMessage
}

// SetPageSize caps every list page (default 100, GitHub's maximum), so tests can paginate.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// AddOrgMember makes login a member of org with role "admin" or "member".
func (s *Server) AddOrgMember(org, login, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(org)
	if s.members[key] == nil {
		s.members[key] = map[string]string{}
	}
	s.members[key][login] = role
}

// AddTeam creates the team org/slug with members logins.
func (s *Server) AddTeam(org, slug string, logins ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teams[strings.ToLower(org+"/"+slug)] = slices.Clone(logins)
}

// AddReleases adds releases to repo ("owner/name"), listed in the order given.
func (s *Server) AddReleases(repo string, rels ...Release) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(repo)
	s.releases[key] = append(s.releases[key], rels...)
}

// AddPulls adds closed pull requests to repo ("owner/name"), listed in the order given.
func (s *Server) AddPulls(repo string, prs ...PullRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(repo)
	s.pulls[key] = append(s.pulls[key], prs...)
}

// SetAncestor records that commit ancestor is an ancestor of descendant in repo: the compare
// API then answers "ahead" for ancestor...descendant and "behind" the other way. Equal commits
// are "identical"; anything else is "diverged".
func (s *Server) SetAncestor(repo, ancestor, descendant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ancestors[strings.ToLower(repo)+"\x00"+ancestor+"\x00"+descendant] = true
}

// EnableHook gives an App a webhook, so its deliveries can be listed (an App without one gets
// 404, as on GitHub).
func (s *Server) EnableHook(appID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks[appID] = true
}

// AddDelivery records a delivery of the App's webhook (EnableHook is implied).
func (s *Server) AddDelivery(appID int64, d Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks[appID] = true
	d.Payload = slices.Clone(d.Payload)
	s.deliveries[appID] = append(s.deliveries[appID], d)
	sort.SliceStable(s.deliveries[appID], func(i, j int) bool {
		return s.deliveries[appID][i].DeliveredAt.After(s.deliveries[appID][j].DeliveredAt)
	})
}

func (s *Server) routeAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /app/hook/deliveries", s.handleDeliveries)
	mux.HandleFunc("GET /app/hook/deliveries/{id}", s.handleDelivery)
	mux.HandleFunc("GET /orgs/{org}/members", s.handleMembers)
	mux.HandleFunc("GET /orgs/{org}/teams/{slug}/members", s.handleTeamMembers)
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases", s.handleReleases)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", s.handlePulls)
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", s.handleCompare)
	s.routeObjects(mux)
	s.routeEffects(mux)
}

// beginToken records a request authenticated with an installation token and returns what
// the token covers, or writes the failure response and returns ok == false.
func (s *Server) beginToken(w http.ResponseWriter, r *http.Request, route string) (mintedToken, bool) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable body")
		return mintedToken{}, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body)) // handlers read it again
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	s.counts[route]++
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body})
	var fail *Failure
	if q := s.failures[route]; len(q) > 0 {
		fail = &q[0]
		s.failures[route] = q[1:]
	}
	m, known := s.minted[tok]
	now := s.clock.Now()
	s.mu.Unlock()
	if fail != nil {
		for k, vs := range fail.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		writeError(w, fail.Status, fail.Message)
		return mintedToken{}, false
	}
	if !known || !now.Before(m.expiresAt) {
		writeError(w, http.StatusUnauthorized, "Bad credentials")
		return mintedToken{}, false
	}
	return m, true
}

// allowRepo checks that the token covers owner/repo with perm at read or above.
func allowRepo(w http.ResponseWriter, m mintedToken, owner, repo, perm string) bool {
	if !strings.EqualFold(m.installation.Owner, owner) ||
		!slices.ContainsFunc(m.repos, func(r string) bool { return strings.EqualFold(r, repo) }) ||
		rank(m.perms[perm]) == 0 {
		writeError(w, http.StatusForbidden, "Resource not accessible by integration")
		return false
	}
	return true
}

// allowOrg checks that the token is on org with members: read or above.
func allowOrg(w http.ResponseWriter, m mintedToken, org string) bool {
	if !strings.EqualFold(m.installation.Owner, org) || rank(m.perms["members"]) == 0 {
		writeError(w, http.StatusForbidden, "Resource not accessible by integration")
		return false
	}
	return true
}

// page writes items[(page-1)*size : page*size] with a Link rel="next" when there are more.
func (s *Server) page(w http.ResponseWriter, r *http.Request, items []any) {
	lo, hi := s.pageBounds(w, r, len(items))
	writeJSON(w, http.StatusOK, items[lo:hi])
}

// pageBounds is the page of n items the request asks for (page, per_page capped by the page
// size), and sets the Link rel="next" header when there are more.
func (s *Server) pageBounds(w http.ResponseWriter, r *http.Request, n int) (lo, hi int) {
	s.mu.Lock()
	size := s.pageSize
	s.mu.Unlock()
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 && v < size {
		size = v
	}
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		p = 1
	}
	lo = min((p-1)*size, n)
	hi = min(lo+size, n)
	if hi < n {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(p+1))
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
		w.Header().Set("Link", "<"+next.String()+`>; rel="next"`)
	}
	return lo, hi
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteMembers)
	if !ok {
		return
	}
	org := r.PathValue("org")
	if !allowOrg(w, m, org) {
		return
	}
	role := r.URL.Query().Get("role")
	if role == "" {
		role = "all"
	}
	s.mu.Lock()
	members, found := s.members[strings.ToLower(org)]
	var logins []string
	for login, rl := range members {
		if role == "all" || rl == role {
			logins = append(logins, login)
		}
	}
	s.mu.Unlock()
	if !found {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	slices.Sort(logins)
	s.page(w, r, users(logins))
}

func (s *Server) handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteTeamMembers)
	if !ok {
		return
	}
	org := r.PathValue("org")
	if !allowOrg(w, m, org) {
		return
	}
	s.mu.Lock()
	logins, found := s.teams[strings.ToLower(org+"/"+r.PathValue("slug"))]
	logins = slices.Clone(logins)
	s.mu.Unlock()
	if !found {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.page(w, r, users(logins))
}

func users(logins []string) []any {
	out := make([]any, len(logins))
	for i, l := range logins {
		out[i] = map[string]any{"login": l, "type": "User"}
	}
	return out
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteReleases)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !allowRepo(w, m, owner, repo, "contents") {
		return
	}
	s.mu.Lock()
	rels := slices.Clone(s.releases[strings.ToLower(owner+"/"+repo)])
	s.mu.Unlock()
	items := make([]any, len(rels))
	for i, rel := range rels {
		items[i] = map[string]any{"tag_name": rel.Tag, "prerelease": rel.Prerelease, "draft": rel.Draft}
	}
	s.page(w, r, items)
}

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RoutePulls)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !allowRepo(w, m, owner, repo, "pull_requests") {
		return
	}
	switch st := r.URL.Query().Get("state"); st {
	case "open":
		s.openPulls(w, r, owner+"/"+repo) // from the object model (effects.go)
		return
	case "closed":
	default:
		writeJSON(w, http.StatusOK, []any{}) // closed PRs (AddPulls) and open ones (the model) only
		return
	}
	s.mu.Lock()
	prs := slices.Clone(s.pulls[strings.ToLower(owner+"/"+repo)])
	s.mu.Unlock()
	items := make([]any, len(prs))
	for i, pr := range prs {
		item := map[string]any{
			"number": pr.Number, "state": "closed", "merged_at": nil, "merge_commit_sha": pr.MergeCommit,
			"base": map[string]any{"ref": pr.Base},
		}
		if !pr.MergedAt.IsZero() {
			item["merged_at"] = pr.MergedAt.UTC().Format(time.RFC3339)
		}
		items[i] = item
	}
	s.page(w, r, items)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	m, ok := s.beginToken(w, r, RouteCompare)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !allowRepo(w, m, owner, repo, "contents") {
		return
	}
	base, head, found := strings.Cut(r.PathValue("basehead"), "...")
	if !found || base == "" || head == "" {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	key := strings.ToLower(owner + "/" + repo)
	s.mu.Lock()
	status := "diverged"
	switch {
	case base == head:
		status = "identical"
	case s.ancestors[key+"\x00"+base+"\x00"+head]:
		status = "ahead"
	case s.ancestors[key+"\x00"+head+"\x00"+base]:
		status = "behind"
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	appID, _, ok := s.begin(w, r, RouteDeliveries)
	if !ok {
		return
	}
	s.mu.Lock()
	hook := s.hooks[appID]
	ds := slices.Clone(s.deliveries[appID])
	size := s.pageSize
	s.mu.Unlock()
	if !hook {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && n > 0 && n < size {
		size = n
	}
	lo := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 || n > len(ds) {
			writeError(w, http.StatusUnprocessableEntity, "Invalid cursor")
			return
		}
		lo = n
	}
	hi := min(lo+size, len(ds))
	if hi < len(ds) {
		q := r.URL.Query()
		q.Set("cursor", strconv.Itoa(hi))
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
		w.Header().Set("Link", "<"+next.String()+`>; rel="next"`)
	}
	items := make([]any, 0, hi-lo)
	for _, d := range ds[lo:hi] {
		items = append(items, deliverySummary(d))
	}
	writeJSON(w, http.StatusOK, items)
}

func deliverySummary(d Delivery) map[string]any {
	return map[string]any{
		"id": d.ID, "guid": d.GUID, "delivered_at": d.DeliveredAt.UTC().Format(time.RFC3339),
		"redelivery": d.Redelivery, "event": d.Event, "action": d.Action, "status_code": d.StatusCode,
		"status": http.StatusText(d.StatusCode),
	}
}

func (s *Server) handleDelivery(w http.ResponseWriter, r *http.Request) {
	appID, _, ok := s.begin(w, r, RouteDelivery)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.mu.Lock()
	var d *Delivery
	for i := range s.deliveries[appID] {
		if s.deliveries[appID][i].ID == id {
			cp := s.deliveries[appID][i]
			d = &cp
		}
	}
	s.mu.Unlock()
	if d == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := deliverySummary(*d)
	out["request"] = map[string]any{"headers": d.Headers, "payload": d.Payload}
	out["response"] = map[string]any{"headers": map[string]string{}, "payload": ""}
	writeJSON(w, http.StatusOK, out)
}

package policy

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// Access is a repo's access for the task (gateway.md, github.com): rw for the task repo whose
// credential writes contents, read for the task repo otherwise and for every read credential's
// repos, unknown for any other repo.
type Access uint8

// The accesses.
const (
	AccessUnknown Access = iota
	AccessRead
	AccessRW
)

// Canonical reports whether u's path is the one that is matched: not encoded another way
// (RawPath empty) and clean, so /a/../b, //a and /a/ never reach a path rule.
func Canonical(u *url.URL) bool { return u.RawPath == "" && path.Clean(u.Path) == u.Path }

// GitHubRequest is a github.com request's repo and kind.
type GitHubRequest struct {
	// Repo is the path's owner/repo, lower-cased without .git; "" when the path names none.
	Repo string
	// Git is whether it is a git request: the repo segment ends in .git (any case), or the rest
	// of the path, lower-cased, is info/refs, git-upload-pack or git-receive-pack. Every other
	// request is a web request.
	Git  bool
	rest string
}

// ParseGitHubPath splits a canonical github.com path, /<owner>/<repo>[.git]/<rest>.
func ParseGitHubPath(p string) GitHubRequest {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 2 {
		return GitHubRequest{}
	}
	owner, repo := asciiLower(parts[0]), asciiLower(parts[1])
	var req GitHubRequest
	if strings.HasSuffix(repo, ".git") {
		req.Git, repo = true, strings.TrimSuffix(repo, ".git")
	}
	if len(parts) == 3 {
		req.rest = parts[2]
	}
	switch asciiLower(req.rest) {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		req.Git = true
	}
	if owner != "" && repo != "" {
		req.Repo = owner + "/" + repo
	}
	return req
}

// GitHubDecision is the github.com rule's answer for one request inside the tunnel.
type GitHubDecision struct {
	GitHubRequest
	// Access is the repo's, from the access function; AccessUnknown for a path without a repo.
	Access Access
	// Refusal is set when the request is refused; "" to forward it.
	Refusal Code
}

// Credentialed reports whether the repo's token goes with the request: a git request that is
// forwarded, for a repo that is rw or read. Web requests and unknown repos go anonymously.
func (d GitHubDecision) Credentialed() bool {
	return d.Refusal == "" && d.Git && d.Access != AccessUnknown
}

// DecideGitHub applies gateway.md's github.com table to a request inside the tunnel: bad-path
// for a path that isn't canonical; a web request forwarded on GET and HEAD, else web-write; a
// git request for an rw repo forwarded whatever it is, and for a read or unknown repo only the
// two exact fetch forms (GET info/refs with the raw query service=git-upload-pack, and POST
// git-upload-pack without a query), else push-refused. access gives a repo's access; it is
// called only for a git request that names a repo.
func DecideGitHub(method string, u *url.URL, access func(repo string) Access) GitHubDecision {
	if !Canonical(u) {
		return GitHubDecision{Refusal: CodeBadPath}
	}
	d := GitHubDecision{GitHubRequest: ParseGitHubPath(u.Path)}
	if !d.Git {
		if method != http.MethodGet && method != http.MethodHead {
			d.Refusal = CodeWebWrite
		}
		return d
	}
	if d.Repo != "" {
		d.Access = access(d.Repo)
	}
	if d.Access != AccessRW && !gitFetch(method, d.rest, u.RawQuery) {
		d.Refusal = CodePushRefused
	}
	return d
}

// gitFetch reports whether a git request is exactly a smart-HTTP fetch, the only one a repo
// that isn't rw may make. Anything else (a push, a duplicate or case-variant parameter, another
// path) is refused, so a request git http-backend reads differently can't pass.
func gitFetch(method, rest, rawQuery string) bool {
	switch {
	case method == http.MethodGet && rest == "info/refs":
		return rawQuery == "service=git-upload-pack"
	case method == http.MethodPost && rest == "git-upload-pack":
		return rawQuery == ""
	}
	return false
}

// GraphQLPath is api.github.com's GraphQL endpoint.
const GraphQLPath = "/graphql"

// DecideGitHubAPI applies the api.github.com rule: GET and HEAD on any path are reads (the path
// isn't matched, so encoded branch names pass); POST /graphql, exactly and without a query, is
// a read only when its body is (graphql true: the caller checks it with CheckGraphQLRequest);
// every other request is api-write.
func DecideGitHubAPI(method string, u *url.URL) (graphql bool, refusal Code) {
	switch {
	case method == http.MethodGet || method == http.MethodHead:
		return false, ""
	case method == http.MethodPost && u.Path == GraphQLPath && u.RawPath == "" && u.RawQuery == "":
		return true, ""
	}
	return false, CodeAPIWrite
}

// DecideDispatcher applies the dispatcher's only route to a request: POST
// /api/v1/tasks/<taskID>/effects, compared segment by segment after the canonical-path check
// (bad-path), with any query; every other request is dispatcher-route. taskID must not be
// empty.
func DecideDispatcher(method string, u *url.URL, taskID string) Code {
	if !Canonical(u) {
		return CodeBadPath
	}
	segs := strings.Split(u.Path, "/")
	if method != http.MethodPost || taskID == "" || len(segs) != 6 || segs[0] != "" || segs[1] != "api" ||
		segs[2] != "v1" || segs[3] != "tasks" || segs[4] != taskID || segs[5] != "effects" {
		return CodeDispatcherRoute
	}
	return ""
}

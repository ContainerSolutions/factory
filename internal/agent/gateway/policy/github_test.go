package policy

import (
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestDecideGitHub has a row per row of gateway.md's github.com table, for each access.
func TestDecideGitHub(t *testing.T) {
	access := map[string]Access{"acme/rw": AccessRW, "acme/ro": AccessRead}
	tests := []struct {
		name, method, uri string
		repo              string
		git               bool
		// want is the refusal for an rw, a read and an unknown repo ("" to forward).
		want [3]Code
	}{
		{"fetch refs", "GET", "/acme/%s.git/info/refs?service=git-upload-pack", "", true, [3]Code{}},
		{"fetch refs without .git", "GET", "/acme/%s/info/refs?service=git-upload-pack", "", true, [3]Code{}},
		{"fetch pack", "POST", "/acme/%s.git/git-upload-pack", "", true, [3]Code{}},
		{"push refs", "GET", "/acme/%s.git/info/refs?service=git-receive-pack", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"push pack", "POST", "/acme/%s.git/git-receive-pack", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"push pack without .git", "POST", "/acme/%s/git-receive-pack", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"a duplicate parameter", "GET", "/acme/%s.git/info/refs?service=git-upload-pack&service=git-receive-pack", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"an upper-case parameter", "GET", "/acme/%s.git/info/refs?service=GIT-UPLOAD-PACK", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"no query", "GET", "/acme/%s.git/info/refs", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"a fetch pack with a query", "POST", "/acme/%s.git/git-upload-pack?x=1", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"HEAD refs", "HEAD", "/acme/%s.git/info/refs?service=git-upload-pack", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"GET upload-pack", "GET", "/acme/%s.git/git-upload-pack", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"objects", "GET", "/acme/%s.git/objects/info/packs", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"upper-case rest", "POST", "/acme/%s/GIT-RECEIVE-PACK", "", true, [3]Code{"", CodePushRefused, CodePushRefused}},
		{"web GET", "GET", "/acme/%s/releases/download/v1/tool.tgz", "", false, [3]Code{}},
		{"web HEAD", "HEAD", "/acme/%s", "", false, [3]Code{}},
		{"web POST", "POST", "/acme/%s/issues", "", false, [3]Code{CodeWebWrite, CodeWebWrite, CodeWebWrite}},
		{"not canonical", "GET", "/acme/%s/../x/info/refs?service=git-upload-pack", "", false, [3]Code{CodeBadPath, CodeBadPath, CodeBadPath}},
		{"a trailing slash", "GET", "/acme/%s/", "", false, [3]Code{CodeBadPath, CodeBadPath, CodeBadPath}},
	}
	for _, tt := range tests {
		for i, repo := range []string{"rw", "ro", "other"} {
			uri := mustURL(t, strings.ReplaceAll(tt.uri, "%s", repo))
			t.Run(tt.name+"/"+repo, func(t *testing.T) {
				calls := 0
				d := DecideGitHub(tt.method, uri, func(r string) Access { calls++; return access[r] })
				if d.Refusal != tt.want[i] {
					t.Fatalf("refusal %q, want %q", d.Refusal, tt.want[i])
				}
				if d.Refusal == CodeBadPath {
					return
				}
				if d.Git != tt.git || d.Repo != "acme/"+repo {
					t.Errorf("parsed %+v, want git %v repo acme/%s", d.GitHubRequest, tt.git, repo)
				}
				wantCred := tt.git && tt.want[i] == "" && repo != "other"
				if d.Credentialed() != wantCred {
					t.Errorf("credentialed %v, want %v", d.Credentialed(), wantCred)
				}
				if !tt.git && calls != 0 {
					t.Error("a web request looked up the repo's access")
				}
			})
		}
	}
	t.Run("an encoded path", func(t *testing.T) {
		u := mustURL(t, "/acme/rw%2Egit/info/refs?service=git-receive-pack")
		if d := DecideGitHub("GET", u, func(string) Access { return AccessRW }); d.Refusal != CodeBadPath {
			t.Errorf("refusal %q, want bad-path", d.Refusal)
		}
	})
}

func TestParseGitHubPath(t *testing.T) {
	tests := []struct {
		path, repo string
		git        bool
	}{
		{"/Acme/Docs.GIT/info/refs", "acme/docs", true},
		{"/acme/docs", "acme/docs", false},
		{"/acme/docs.git", "acme/docs", true},
		{"/acme/.git/info/refs", "", true},
		{"/acme", "", false},
		{"/", "", false},
		{"/acme/docs/info/refs/x", "acme/docs", false},
	}
	for _, tt := range tests {
		if got := ParseGitHubPath(tt.path); got.Repo != tt.repo || got.Git != tt.git {
			t.Errorf("%s: %+v, want repo %q git %v", tt.path, got, tt.repo, tt.git)
		}
	}
}

func TestDecideGitHubAPI(t *testing.T) {
	tests := []struct {
		method, uri string
		graphql     bool
		code        Code
	}{
		{"GET", "/repos/acme/docs/branches/feature%2Fx", false, ""},
		{"HEAD", "/repos/acme/docs", false, ""},
		{"POST", "/graphql", true, ""},
		{"POST", "/graphql?x=1", false, CodeAPIWrite},
		{"POST", "/graphql/", false, CodeAPIWrite},
		{"POST", "/%67raphql", false, CodeAPIWrite},
		{"POST", "/repos/acme/docs/issues", false, CodeAPIWrite},
		{"PUT", "/graphql", false, CodeAPIWrite},
		{"PATCH", "/repos/acme/docs", false, CodeAPIWrite},
		{"DELETE", "/repos/acme/docs", false, CodeAPIWrite},
	}
	for _, tt := range tests {
		graphql, code := DecideGitHubAPI(tt.method, mustURL(t, tt.uri))
		if graphql != tt.graphql || code != tt.code {
			t.Errorf("%s %s: graphql %v code %q, want %v %q", tt.method, tt.uri, graphql, code, tt.graphql, tt.code)
		}
	}
}

func TestDecideDispatcher(t *testing.T) {
	tests := []struct {
		method, uri string
		code        Code
	}{
		{"POST", "/api/v1/tasks/task-1/effects", ""},
		{"POST", "/api/v1/tasks/task-1/effects?wait=1", ""},
		{"POST", "/api/v1/tasks/task-2/effects", CodeDispatcherRoute},
		{"GET", "/api/v1/tasks/task-1/effects", CodeDispatcherRoute},
		{"PUT", "/api/v1/runs/run-1/transcript", CodeDispatcherRoute},
		{"GET", "/api/v1/tasks/task-1/prompt", CodeDispatcherRoute},
		{"POST", "/api/v1/tasks/task-1/effects/x", CodeDispatcherRoute},
		{"POST", "/API/v1/tasks/task-1/effects", CodeDispatcherRoute},
		{"POST", "/api/v1/tasks/task-1/effects/", CodeBadPath},
		{"POST", "/api/v1/tasks/x/../task-1/effects", CodeBadPath},
		{"POST", "/api/v1/tasks/task%2D1/effects", CodeBadPath},
		{"POST", "//api/v1/tasks/task-1/effects", CodeBadPath},
	}
	for _, tt := range tests {
		if code := DecideDispatcher(tt.method, mustURL(t, tt.uri), "task-1"); code != tt.code {
			t.Errorf("%s %s: %q, want %q", tt.method, tt.uri, code, tt.code)
		}
	}
	if code := DecideDispatcher("POST", mustURL(t, "/api/v1/tasks//effects"), ""); code != CodeBadPath {
		t.Errorf("an empty task: %q, want bad-path", code)
	}
}

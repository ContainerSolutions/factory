package stack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHub(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/repos/compliance-framework/workflows/issues/123":
			_, _ = w.Write([]byte(`{"number":123,"body":"<!-- train-state {} -->"}`))
		case "/repos/compliance-framework/api/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v0.22.0","prerelease":false}`))
		case "/repos/compliance-framework/ui/releases/latest":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	g := GitHub{BaseURL: srv.URL + "/"}
	body, err := g.IssueBody(ctx, IssueRef{Owner: "compliance-framework", Repo: "workflows", Number: 123})
	if err != nil || body != "<!-- train-state {} -->" {
		t.Errorf("IssueBody = %q, %v", body, err)
	}
	if tag, err := g.LatestRelease(ctx, "compliance-framework", "api"); err != nil || tag != "v0.22.0" {
		t.Errorf("LatestRelease = %q, %v", tag, err)
	}
	if _, err := g.LatestRelease(ctx, "compliance-framework", "ui"); err == nil {
		t.Error("a release without a tag passed")
	}
	if _, err := g.LatestRelease(ctx, "compliance-framework", "agent"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing release: %v", err)
	}
	if auth[0] != "" {
		t.Errorf("anonymous reader sent %q", auth[0])
	}
	g.Token = "tok"
	if _, err := g.LatestRelease(ctx, "compliance-framework", "api"); err != nil || auth[len(auth)-1] != "Bearer tok" {
		t.Errorf("with a token: %v %q", err, auth[len(auth)-1])
	}
}

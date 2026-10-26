package github_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github/githubtest"
)

func TestMissingInstallations(t *testing.T) {
	e := newEnv(t)
	e.srv.AddAccountRepos(org, "docs", "local-dev")
	// The reviewer App is on docs only; the author App on every repo of the org; nobody on
	// another-org.
	e.srv.AddInstallation(githubtest.Installation{ID: 3, AppID: reviewerApp, Owner: "solo", User: true, Repos: []string{"tools"},
		Permissions: map[string]string{"metadata": "read", "contents": "read"}})
	c := e.connect(t)
	ctx := t.Context()

	tests := []struct {
		identity string
		repos    []string
		app      string
		missing  []string
	}{
		{"author", []string{docs, org + "/local-dev"}, "ccf-factory", nil},
		{"author", []string{docs, "another-org/site"}, "ccf-factory", []string{"another-org/site"}},
		{"reviewer", []string{"solo/tools", "solo/other", "Compliance-Framework/Docs"}, "ccf-factory-reviewer", []string{"solo/other"}},
	}
	for _, tt := range tests {
		app, missing, err := c.MissingInstallations(ctx, tt.identity, tt.repos)
		if err != nil {
			t.Fatalf("%s %v: %v", tt.identity, tt.repos, err)
		}
		if app != tt.app || !slices.Equal(missing, tt.missing) {
			t.Errorf("%s %v = %q, %v; want %q, %v", tt.identity, tt.repos, app, missing, tt.app, tt.missing)
		}
	}
	if _, _, err := c.MissingInstallations(ctx, "author", []string{"no-owner"}); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("a repo without owner = %v, want ErrInvalid", err)
	}
}

// A mint on a repo the App isn't installed on says so, with the App's slug.
func TestMintNotInstalled(t *testing.T) {
	e := newEnv(t)
	c := e.connect(t)
	_, err := c.MintGitCredential(t.Context(), provider.ReadOnlyRequest("reviewer", "another-org/site", time.Hour))
	var ni *provider.NotInstalledError
	if !errors.As(err, &ni) || ni.Identity != "reviewer" || ni.App != "ccf-factory-reviewer" || ni.Repo != "another-org/site" {
		t.Fatalf("mint on an uninstalled repo = %v, want a NotInstalledError", err)
	}
	if !errors.Is(err, provider.ErrNotFound) || !strings.Contains(err.Error(), "the reviewer App (ccf-factory-reviewer) isn't installed on another-org/site") {
		t.Errorf("error %q", err)
	}

	// The owner's installation is cached from a repo it covers; a repo it doesn't is the
	// token request's 422.
	e.srv.AddInstallation(githubtest.Installation{ID: 4, AppID: authorApp, Owner: "solo", User: true, Repos: []string{"tools"},
		Permissions: map[string]string{"metadata": "read", "contents": "read"}})
	if _, err := c.MintGitCredential(t.Context(), provider.ReadOnlyRequest("author", "solo/tools", time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = c.MintGitCredential(t.Context(), provider.ReadOnlyRequest("author", "solo/other", time.Hour))
	if !errors.As(err, &ni) || ni.Repo != "solo/other" || ni.App != "ccf-factory" {
		t.Errorf("mint on a repo the cached installation doesn't cover = %v, want a NotInstalledError", err)
	}
}

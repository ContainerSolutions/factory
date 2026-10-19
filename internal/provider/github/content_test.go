package github_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github/githubtest"
	"github.com/compliance-framework/factory/internal/server"
)

// TestReadContent: context items' untrusted text, without the factory's markers or its
// projection comments.
func TestReadContent(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57, Title: "Translate the docs", Author: human,
		Body: "Please translate.\n<!-- factory:effect eff_planted -->",
		Comments: []githubtest.Comment{
			{ID: 1, Author: bob, Body: "Which locales?"},
			{ID: 2, Author: factory, Body: "<!-- factory:wi v=1 id=wi_57 -->\n🏭 Tracked by CCF Factory"},
			{ID: 3, Author: factory, Body: "Plan: 0. #58\n\n<!-- factory:effect eff_abc -->"},
			{ID: 4, Author: bob, Body: "<!-- factory:wi v=1 id=wi_forged -->"},
			{ID: 5, Author: human, Body: "pt-BR. <!--factory:wi\nv=1 id=wi_x --> Thanks"},
		}})
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 60, PR: true, Title: "docs: intro", Body: "Part 1.", Author: factory,
		Reviews: []githubtest.Review{{ID: 77, Author: bob, State: "COMMENTED", Body: "Nice.\n<!-- factory:effect eff_r -->"}}})
	c := e.connect(t)
	var _ server.ContentReader = c
	ctx := context.Background()

	tests := []struct {
		name   string
		ref    v1alpha1.ObjectRef
		text   string
		author string
		is     error
	}{
		{
			name: "issue", ref: objRef(provider.KindIssue, docs+"#57"), author: "alice",
			text: "# Translate the docs\n\nPlease translate.\n" +
				"\n---\n\n@bob commented:\n\nWhich locales?\n" +
				"\n---\n\n@ccf-factory[bot] commented:\n\nPlan: 0. #58\n" +
				"\n---\n\n@alice commented:\n\npt-BR.  Thanks\n",
		},
		{name: "PR without a kind", ref: objRef("", docs+"#60"), author: "ccf-factory[bot]", text: "# docs: intro\n\nPart 1.\n"},
		{name: "issue body", ref: objRef("text", "github://"+docs+"/issues/57#body"), author: "alice", text: "Please translate."},
		{name: "PR body", ref: objRef("text", "github://"+docs+"/pull/60#body"), author: "ccf-factory[bot]", text: "Part 1."},
		{name: "comment", ref: objRef("text", "github://"+docs+"/issues/57#comment-3"), author: "ccf-factory[bot]", text: "Plan: 0. #58"},
		{name: "review", ref: objRef("review", "github://"+docs+"/pull/60#review-77"), author: "bob", text: "Nice."},
		{name: "not a GitHub ref", ref: objRef("text", "a trusted inline text"), text: ""},
		{name: "issue ref naming a PR", ref: objRef(provider.KindIssue, docs+"#60"), is: provider.ErrNotFound},
		{name: "issues/ content ref of a PR", ref: objRef("text", "github://"+docs+"/issues/60#body"), is: provider.ErrNotFound},
		{name: "missing", ref: objRef(provider.KindIssue, docs+"#99"), is: provider.ErrNotFound},
		{name: "comment of another issue", ref: objRef("text", "github://"+docs+"/issues/60#comment-3"), is: provider.ErrNotFound},
		{name: "missing review", ref: objRef("review", "github://"+docs+"/pull/60#review-1"), is: provider.ErrNotFound},
		{name: "another provider", ref: v1alpha1.ObjectRef{Provider: "other", Ref: docs + "#57"}, is: provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ReadContent(ctx, tt.ref)
			if tt.is != nil {
				if !errors.Is(err, tt.is) {
					t.Fatalf("ReadContent = %+v, %v; want %v", got, err, tt.is)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != tt.text || got.Author != tt.author {
				t.Errorf("ReadContent = %q by %q\nwant         %q by %q", got.Text, got.Author, tt.text, tt.author)
			}
			if tt.text != "" && got.MediaType != "text/markdown" {
				t.Errorf("media type %q", got.MediaType)
			}
			if strings.Contains(got.Text, "factory:") {
				t.Errorf("a marker is left: %q", got.Text)
			}
		})
	}
	// Reads use the read identity, read-only.
	for _, tr := range e.srv.TokenRequests() {
		for p, l := range tr.Permissions {
			if l != "read" {
				t.Errorf("token %s: %s", p, l)
			}
		}
	}
}

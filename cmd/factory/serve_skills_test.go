package main

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
)

func TestSkillsToken(t *testing.T) {
	ghProvider := func(name, api string) *v1alpha1.Provider {
		return &v1alpha1.Provider{
			Metadata: v1alpha1.ObjectMeta{Name: name},
			Spec: v1alpha1.ProviderSpec{Type: v1alpha1.ProviderTypeGitHub, GitHub: &v1alpha1.GitHubProviderSpec{
				APIURL:     api,
				Identities: map[string]v1alpha1.AppIdentity{"author": {AppID: 1}, "reviewer": {AppID: 2}},
			}},
		}
	}
	public := ghProvider("github-public", "https://api.github.com")
	other := ghProvider("github-other", "")
	ghe := ghProvider("ghe", "https://ghe.example.com/api/v3")
	slack := &v1alpha1.Provider{Metadata: v1alpha1.ObjectMeta{Name: "slack"}, Spec: v1alpha1.ProviderSpec{Type: "slack"}}

	for _, tc := range []struct {
		name      string
		providers []*v1alpha1.Provider
		base      string
		preferred string
		noConn    bool
		want      string // the Provider that mints
		wantErr   string
	}{
		{name: "the only github Provider on the host", providers: []*v1alpha1.Provider{public, ghe, slack}, base: "https://github.com/compliance-framework", want: "github-public"},
		{name: "a GHE skills base", providers: []*v1alpha1.Provider{public, ghe}, base: "https://ghe.example.com/org", want: "ghe"},
		{name: "several: the auth Provider", providers: []*v1alpha1.Provider{public, other}, base: "https://github.com/o", preferred: "github-other", want: "github-other"},
		{name: "several, none the auth Provider", providers: []*v1alpha1.Provider{public, other}, base: "https://github.com/o", preferred: "ghe", wantErr: "share the skills base's git host"},
		{name: "no Provider on the host", providers: []*v1alpha1.Provider{ghe, slack}, base: "https://github.com/o", wantErr: "no github Provider"},
		{name: "not connected", providers: []*v1alpha1.Provider{public}, base: "https://github.com/o", noConn: true, wantErr: "no connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := &catalog.Catalog{Providers: map[string]*v1alpha1.Provider{}}
			conns := controllers.StaticConns{}
			fakes := map[string]*fake.Conn{}
			for _, p := range tc.providers {
				cat.Providers[p.Metadata.Name] = p
				if !tc.noConn {
					c := fake.NewConn(*p, fake.Options{})
					conns[p.Metadata.Name], fakes[p.Metadata.Name] = c, c
				}
			}
			mint := skillsToken(func() *catalog.Catalog { return cat }, conns, tc.base, tc.preferred)
			tok, err := mint(context.Background(), "compliance-framework/ccf-skills")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || tok == "" {
				t.Fatalf("token %q, err %v", tok, err)
			}
			for name, c := range fakes {
				mints := c.Mints()
				if name != tc.want {
					if len(mints) != 0 {
						t.Errorf("%s minted %d tokens, want none", name, len(mints))
					}
					continue
				}
				if len(mints) != 1 {
					t.Fatalf("%s minted %d tokens, want 1", name, len(mints))
				}
				req := mints[0].Request
				if want := provider.ReadOnlyRequest("author", "compliance-framework/ccf-skills", skillsTokenTTL); req.Identity != want.Identity ||
					req.Repo != want.Repo || !maps.Equal(req.Permissions, want.Permissions) || req.TTL != want.TTL || !req.ReadOnly() {
					t.Errorf("mint request = %+v, want %+v", req, want)
				}
				if tok != mints[0].Credential.Token {
					t.Errorf("token is not the minted one")
				}
			}
		})
	}
}

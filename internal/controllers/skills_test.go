package controllers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog/gitsource"
	"github.com/ContainerSolutions/factory/internal/rules"
)

const (
	skillsBase = "https://github.com/compliance-framework"
	skillsURL  = skillsBase + "/ccf-skills"
	skillsRepo = "compliance-framework/ccf-skills"
)

func TestAppendSkillRepos(t *testing.T) {
	use := []string{"lisa"}
	skill := func(source string) v1alpha1.SkillRef { return v1alpha1.SkillRef{Source: source, Ref: "v1", Use: use} }
	for _, tc := range []struct {
		name     string
		read     []string
		taskRepo string
		host     string
		skills   []v1alpha1.SkillRef
		want     []string
	}{
		{name: "short source (unresolved) is skipped", host: "github.com", skills: []v1alpha1.SkillRef{skill("ccf-skills")}},
		{name: "full URL on the host", host: "github.com", skills: []v1alpha1.SkillRef{skill(skillsURL)}, want: []string{skillsRepo}},
		{name: ".git suffix and trailing slash", host: "github.com", skills: []v1alpha1.SkillRef{skill("https://github.com/o/r.git/")}, want: []string{"o/r"}},
		{name: "host matches case-insensitively", host: "github.com", skills: []v1alpha1.SkillRef{skill("https://GitHub.com/o/r")}, want: []string{"o/r"}},
		{name: "URL on another host is not added", host: "github.com", skills: []v1alpha1.SkillRef{skill("https://gitlab.com/o/r")}},
		{name: "a GHE host", host: "ghe.example.com", skills: []v1alpha1.SkillRef{skill("https://github.com/o/r"), skill("https://ghe.example.com/o/r")}, want: []string{"o/r"}},
		{name: "no git host adds nothing", skills: []v1alpha1.SkillRef{skill(skillsURL)}},
		{name: "dedupe with an ro mount", read: []string{"compliance-framework/docs", "Compliance-Framework/CCF-Skills"}, host: "github.com",
			skills: []v1alpha1.SkillRef{skill(skillsURL)}, want: []string{"compliance-framework/docs", "Compliance-Framework/CCF-Skills"}},
		{name: "the task repo is not a read repo", taskRepo: skillsRepo, host: "github.com", skills: []v1alpha1.SkillRef{skill(skillsURL)}},
		{name: "two refs of one repo once", host: "github.com", skills: []v1alpha1.SkillRef{skill(skillsURL), skill(skillsURL + ".git")}, want: []string{skillsRepo}},
		{name: "skills with no use aren't fetched", host: "github.com", skills: []v1alpha1.SkillRef{{Source: skillsURL, Ref: "v1"}}},
		{name: "not a repo path", host: "github.com", skills: []v1alpha1.SkillRef{
			skill("https://github.com/o"), skill("https://github.com/o/r/tree/main"), skill("https://github.com/o/../r"),
			skill("https://x-access-token:t@github.com/o/r"), skill("https://github.com/o/r?x=1"), skill("ssh://git@github.com/o/r"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := appendSkillRepos(slices.Clone(tc.read), tc.taskRepo, tc.host, tc.skills)
			if !slices.Equal(got, tc.want) {
				t.Errorf("read repos = %q, want %q", got, tc.want)
			}
		})
	}
}

// resolveTo resolves every skill source to url (a fixed commit), as GitSkills would.
type resolveTo string

func (r resolveTo) Resolve(context.Context, string, string) (string, string, error) {
	return string(r), "0123456789abcdef0123456789abcdef01234567", nil
}

// TestCompileSkillReadRepos: docs-review (task repo docs, no mounts, the ccf-skills skills)
// gets the skills repo as a read repo when it resolves to a repo on the provider's host.
func TestCompileSkillReadRepos(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	for _, tc := range []struct {
		name   string
		skills SkillResolver
		want   []string
	}{
		{name: "resolved on the provider's host", skills: resolveTo(skillsURL), want: []string{skillsRepo}},
		{name: "unresolved short source", skills: KeepSkills{}},
		{name: "another host", skills: resolveTo("https://gitlab.com/compliance-framework/ccf-skills")},
		{name: "the task repo itself", skills: resolveTo("https://github.com/" + docsRepo)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, _, err := compileTask(e.ctx, compileInput{
				cat: e.cat, wi: wi, ev: v1alpha1.Event{ID: "ev"}, taskID: "tsk_s", now: e.clk.Now(),
				d: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc"},
			}, tc.skills)
			if err != nil {
				t.Fatal(err)
			}
			g := task.Spec.Credentials.Git
			if g.Repo != docsRepo || !slices.Equal(g.ReadRepos, tc.want) {
				t.Errorf("credentials = repo %q read %q, want repo %q read %q", g.Repo, g.ReadRepos, docsRepo, tc.want)
			}
		})
	}
}

func TestGitHost(t *testing.T) {
	gh := func(api string) *v1alpha1.Provider {
		return &v1alpha1.Provider{Spec: v1alpha1.ProviderSpec{Type: v1alpha1.ProviderTypeGitHub, GitHub: &v1alpha1.GitHubProviderSpec{APIURL: api}}}
	}
	for _, tc := range []struct {
		name string
		p    *v1alpha1.Provider
		want string
	}{
		{"api.github.com", gh("https://api.github.com/"), "github.com"},
		{"default API", gh(""), "github.com"},
		{"GHE", gh("https://ghe.example.com/api/v3"), "ghe.example.com"},
		{"not github", &v1alpha1.Provider{Spec: v1alpha1.ProviderSpec{Type: "slack"}}, ""},
		{"nil", nil, ""},
	} {
		if got := GitHost(tc.p); got != tc.want {
			t.Errorf("%s: GitHost = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// lsCall is one recorded ls-remote.
type lsCall struct {
	url, ref string
	auth     gitsource.Auth
}

// fakeLsRemote records ls-remotes; it fails anonymous ones when private, and every one with
// err when set.
type fakeLsRemote struct {
	mu      sync.Mutex
	calls   []lsCall
	private bool
	err     error
}

func (f *fakeLsRemote) run(_ context.Context, url, ref string, auth gitsource.Auth) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, lsCall{url, ref, auth})
	if f.err != nil {
		return "", f.err
	}
	if f.private && auth.Token == "" {
		return "", errors.New("git ls-remote: Authentication failed")
	}
	return "c0ffee", nil
}

func TestGitSkillsToken(t *testing.T) {
	const tok = "ghs_t0ken"
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		source    string
		ref       string // default v1
		token     func(*[]string) func(context.Context, string) (string, error)
		private   bool
		wantAuth  []string // the token of each ls-remote, in order
		wantRepos []string // the repos Token was asked for
		wantErr   []string // substrings of the error
	}{
		{name: "short source on the host", source: "ccf-skills", private: true, token: mintOK(tok), wantAuth: []string{tok}, wantRepos: []string{skillsRepo}},
		{name: "full URL on the host", source: "https://github.com/o/r.git", private: true, token: mintOK(tok), wantAuth: []string{tok}, wantRepos: []string{"o/r"}},
		{name: "another host is anonymous", source: "https://gitlab.com/o/r", token: mintOK(tok), wantAuth: []string{""}},
		{name: "no Token is anonymous", source: "ccf-skills", wantAuth: []string{""}},
		{name: "a pinned commit needs no token", source: "ccf-skills", ref: "0123456789abcdef0123456789abcdef01234567", token: mintOK(tok), wantAuth: []string{""}},
		{name: "mint failure falls back to anonymous", source: "ccf-skills", token: mintFail("App not installed"), wantAuth: []string{""}, wantRepos: []string{skillsRepo}},
		{name: "mint failure on a private repo has both errors", source: "ccf-skills", private: true, token: mintFail("App not installed"),
			wantAuth: []string{""}, wantRepos: []string{skillsRepo}, wantErr: []string{"App not installed", "Authentication failed", skillsRepo}},
		{name: "empty token is a mint failure", source: "ccf-skills", private: true, token: mintOK(""),
			wantAuth: []string{""}, wantRepos: []string{skillsRepo}, wantErr: []string{"empty token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			ls := &fakeLsRemote{private: tc.private}
			g := &GitSkills{Base: skillsBase, lsRemote: ls.run}
			if tc.token != nil {
				g.Token = tc.token(&asked)
			}
			ref := tc.ref
			if ref == "" {
				ref = "v1"
			}
			for range 2 { // the second resolve is cached
				_, commit, err := g.Resolve(ctx, tc.source, ref)
				if len(tc.wantErr) > 0 {
					if err == nil {
						t.Fatal("want an error")
					}
					for _, w := range tc.wantErr {
						if !strings.Contains(err.Error(), w) {
							t.Errorf("error %q lacks %q", err, w)
						}
					}
					break
				}
				if err != nil || commit != "c0ffee" {
					t.Fatalf("Resolve = %q, %v", commit, err)
				}
			}
			var auth []string
			for _, c := range ls.calls {
				auth = append(auth, c.auth.Token)
			}
			if !slices.Equal(auth, tc.wantAuth) {
				t.Errorf("ls-remote tokens = %q, want %q", auth, tc.wantAuth)
			}
			if !slices.Equal(asked, tc.wantRepos) {
				t.Errorf("Token asked for %q, want %q", asked, tc.wantRepos)
			}
		})
	}
}

// TestGitSkillsTokenNotInErrors: a failing authenticated ls-remote (nothing listens on
// port 1) doesn't put the token in the error.
func TestGitSkillsTokenNotInErrors(t *testing.T) {
	const tok = "ghs_n3verL0gged"
	var asked []string
	g := &GitSkills{Base: "https://127.0.0.1:1/o", Token: mintOK(tok)(&asked)}
	_, _, err := g.Resolve(context.Background(), "skills", "v1")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), tok) || !slices.Equal(asked, []string{"o/skills"}) {
		t.Errorf("err = %v (Token asked for %q)", err, asked)
	}
}

func mintOK(tok string) func(*[]string) func(context.Context, string) (string, error) {
	return func(asked *[]string) func(context.Context, string) (string, error) {
		return func(_ context.Context, repo string) (string, error) {
			*asked = append(*asked, repo)
			return tok, nil
		}
	}
}

func mintFail(msg string) func(*[]string) func(context.Context, string) (string, error) {
	return func(asked *[]string) func(context.Context, string) (string, error) {
		return func(_ context.Context, repo string) (string, error) {
			*asked = append(*asked, repo)
			return "", errors.New(msg)
		}
	}
}

package controllers

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ContainerSolutions/factory/internal/provider"
)

// installs is a Conn that is a provider.InstallationChecker: the identities' installed repos.
type installs struct {
	provider.Conn
	installed map[string][]string // identity → repos
	fail      map[string]error
	asked     map[string][]string
}

func (i *installs) MissingInstallations(_ context.Context, identity string, repos []string) (string, []string, error) {
	if i.asked == nil {
		i.asked = map[string][]string{}
	}
	i.asked[identity] = repos
	if err := i.fail[identity]; err != nil {
		return "", nil, err
	}
	var missing []string
	for _, r := range repos {
		if !slices.Contains(i.installed[identity], r) {
			missing = append(missing, r)
		}
	}
	return "ccf-factory-" + identity, missing, nil
}

func TestInstallationTargets(t *testing.T) {
	got := InstallationTargets(loadPoC(t), DefaultRuleIdentity, "https://github.com/compliance-framework")
	if len(got) != 2 || got[0].Identity != "author" || got[1].Identity != "reviewer" || got[0].Provider != ghProvider {
		t.Fatalf("targets = %+v, want author and reviewer on %s", got, ghProvider)
	}
	// docs is a RuleSet target, local-dev a TaskSpec workspace repo, ccf-skills a skill source.
	for _, r := range []string{docsRepo, "compliance-framework/local-dev", "compliance-framework/ccf-skills"} {
		if !slices.Contains(got[0].Repos, r) {
			t.Errorf("author repos %v: no %s", got[0].Repos, r)
		}
	}
	if !slices.Contains(got[1].Repos, docsRepo) || !slices.IsSorted(got[1].Repos) {
		t.Errorf("reviewer repos %v", got[1].Repos)
	}
	// Without a skills base, a bare skill source names no repo.
	for _, tg := range InstallationTargets(loadPoC(t), DefaultRuleIdentity, "") {
		if slices.Contains(tg.Repos, "compliance-framework/ccf-skills") {
			t.Errorf("%s repos %v: a bare skill source without a base", tg.Identity, tg.Repos)
		}
	}
}

func TestInstallationCheck(t *testing.T) {
	e := newEnv(t)
	targets := InstallationTargets(e.cat, DefaultRuleIdentity, "")
	conn := &installs{Conn: e.gh, installed: map[string][]string{"author": slices.DeleteFunc(slices.Clone(targets[0].Repos),
		func(r string) bool { return r == "compliance-framework/local-dev" })}}
	conn.fail = map[string]error{"reviewer": errors.New("github unreachable")}
	c := NewInstallationCheck(StaticConns{ghProvider: conn}, InstallationCheckOptions{Logger: discardLogger()})
	gaps := c.Check(e.ctx, e.cat)
	want := InstallationGap{Provider: ghProvider, Identity: "author", App: "ccf-factory-author", Repo: "compliance-framework/local-dev"}
	if len(gaps) != 1 || gaps[0] != want {
		t.Fatalf("gaps = %+v, want [%+v] (the reviewer's check failed: no gaps for it)", gaps, want)
	}
	if got := c.Gaps(); len(got) != 1 || got[0].Message() !=
		"the author App (ccf-factory-author) isn't installed on compliance-framework/local-dev (provider github-public): install it there" {
		t.Errorf("Gaps = %+v", got)
	}
	if len(conn.asked["reviewer"]) == 0 {
		t.Error("the reviewer's repos weren't checked")
	}

	// A Conn that can't check (the fake) has no gaps.
	c = NewInstallationCheck(e.conns, InstallationCheckOptions{Logger: discardLogger()})
	if gaps := c.Check(e.ctx, e.cat); len(gaps) != 0 {
		t.Errorf("gaps through the fake = %+v", gaps)
	}
}

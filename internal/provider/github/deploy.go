package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/provider"
)

// DeployLinkPRs is how many of the newest merged PRs a deployment is checked against.
const DeployLinkPRs = 20

// deployDrafts is deploy.succeeded / deploy.failed: one Event per merged PR the deployed
// commit contains (its merge commit is an ancestor of, or equal to, the deployment's SHA,
// checked with the compare API on the DeployLinkPRs newest merged PRs), so the Event reaches
// the work items those PRs belong to. A deployment that contains none of them is one Event on
// the deployment itself ("owner/name#d-<id>"). Facts: deploy{environment, sha, url}.
func (c *Conn) deployDrafts(ctx context.Context, repo string, d *ghDeployment, st *ghDeploymentStatus) ([]draft, error) {
	env := st.Environment
	if env == "" {
		env = d.Environment
	}
	u := st.EnvironmentURL
	if u == "" {
		u = st.TargetURL
	}
	facts := map[string]any{"deploy": map[string]any{"environment": env, "sha": d.SHA, "url": u}}
	prs, err := c.deployedPRs(ctx, repo, d.SHA)
	if err != nil {
		return nil, fmt.Errorf("link deployment %d to PRs: %w", d.ID, err)
	}
	if len(prs) == 0 {
		return []draft{{obj: c.ref(KindDeployment, fmt.Sprintf("%s#d-%d", repo, d.ID)), facts: facts}}, nil
	}
	out := make([]draft, 0, len(prs))
	for _, n := range prs {
		out = append(out, draft{obj: c.ref(provider.KindPR, fmt.Sprintf("%s#%d", repo, n)), facts: facts})
	}
	return out, nil
}

// deployedPRs returns the numbers of the newest merged PRs whose merge commit sha contains.
func (c *Conn) deployedPRs(ctx context.Context, repo, sha string) ([]int, error) {
	tok, err := c.repoToken(ctx, repo, map[string]string{"contents": "read", "pull_requests": "read"})
	if err != nil {
		return nil, err
	}
	type merged struct {
		number int
		at     time.Time
		commit string
	}
	var prs []merged
	q := url.Values{"state": {"closed"}, "sort": {"updated"}, "direction": {"desc"}}
	err = c.list(ctx, tok, "repos/"+repo+"/pulls", q, 1, func(item json.RawMessage) (bool, error) {
		var pr struct {
			Number      int        `json:"number"`
			MergedAt    *time.Time `json:"merged_at"`
			MergeCommit string     `json:"merge_commit_sha"`
		}
		if err := json.Unmarshal(item, &pr); err != nil {
			return false, fmt.Errorf("decode pull request: %w", err)
		}
		if pr.MergedAt != nil && pr.MergeCommit != "" {
			prs = append(prs, merged{number: pr.Number, at: *pr.MergedAt, commit: pr.MergeCommit})
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(prs, func(a, b merged) int { return b.at.Compare(a.at) })
	prs = prs[:min(len(prs), DeployLinkPRs)]
	var out []int
	for _, pr := range prs {
		ok, err := c.contains(ctx, tok, repo, pr.commit, sha)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, pr.number)
		}
	}
	return out, nil
}

// DefaultDeployEnvironment is the environment whose latest deployment Observe reports
// (item.deploy): the docs' GitHub Pages.
const DefaultDeployEnvironment = "github-pages"

// deployPerms read deployments and compare commits.
var deployPerms = map[string]string{"contents": "read", "deployments": "read"}

// observeDeploy is the latest deployment of the default branch to the deploy environment
// (github-provider.md §Observe): its latest status's state (pending when it has none), SHA and
// URL, and containsMerge when merge is an ancestor of, or equal to, the deployed SHA. The
// docs' Pages workflow cancels deployments in progress, so only the latest one matters. nil
// when the repo has none.
func (c *Conn) observeDeploy(ctx context.Context, repo, branch, merge string) (*provider.DeployObservation, error) {
	tok, err := c.repoToken(ctx, repo, deployPerms)
	if err != nil {
		return nil, err
	}
	var deps []struct {
		ID  int64  `json:"id"`
		SHA string `json:"sha"`
	}
	q := url.Values{"environment": {c.deployEnv}, "ref": {branch}, "per_page": {"1"}}
	if _, err := c.minter.Get(ctx, tok, "repos/"+repo+"/deployments", q, &deps); err != nil {
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}
	d := deps[0]
	var sts []ghDeploymentStatus
	path := fmt.Sprintf("repos/%s/deployments/%d/statuses", repo, d.ID)
	if _, err := c.minter.Get(ctx, tok, path, url.Values{"per_page": {"1"}}, &sts); err != nil {
		return nil, err
	}
	out := &provider.DeployObservation{State: "pending", SHA: d.SHA}
	if len(sts) > 0 {
		out.State = sts[0].State
		out.URL = cmpOr(sts[0].EnvironmentURL, sts[0].TargetURL)
	}
	if d.SHA != "" {
		if out.ContainsMerge, err = c.contains(ctx, tok, repo, merge, d.SHA); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// contains reports whether commit is an ancestor of, or equal to, sha (the compare API says
// sha is ahead of it or identical). A commit GitHub doesn't know is not contained.
func (c *Conn) contains(ctx context.Context, tok, repo, commit, sha string) (bool, error) {
	var cmp struct {
		Status string `json:"status"`
	}
	path := "repos/" + repo + "/compare/" + url.PathEscape(commit) + "..." + url.PathEscape(sha)
	_, err := c.minter.Get(ctx, tok, path, nil, &cmp)
	if errors.Is(err, provider.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cmp.Status == "ahead" || cmp.Status == "identical", nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

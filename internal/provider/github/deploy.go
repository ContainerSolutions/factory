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
		var cmp struct {
			Status string `json:"status"`
		}
		path := "repos/" + repo + "/compare/" + url.PathEscape(pr.commit) + "..." + url.PathEscape(sha)
		_, err := c.minter.Get(ctx, tok, path, nil, &cmp)
		if errors.Is(err, provider.ErrNotFound) {
			continue // a commit GitHub doesn't know (force-pushed away): not contained
		}
		if err != nil {
			return nil, err
		}
		if cmp.Status == "ahead" || cmp.Status == "identical" {
			out = append(out, pr.number)
		}
	}
	return out, nil
}

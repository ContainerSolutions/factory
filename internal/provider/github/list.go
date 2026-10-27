package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/patterns"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

// ListWindow is how far back List looks: objects updated in the last 90 days.
const ListWindow = 90 * 24 * time.Hour

// maxSearchPages bounds one search (GitHub serves at most 1000 results, 100 per page).
const maxSearchPages = 10

// List implements provider.Conn (github-provider.md §Observe, list and projection): for each
// repo in scope, the open issues and PRs updated in the last ListWindow, plus those (open or
// closed) updated in that window whose comments carry the projection marker's text: two
// searches per repo (search API, paginated), as the read identity with observePerms on that
// repo. Refs are sorted and unique.
//
// scope.repos are patterns (internal/patterns): an exact "owner/name" is used as is; a glob
// or regex is matched against the repos the read identity's App is installed on, in the
// pattern's owner when that part is exact, else in the Provider's scope.orgs. An empty
// scope.repos means every repo in scope.orgs. scope.exclude removes repos, and a Provider
// scope.orgs bounds everything. Channels and branches don't apply to GitHub objects.
func (c *Conn) List(ctx context.Context, scope v1alpha1.Scope) ([]v1alpha1.ObjectRef, error) {
	out, err := c.listScope(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("github %s: list: %w", c.name, err)
	}
	return out, nil
}

func (c *Conn) listScope(ctx context.Context, scope v1alpha1.Scope) ([]v1alpha1.ObjectRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repos, err := c.scopeRepos(ctx, scope)
	if err != nil {
		return nil, err
	}
	since := c.clock.Now().Add(-ListWindow).UTC().Format(time.DateOnly)
	seen := map[string]bool{}
	var out []v1alpha1.ObjectRef
	for _, repo := range repos {
		tok, err := c.repoToken(ctx, repo, observePerms)
		if errors.Is(err, provider.ErrNotFound) {
			c.log.WarnContext(ctx, "repo in scope not found or the App isn't installed on it: skipped", "repo", repo)
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, q := range []string{
			fmt.Sprintf("repo:%s is:open updated:>=%s", repo, since),
			fmt.Sprintf("repo:%s %q in:comments updated:>=%s", repo, MarkerPhrase, since),
		} {
			refs, err := c.search(ctx, tok, repo, q)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				if !seen[r.Ref] {
					seen[r.Ref] = true
					out = append(out, r)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b v1alpha1.ObjectRef) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// search runs one issue search in repo, page by page.
func (c *Conn) search(ctx context.Context, tok, repo, q string) ([]v1alpha1.ObjectRef, error) {
	query := url.Values{"q": {q}, "per_page": {strconv.Itoa(perPage)}}
	var out []v1alpha1.ObjectRef
	for page := 0; page < maxSearchPages; page++ {
		var res struct {
			TotalCount int  `json:"total_count"`
			Incomplete bool `json:"incomplete_results"`
			Items      []struct {
				Number      int             `json:"number"`
				PullRequest json.RawMessage `json:"pull_request"`
			} `json:"items"`
		}
		h, err := c.minter.Get(ctx, tok, "search/issues", query, &res)
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", repo, err)
		}
		if res.Incomplete {
			c.log.WarnContext(ctx, "search results incomplete", "repo", repo)
		}
		for _, it := range res.Items {
			if it.Number <= 0 {
				continue
			}
			kind := provider.KindIssue
			if len(it.PullRequest) > 0 && string(it.PullRequest) != "null" {
				kind = provider.KindPR
			}
			out = append(out, c.ref(kind, repo+"#"+strconv.Itoa(it.Number)))
		}
		next, ok := tokens.NextPage(h)
		if !ok {
			return out, nil
		}
		query = next
	}
	c.log.WarnContext(ctx, "search has more results than are read", "repo", repo, "pages", maxSearchPages)
	return out, nil
}

// scopeRepos resolves scope.repos to repo full names, sorted and unique.
func (c *Conn) scopeRepos(ctx context.Context, scope v1alpha1.Scope) ([]string, error) {
	var exclude patterns.Pattern
	if len(scope.Exclude) > 0 {
		p, err := patterns.Compile(scope.Exclude)
		if err != nil {
			return nil, fmt.Errorf("scope.exclude: %w: %w", err, provider.ErrInvalid)
		}
		exclude = p
	}
	inScope := func(repo string) bool {
		owner, _, _ := strings.Cut(repo, "/")
		if len(c.orgs) > 0 && !containsFold(c.orgs, owner) {
			return false
		}
		return exclude == nil || !exclude.Match(repo)
	}
	entries := scope.Repos
	if len(entries) == 0 {
		entries = []string{"*"}
	}
	var out []string
	installed := map[string][]string{} // by owner (lower-case)
	for _, e := range entries {
		if literalRepo(e) {
			if inScope(e) {
				out = append(out, e)
			}
			continue
		}
		pat, err := patterns.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("scope.repos: %w: %w", err, provider.ErrInvalid)
		}
		owners := c.orgs
		if owner, _, ok := strings.Cut(e, "/"); ok && !strings.HasPrefix(e, patterns.RegexPrefix) && !strings.ContainsAny(owner, `*?{}\`) {
			owners = []string{owner}
		}
		if len(owners) == 0 {
			return nil, fmt.Errorf("scope.repos %q: no owner to list repos in (name it, or set the Provider's scope.orgs): %w", e, provider.ErrInvalid)
		}
		for _, owner := range owners {
			key := strings.ToLower(owner)
			repos, ok := installed[key]
			if !ok {
				repos, err = c.minter.InstallationRepos(ctx, c.readIdentity, owner)
				c.record(c.readIdentity, err)
				if err != nil {
					return nil, fmt.Errorf("scope.repos %q: %w", e, err)
				}
				installed[key] = repos
			}
			for _, r := range repos {
				if pat.Match(r) && inScope(r) {
					out = append(out, r)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// literalRepo reports whether a scope entry names one repo ("owner/name", no pattern).
func literalRepo(e string) bool {
	return !strings.HasPrefix(e, patterns.RegexPrefix) && !strings.ContainsAny(e, `*?{}\`) && validRepo(e)
}

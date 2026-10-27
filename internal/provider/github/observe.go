package github

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
)

// KindRepo is the kind of a work item's link to a whole repo (an event without an object).
const KindRepo = "repo"

// maxFollowPages bounds the follow-up pages Observe reads of one connection (comments, review
// threads): 100 per page.
const maxFollowPages = 10

// observePerms are the read identity's permissions for Observe, exactly what observeQuery
// reads: the issue or the PR with its reviews and threads (issues, pull_requests), the
// commits it reaches (the head, the merge commit, each review's commit: contents) and the head
// commit's check suites (checks). List, ReadContent and Project's reads share the set, so one
// cached token per repo serves them all. No statuses: the head's statusCheckRollup also
// covers commit statuses, which the factory's Apps aren't granted (see checksOf).
var observePerms = map[string]string{"issues": "read", "pull_requests": "read", "contents": "read", "checks": "read"}

// observeQuery reads one issue or PR (github-provider.md §Observe): whichever the number is,
// so a ref of the wrong kind is told apart from a missing one.
const observeQuery = `query Observe($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    nameWithOwner
    defaultBranchRef { name }
    issueOrPullRequest(number: $number) {
      __typename
      ... on Issue {
        id
        number
        state
        updatedAt
        author { __typename login }
        labels(first: 100) { nodes { name } }
        subIssues(first: 100) { nodes { number repository { nameWithOwner } } }
        closedByPullRequestsReferences(first: 50, includeClosedPrs: true) { nodes { number repository { nameWithOwner } } }
        comments(first: 100) { totalCount pageInfo { hasNextPage endCursor } nodes { databaseId author { __typename login } body } }
      }
      ... on PullRequest {
        id
        number
        state
        isDraft
        updatedAt
        headRefOid
        baseRefName
        merged
        mergedAt
        mergeCommit { oid }
        author { __typename login }
        labels(first: 100) { nodes { name } }
        reviews(last: 100) { nodes { author { __typename login } state commit { oid } } }
        reviewThreads(first: 100) { pageInfo { hasNextPage endCursor } nodes { isResolved } }
        commits(last: 1) { nodes { commit { oid checkSuites(first: 100) { nodes { status conclusion checkRuns(first: 1) { totalCount } } } } } }
        comments(first: 100) { totalCount pageInfo { hasNextPage endCursor } nodes { databaseId author { __typename login } body } }
      }
    }
  }
}`

// commentsQuery reads the next page of an issue's or a PR's comments.
const commentsQuery = `query ObserveComments($id: ID!, $after: String!) {
  node(id: $id) {
    ... on Issue { comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { databaseId author { __typename login } body } } }
    ... on PullRequest { comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { databaseId author { __typename login } body } } }
  }
}`

// threadsQuery reads the next page of a PR's review threads.
const threadsQuery = `query ObserveThreads($id: ID!, $after: String!) {
  node(id: $id) {
    ... on PullRequest { reviewThreads(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { isResolved } } }
  }
}`

// repoQuery reads a repo, for a work item linked to a whole repo.
const repoQuery = `query ObserveRepo($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) { nameWithOwner isArchived updatedAt }
}`

// The GraphQL response shapes.
type (
	gqlActor struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	}
	gqlPageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	}
	gqlNumbered struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	gqlComment struct {
		DatabaseID int64     `json:"databaseId"`
		Author     *gqlActor `json:"author"`
		Body       string    `json:"body"`
	}
	gqlComments struct {
		TotalCount int          `json:"totalCount"`
		PageInfo   gqlPageInfo  `json:"pageInfo"`
		Nodes      []gqlComment `json:"nodes"`
	}
	gqlThreads struct {
		PageInfo gqlPageInfo `json:"pageInfo"`
		Nodes    []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	}
	gqlObject struct {
		Typename  string    `json:"__typename"`
		ID        string    `json:"id"`
		Number    int       `json:"number"`
		State     string    `json:"state"`
		UpdatedAt time.Time `json:"updatedAt"`
		Author    *gqlActor `json:"author"`
		Labels    struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
		Comments gqlComments `json:"comments"`
		// Issue.
		SubIssues struct {
			Nodes []gqlNumbered `json:"nodes"`
		} `json:"subIssues"`
		ClosedBy struct {
			Nodes []gqlNumbered `json:"nodes"`
		} `json:"closedByPullRequestsReferences"`
		// PullRequest.
		IsDraft     bool       `json:"isDraft"`
		HeadRefOid  string     `json:"headRefOid"`
		BaseRefName string     `json:"baseRefName"`
		Merged      bool       `json:"merged"`
		MergedAt    *time.Time `json:"mergedAt"`
		MergeCommit *struct {
			Oid string `json:"oid"`
		} `json:"mergeCommit"`
		Reviews struct {
			Nodes []struct {
				Author *gqlActor `json:"author"`
				State  string    `json:"state"`
				Commit *struct {
					Oid string `json:"oid"`
				} `json:"commit"`
			} `json:"nodes"`
		} `json:"reviews"`
		ReviewThreads gqlThreads `json:"reviewThreads"`
		Commits       struct {
			Nodes []struct {
				Commit struct {
					Oid         string `json:"oid"`
					CheckSuites *struct {
						Nodes []gqlCheckSuite `json:"nodes"`
					} `json:"checkSuites"`
				} `json:"commit"`
			} `json:"nodes"`
		} `json:"commits"`
	}
	// gqlCheckSuite is one check suite of a commit: an App's checks (GitHub Actions: one
	// workflow run's jobs).
	gqlCheckSuite struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		CheckRuns  struct {
			TotalCount int `json:"totalCount"`
		} `json:"checkRuns"`
	}
	gqlObserve struct {
		Repository *struct {
			NameWithOwner    string `json:"nameWithOwner"`
			DefaultBranchRef *struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			Object *gqlObject `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
)

// Observe implements provider.Conn: one GraphQL query per object (github-provider.md
// §Observe), as the read identity with observePerms on the object's repo, and follow-up pages
// for comments and review threads beyond the first 100 (up to maxFollowPages each).
//
//   - Issues: state, labels, author, sub-issues, the PRs that close it (linked PRs), the
//     comment count.
//   - PRs: draft, head SHA, base branch, labels, author, checks (the head commit's check
//     suites rolled up, see checksOf), approvals (the latest
//     approving, changes-requesting or dismissing review of each reviewer: the factory's
//     Apps map to their identity, "reviewer" or "author", anyone else to their login),
//     unresolved threads, merged-at and the merge commit; a merged PR also gets Deploy.
//   - Projections: the markers in comments written by the projecting App (comments by anyone
//     else that look like markers are ignored), one per work item (the oldest wins), sorted.
//   - A "repo" ref: open, or closed once archived. Releases and deployments are immutable
//     records: closed, with no call.
//
// A ref of the other kind (an issue ref naming a PR) is ErrNotFound, as a missing object is.
// No untrusted text is returned: comment bodies are read only to find the markers.
func (c *Conn) Observe(ctx context.Context, ref v1alpha1.ObjectRef) (provider.Observation, error) {
	obs, err := c.observe(ctx, ref)
	if err != nil {
		return provider.Observation{}, fmt.Errorf("github %s: observe %s: %w", c.name, ref.Ref, err)
	}
	return obs, nil
}

func (c *Conn) observe(ctx context.Context, ref v1alpha1.ObjectRef) (provider.Observation, error) {
	if err := ctx.Err(); err != nil {
		return provider.Observation{}, err
	}
	if ref.Provider != "" && ref.Provider != c.name {
		return provider.Observation{}, fmt.Errorf("object is on provider %q: %w", ref.Provider, provider.ErrInvalid)
	}
	ref.Provider = c.name
	switch ref.Kind {
	case KindRepo:
		return c.observeRepo(ctx, ref)
	case KindRelease, KindDeployment:
		if !validRepo(repoOf(ref.Ref)) || !strings.Contains(ref.Ref, "#") {
			return provider.Observation{}, fmt.Errorf("ref %q: want owner/name#…: %w", ref.Ref, provider.ErrInvalid)
		}
		return provider.Observation{Ref: ref, State: provider.StateClosed}, nil
	case "", provider.KindIssue, provider.KindPR:
	default:
		return provider.Observation{}, fmt.Errorf("kind %q: %w", ref.Kind, provider.ErrInvalid)
	}
	repo, number, err := splitRef(ref.Ref)
	if err != nil {
		return provider.Observation{}, err
	}
	owner, name, _ := strings.Cut(repo, "/")
	tok, err := c.repoToken(ctx, repo, observePerms)
	if err != nil {
		return provider.Observation{}, err
	}
	var data gqlObserve
	vars := map[string]any{"owner": owner, "name": name, "number": number}
	if err := c.minter.GraphQL(ctx, tok, "Observe", observeQuery, vars, &data); err != nil {
		return provider.Observation{}, err
	}
	if data.Repository == nil || data.Repository.Object == nil {
		return provider.Observation{}, fmt.Errorf("%s: %w", ref.Ref, provider.ErrNotFound)
	}
	o := data.Repository.Object
	kind := provider.KindIssue
	if o.Typename == "PullRequest" {
		kind = provider.KindPR
	}
	if ref.Kind != "" && ref.Kind != kind {
		return provider.Observation{}, fmt.Errorf("%s is a %s, not a %s: %w", ref.Ref, kind, ref.Kind, provider.ErrNotFound)
	}
	ref.Kind = kind
	if err := c.followComments(ctx, tok, o); err != nil {
		return provider.Observation{}, err
	}
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return provider.Observation{}, err
	}
	obs := provider.Observation{
		Ref:         ref,
		State:       objectState(o.State),
		Author:      c.actorOf(o.Author, bots),
		UpdatedAt:   o.UpdatedAt.UTC(),
		Projections: c.projectionsIn(ctx, o.Comments.Nodes, bots),
	}
	for _, l := range o.Labels.Nodes {
		obs.Labels = append(obs.Labels, l.Name)
	}
	if kind == provider.KindIssue {
		obs.Issue = &provider.IssueObservation{
			SubIssues: c.refsOf(provider.KindIssue, o.SubIssues.Nodes),
			LinkedPRs: c.refsOf(provider.KindPR, o.ClosedBy.Nodes),
			Comments:  o.Comments.TotalCount,
		}
		return obs, nil
	}
	if err := c.followThreads(ctx, tok, o); err != nil {
		return provider.Observation{}, err
	}
	obs.PR = prObservation(o, bots)
	if obs.State == provider.StateMerged && obs.PR.MergeCommit != "" && data.Repository.DefaultBranchRef != nil {
		if obs.Deploy, err = c.observeDeploy(ctx, repo, data.Repository.DefaultBranchRef.Name, obs.PR.MergeCommit); err != nil {
			return provider.Observation{}, fmt.Errorf("deployment: %w", err)
		}
	}
	return obs, nil
}

// observeRepo is a "repo" link: open unless archived.
func (c *Conn) observeRepo(ctx context.Context, ref v1alpha1.ObjectRef) (provider.Observation, error) {
	if !validRepo(ref.Ref) || strings.Contains(ref.Ref, "#") {
		return provider.Observation{}, fmt.Errorf("repo ref %q: want owner/name: %w", ref.Ref, provider.ErrInvalid)
	}
	tok, err := c.repoToken(ctx, ref.Ref, map[string]string{"metadata": "read"})
	if err != nil {
		return provider.Observation{}, err
	}
	owner, name, _ := strings.Cut(ref.Ref, "/")
	var data struct {
		Repository *struct {
			IsArchived bool      `json:"isArchived"`
			UpdatedAt  time.Time `json:"updatedAt"`
		} `json:"repository"`
	}
	if err := c.minter.GraphQL(ctx, tok, "ObserveRepo", repoQuery, map[string]any{"owner": owner, "name": name}, &data); err != nil {
		return provider.Observation{}, err
	}
	if data.Repository == nil {
		return provider.Observation{}, fmt.Errorf("%s: %w", ref.Ref, provider.ErrNotFound)
	}
	obs := provider.Observation{Ref: ref, State: provider.StateOpen, UpdatedAt: data.Repository.UpdatedAt.UTC()}
	if data.Repository.IsArchived {
		obs.State = provider.StateClosed
	}
	return obs, nil
}

// followComments reads the comments past the first page into o.
func (c *Conn) followComments(ctx context.Context, tok string, o *gqlObject) error {
	for i := 0; o.Comments.PageInfo.HasNextPage && i < maxFollowPages; i++ {
		var data struct {
			Node *struct {
				Comments gqlComments `json:"comments"`
			} `json:"node"`
		}
		vars := map[string]any{"id": o.ID, "after": o.Comments.PageInfo.EndCursor}
		if err := c.minter.GraphQL(ctx, tok, "ObserveComments", commentsQuery, vars, &data); err != nil {
			return err
		}
		if data.Node == nil {
			return fmt.Errorf("comments of %s: %w", o.ID, provider.ErrNotFound)
		}
		o.Comments.Nodes = append(o.Comments.Nodes, data.Node.Comments.Nodes...)
		o.Comments.PageInfo = data.Node.Comments.PageInfo
	}
	if o.Comments.PageInfo.HasNextPage {
		c.log.WarnContext(ctx, "comments beyond the pages read; markers there are not seen", "object", o.ID, "pages", maxFollowPages+1)
	}
	return nil
}

// followThreads reads the review threads past the first page into o.
func (c *Conn) followThreads(ctx context.Context, tok string, o *gqlObject) error {
	for i := 0; o.ReviewThreads.PageInfo.HasNextPage && i < maxFollowPages; i++ {
		var data struct {
			Node *struct {
				ReviewThreads gqlThreads `json:"reviewThreads"`
			} `json:"node"`
		}
		vars := map[string]any{"id": o.ID, "after": o.ReviewThreads.PageInfo.EndCursor}
		if err := c.minter.GraphQL(ctx, tok, "ObserveThreads", threadsQuery, vars, &data); err != nil {
			return err
		}
		if data.Node == nil {
			return fmt.Errorf("review threads of %s: %w", o.ID, provider.ErrNotFound)
		}
		o.ReviewThreads.Nodes = append(o.ReviewThreads.Nodes, data.Node.ReviewThreads.Nodes...)
		o.ReviewThreads.PageInfo = data.Node.ReviewThreads.PageInfo
	}
	return nil
}

// prObservation is the PR part of an observation.
func prObservation(o *gqlObject, bots map[string]string) *provider.PRObservation {
	pr := &provider.PRObservation{Draft: o.IsDraft, Head: o.HeadRefOid, Base: o.BaseRefName}
	if n := len(o.Commits.Nodes); n > 0 {
		if cs := o.Commits.Nodes[n-1].Commit.CheckSuites; cs != nil {
			pr.Checks = checksOf(cs.Nodes)
		}
	}
	// The latest opinion of each reviewer: an approval stands until the same reviewer requests
	// changes or it is dismissed; comments don't change it.
	type opinion struct {
		state, commit string
	}
	latest := map[string]opinion{}
	var order []string
	for _, r := range o.Reviews.Nodes {
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default:
			continue
		}
		who := reviewerIdentity(r.Author, bots)
		if who == "" {
			continue
		}
		if _, seen := latest[who]; !seen {
			order = append(order, who)
		}
		op := opinion{state: r.State}
		if r.Commit != nil {
			op.commit = r.Commit.Oid
		}
		latest[who] = op
	}
	for _, who := range order {
		if op := latest[who]; op.state == "APPROVED" {
			pr.ApprovedBy = append(pr.ApprovedBy, provider.Approval{Identity: who, Commit: op.commit})
		}
	}
	slices.SortFunc(pr.ApprovedBy, func(a, b provider.Approval) int {
		return cmp.Or(strings.Compare(a.Identity, b.Identity), strings.Compare(a.Commit, b.Commit))
	})
	for _, t := range o.ReviewThreads.Nodes {
		if !t.IsResolved {
			pr.UnresolvedThreads++
		}
	}
	if o.MergedAt != nil {
		pr.MergedAt = o.MergedAt.UTC()
	}
	if o.MergeCommit != nil && (o.Merged || o.State == "MERGED") {
		pr.MergeCommit = o.MergeCommit.Oid
	}
	return pr
}

// checksOf rolls the head commit's check suites up into checks{state, settled}, as GitHub's
// combined state does: "failure" once a suite concluded failing (failure, timed out,
// cancelled, action required, startup failure, stale), else "pending" while one hasn't
// concluded, else "success" (success, neutral, skipped); settled for success and failure.
// Suites without check runs don't count: GitHub leaves one queued for every App that could
// run checks (Actions with no workflow for the event), and they never complete. No suite
// with runs leaves checks empty. The first 100 suites are read (one page; a commit has one per
// App and workflow run).
//
// Check suites are what checks: read reaches, and what pr.checks.completed (check_suite
// webhooks) reports; commit statuses (the statuses API) aren't read: the App isn't granted
// them, so a status-only CI doesn't count here (branch protection still gates the merge on
// required statuses).
func checksOf(suites []gqlCheckSuite) provider.Checks {
	var counted, failed, pending bool
	for _, s := range suites {
		if s.CheckRuns.TotalCount == 0 {
			continue
		}
		counted = true
		if s.Status != "COMPLETED" {
			pending = true
			continue
		}
		switch s.Conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			failed = true
		default:
			pending = true // completed without a conclusion GitHub has documented
		}
	}
	switch {
	case !counted:
		return provider.Checks{}
	case failed:
		return provider.Checks{State: "failure", Settled: true}
	case pending:
		return provider.Checks{State: "pending"}
	}
	return provider.Checks{State: "success", Settled: true}
}

// objectState maps an issue's or PR's GraphQL state.
func objectState(s string) provider.ObjectState {
	switch s {
	case "MERGED":
		return provider.StateMerged
	case "CLOSED":
		return provider.StateClosed
	}
	return provider.StateOpen
}

// restLogin is a GraphQL actor's login as REST and webhooks spell it: an App's bot account is
// "<slug>[bot]" there and "<slug>" in GraphQL.
func restLogin(a *gqlActor) string {
	if a == nil || a.Login == "" {
		return ""
	}
	if a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
		return a.Login + "[bot]"
	}
	return a.Login
}

// actorOf is who opened the object: "<provider>:<login>", isBot, and isFactory with the
// identity for one of the factory's Apps. nil for a deleted account.
func (c *Conn) actorOf(a *gqlActor, bots map[string]string) *v1alpha1.Actor {
	login := restLogin(a)
	if login == "" {
		return nil
	}
	actor := &v1alpha1.Actor{ID: c.name + ":" + login, IsBot: a.Typename == "Bot"}
	if id, ok := bots[strings.ToLower(login)]; ok {
		actor.IsFactory, actor.Identity, actor.IsBot = true, id, true
	}
	return actor
}

// reviewerIdentity is who a review counts for: the factory identity of one of its Apps, else
// the reviewer's login; "" for a deleted account.
func reviewerIdentity(a *gqlActor, bots map[string]string) string {
	login := restLogin(a)
	if id, ok := bots[strings.ToLower(login)]; ok {
		return id
	}
	return login
}

// refsOf are the objects of nodes, as refs of kind.
func (c *Conn) refsOf(kind string, nodes []gqlNumbered) []v1alpha1.ObjectRef {
	var out []v1alpha1.ObjectRef
	for _, n := range nodes {
		if n.Number > 0 && validRepo(n.Repository.NameWithOwner) {
			out = append(out, c.ref(kind, n.Repository.NameWithOwner+"#"+strconv.Itoa(n.Number)))
		}
	}
	return out
}

// projectionsIn reads the markers among comments: only those written by the projecting App
// count (anyone can post a comment that looks like one), one per work item (the oldest
// comment wins), sorted by work item ID.
func (c *Conn) projectionsIn(ctx context.Context, comments []gqlComment, bots map[string]string) []provider.Projection {
	ms := make([]markerComment, 0, len(comments))
	for _, cm := range comments {
		ms = append(ms, markerComment{id: cm.DatabaseID, login: restLogin(cm.Author), body: cm.Body})
	}
	var out []provider.Projection
	for _, m := range c.markers(ctx, ms, bots) {
		out = append(out, m.p)
	}
	return out
}

// markerComment is a comment that may be a marker, from GraphQL or REST.
type markerComment struct {
	id    int64
	login string
	body  string
}

// parsedMarker is a marker comment read back.
type parsedMarker struct {
	id   int64
	body string
	p    provider.Projection
}

// markers returns the projection markers among comments written by the projecting App, one
// per work item (the oldest comment), sorted by work item ID.
func (c *Conn) markers(ctx context.Context, comments []markerComment, bots map[string]string) []parsedMarker {
	byID := map[string]parsedMarker{}
	for _, cm := range comments {
		if !isMarker(cm.body) || bots[strings.ToLower(cm.login)] != c.projectIdentity() {
			continue
		}
		p, ok, err := parseMarker(cm.body)
		if !ok {
			if err != nil {
				c.log.WarnContext(ctx, "projection marker not read", "comment", cm.id, "error", err)
			}
			continue
		}
		if prev, dup := byID[p.WorkItemID]; dup && prev.id < cm.id {
			continue
		}
		byID[p.WorkItemID] = parsedMarker{id: cm.id, body: cm.body, p: p}
	}
	out := make([]parsedMarker, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b parsedMarker) int { return strings.Compare(a.p.WorkItemID, b.p.WorkItemID) })
	return out
}

// splitRef splits "owner/name#42".
func splitRef(ref string) (repo string, number int, err error) {
	repo, num, ok := strings.Cut(ref, "#")
	if ok && validRepo(repo) {
		if n, err := strconv.Atoi(num); err == nil && n > 0 {
			return repo, n, nil
		}
	}
	return "", 0, fmt.Errorf("ref %q: want owner/name#<number>: %w", ref, provider.ErrInvalid)
}

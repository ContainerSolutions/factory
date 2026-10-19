package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/tokens"
)

// The PR, review and thread ops. Their token is pull_requests: write on the target repo (and
// contents: write for a merge), never issues.

func prWrite() map[string]string { return map[string]string{"pull_requests": "write"} }

// mergePerms are what merging needs: contents (the base branch moves) and pull requests.
func mergePerms() map[string]string {
	return map[string]string{"contents": "write", "pull_requests": "write"}
}

// ghPull is a PR as the pulls API shows it.
type ghPull struct {
	Number  int       `json:"number"`
	State   string    `json:"state"`
	Merged  bool      `json:"merged"`
	Title   string    `json:"title"`
	Body    *string   `json:"body"`
	User    ghUser    `json:"user"`
	HTMLURL string    `json:"html_url"`
	Labels  []ghLabel `json:"labels"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p *ghPull) body() string {
	if p.Body == nil {
		return ""
	}
	return *p.Body
}

func (c *Conn) pull(ctx context.Context, tok string, e *effect) (*ghPull, error) {
	var p ghPull
	if _, err := c.minter.Get(ctx, tok, e.pullPath(), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// applyPROpen opens a PR in the target repo from args.head onto args.base (args.title,
// args.body, args.draft, args.labels), unless the App already has an open PR for that head:
// a head has one open PR on GitHub, so that PR is the earlier write. An open PR for the head
// by anyone else is ErrInvalid.
func (c *Conn) applyPROpen(ctx context.Context, e *effect) (provider.EffectResult, error) {
	head, base, title := stringArg(e.req.Args, "head"), stringArg(e.req.Args, "base"), stringArg(e.req.Args, "title")
	if head == "" || base == "" || strings.TrimSpace(title) == "" {
		return provider.EffectResult{}, fmt.Errorf("args.head, args.base and args.title are required: %w", provider.ErrInvalid)
	}
	body, err := markBody(stringArg(e.req.Args, "body"), e.key())
	if err != nil {
		return provider.EffectResult{}, err
	}
	tok, err := c.token(ctx, e, prWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	qualified := head
	if !strings.Contains(head, ":") {
		owner, _, _ := strings.Cut(e.repo, "/")
		qualified = owner + ":" + head
	}
	var open []ghPull
	if _, err := c.minter.Get(ctx, tok, "repos/"+e.repo+"/pulls", url.Values{"state": {"open"}, "head": {qualified}, "per_page": {"10"}}, &open); err != nil {
		return provider.EffectResult{}, fmt.Errorf("look up the PR: %w", err)
	}
	var p *ghPull
	replayed := false
	for i := range open {
		if !sameLogin(open[i].User.Login, e.bot) {
			return provider.EffectResult{}, fmt.Errorf("an open PR by %s already uses head %s: %w", open[i].User.Login, head, provider.ErrInvalid)
		}
		p, replayed = &open[i], true
	}
	if p == nil {
		in := map[string]any{"title": title, "head": head, "base": base, "body": body, "draft": boolArg(e.req.Args, "draft")}
		var created ghPull
		if _, err := c.minter.Do(ctx, tok, http.MethodPost, "repos/"+e.repo+"/pulls", nil, in, http.StatusCreated, &created); err != nil {
			return provider.EffectResult{}, fmt.Errorf("open the PR: %w", err)
		}
		p = &created
	}
	if labels := stringsArg(e.req.Args, "labels"); len(labels) > 0 {
		pe := *e
		pe.number = p.Number
		if _, err := c.addLabels(ctx, tok, &pe, &ghObject{Labels: p.Labels}, labels); err != nil {
			return provider.EffectResult{}, err
		}
	}
	return c.result(provider.KindPR, e.repo, p.Number, p.HTMLURL, replayed), nil
}

// effectMarkers matches the effect markers in a body.
var effectMarkers = regexp.MustCompile(`<!-- factory:effect [^ ]* -->`)

// applyPRUpdate sets the PR's args.title and args.body (at least one), unless they are
// already so. The effect markers in the PR's body (pr.open's among them) are kept.
func (c *Conn) applyPRUpdate(ctx context.Context, e *effect) (provider.EffectResult, error) {
	title, hasTitle := e.req.Args["title"].(string)
	body, hasBody := e.req.Args["body"].(string)
	if (!hasTitle || strings.TrimSpace(title) == "") && !hasBody {
		return provider.EffectResult{}, fmt.Errorf("args.title or args.body is required: %w", provider.ErrInvalid)
	}
	tok, err := c.token(ctx, e, prWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	p, err := c.pull(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	in := map[string]string{}
	if hasTitle && strings.TrimSpace(title) != "" && title != p.Title {
		in["title"] = title
	}
	if hasBody {
		next := strings.TrimRight(body, "\n")
		if kept := effectMarkers.FindAllString(p.body(), -1); len(kept) > 0 {
			if next != "" {
				next += "\n\n"
			}
			next += strings.Join(kept, "\n")
		}
		if len(next) > maxBodyBytes {
			return provider.EffectResult{}, fmt.Errorf("body is %d bytes (max %d): %w", len(next), maxBodyBytes, provider.ErrInvalid)
		}
		if next != p.body() {
			in["body"] = next
		}
	}
	if len(in) > 0 {
		if _, err := c.minter.Do(ctx, tok, http.MethodPatch, e.pullPath(), nil, in, http.StatusOK, nil); err != nil {
			return provider.EffectResult{}, fmt.Errorf("update the PR: %w", err)
		}
	}
	return c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, len(in) == 0), nil
}

// applyPRRetarget sets the PR's base to args.base, unless it is already.
func (c *Conn) applyPRRetarget(ctx context.Context, e *effect) (provider.EffectResult, error) {
	base := stringArg(e.req.Args, "base")
	if base == "" {
		return provider.EffectResult{}, fmt.Errorf("args.base is required: %w", provider.ErrInvalid)
	}
	tok, err := c.token(ctx, e, prWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	p, err := c.pull(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	if p.Base.Ref == base {
		return c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, true), nil
	}
	if _, err := c.minter.Do(ctx, tok, http.MethodPatch, e.pullPath(), nil, map[string]string{"base": base}, http.StatusOK, nil); err != nil {
		return provider.EffectResult{}, fmt.Errorf("retarget the PR: %w", err)
	}
	return c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, false), nil
}

// mergeMethods are the merge methods in the order pr.merge prefers them among those the repo
// allows: squash keeps one commit per reviewed PR (and a stack's next PR is rebased anyway).
var mergeMethods = []struct{ method, setting string }{
	{"squash", "allow_squash_merge"}, {"merge", "allow_merge_commit"}, {"rebase", "allow_rebase_merge"},
}

// mergeMethod is the repo's merge method: the first of mergeMethods its settings allow.
func (c *Conn) mergeMethod(ctx context.Context, tok, repo string) (string, error) {
	var settings map[string]any
	if _, err := c.minter.Get(ctx, tok, "repos/"+repo, nil, &settings); err != nil {
		return "", fmt.Errorf("read the repo's merge settings: %w", err)
	}
	for _, m := range mergeMethods {
		if allowed, _ := settings[m.setting].(bool); allowed {
			return m.method, nil
		}
	}
	return "", fmt.Errorf("%s allows no merge method: %w", repo, provider.ErrInvalid)
}

// reviewBlocked reports whether a 405 from the merge endpoint is a review requirement
// (branch protection's "At least 1 approving review is required…", a ruleset's "Review
// required", a code owner review).
func reviewBlocked(msg string) bool { return strings.Contains(strings.ToLower(msg), "review") }

// applyPRMerge merges the PR with the repo's merge method at args.sha (default: its current
// head). Already merged is a replay. When review requirements block the merge (the reviewer
// App's approval may not count, Q5), args.mergeLabel, when set, is added instead and the
// outcome is OutcomeLabelledForMerge (a replay when the label was already there); without it
// the merge is refused with ErrReviewRequired. Other refusals: ErrMergeBlocked (405, not
// mergeable) and ErrHeadChanged (409, the head moved past args.sha).
func (c *Conn) applyPRMerge(ctx context.Context, e *effect) (provider.EffectResult, error) {
	label := stringArg(e.req.Args, "mergeLabel")
	tok, err := c.token(ctx, e, mergePerms())
	if err != nil {
		return provider.EffectResult{}, err
	}
	p, err := c.pull(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	switch {
	case p.Merged:
		return c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, true), nil
	case p.State != "open":
		return provider.EffectResult{}, &MergeError{Reason: "the PR is closed"}
	}
	method, err := c.mergeMethod(ctx, tok, e.repo)
	if err != nil {
		return provider.EffectResult{}, err
	}
	sha := stringArg(e.req.Args, "sha")
	if sha == "" {
		sha = p.Head.SHA
	}
	_, err = c.minter.Do(ctx, tok, http.MethodPut, e.pullPath()+"/merge", nil, map[string]string{"merge_method": method, "sha": sha}, http.StatusOK, nil)
	var apiErr *tokens.APIError
	switch {
	case err == nil:
		return c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, false), nil
	case !errors.As(err, &apiErr):
		return provider.EffectResult{}, fmt.Errorf("merge: %w", err)
	case apiErr.StatusCode == http.StatusConflict:
		return provider.EffectResult{}, &MergeError{Status: apiErr.StatusCode, Reason: "the head moved past " + sha, Err: apiErr}
	case apiErr.StatusCode != http.StatusMethodNotAllowed:
		return provider.EffectResult{}, fmt.Errorf("merge: %w", err)
	case strings.Contains(strings.ToLower(apiErr.Message), "already merged"):
		return c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, true), nil
	case !reviewBlocked(apiErr.Message):
		return provider.EffectResult{}, &MergeError{Status: apiErr.StatusCode, Reason: "not mergeable", Err: apiErr}
	case label == "":
		return provider.EffectResult{}, &MergeError{Status: apiErr.StatusCode, ReviewRequired: true, Reason: "review required and no args.mergeLabel to fall back to", Err: apiErr}
	}
	added, err := c.addLabels(ctx, tok, e, &ghObject{Labels: p.Labels}, []string{label})
	if err != nil {
		return provider.EffectResult{}, fmt.Errorf("merge blocked by review requirements: %w", err)
	}
	res := c.result(provider.KindPR, e.repo, e.number, p.HTMLURL, !added)
	res.Outcome = OutcomeLabelledForMerge
	return res, nil
}

// reviewEvents are the review ops' events.
var reviewEvents = map[string]string{
	"review.comment": "COMMENT", "review.request-changes": "REQUEST_CHANGES", "review.approve": "APPROVE",
}

// applyReview submits a review (review.comment, review.request-changes, review.approve) at
// args.commit (default: the PR's head) with args.body (required but for an approval) carrying
// the key's marker and args.comments inline ({path, line, body, side?, startLine?,
// startSide?}), unless the App's review with the key exists.
func (c *Conn) applyReview(ctx context.Context, e *effect) (provider.EffectResult, error) {
	event := reviewEvents[string(e.req.Op)]
	body := stringArg(e.req.Args, "body")
	if event != "APPROVE" && strings.TrimSpace(body) == "" {
		return provider.EffectResult{}, fmt.Errorf("args.body is required: %w", provider.ErrInvalid)
	}
	comments, err := reviewComments(e.req.Args["comments"])
	if err != nil {
		return provider.EffectResult{}, err
	}
	marked, err := markBody(body, e.key())
	if err != nil {
		return provider.EffectResult{}, err
	}
	tok, err := c.token(ctx, e, prWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	found, err := c.findWritten(ctx, tok, e, e.pullPath()+"/reviews", nil, maxReviewPages, nil)
	if err != nil {
		return provider.EffectResult{}, fmt.Errorf("look up the review: %w", err)
	}
	if found != nil {
		return c.result(provider.KindPR, e.repo, e.number, found.HTMLURL, true), nil
	}
	commit := stringArg(e.req.Args, "commit")
	if commit == "" {
		p, err := c.pull(ctx, tok, e)
		if err != nil {
			return provider.EffectResult{}, err
		}
		commit = p.Head.SHA
	}
	in := map[string]any{"commit_id": commit, "body": marked, "event": event}
	if len(comments) > 0 {
		in["comments"] = comments
	}
	var created ghWritten
	if _, err := c.minter.Do(ctx, tok, http.MethodPost, e.pullPath()+"/reviews", nil, in, http.StatusOK, &created); err != nil {
		return provider.EffectResult{}, fmt.Errorf("submit the review: %w", err)
	}
	return c.result(provider.KindPR, e.repo, e.number, created.HTMLURL, false), nil
}

// reviewComments reads args.comments: each {path, line, body} with optional side, startLine
// and startSide, in GitHub's field names.
func reviewComments(v any) ([]map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("args.comments: want a list: %w", provider.ErrInvalid)
	}
	out := make([]map[string]any, 0, len(list))
	for i, x := range list {
		m, ok := x.(map[string]any)
		path, body := stringArg(m, "path"), stringArg(m, "body")
		line, lok := m["line"].(float64)
		if !ok || path == "" || body == "" || !lok || line < 1 {
			return nil, fmt.Errorf("args.comments[%d]: want {path, line, body}: %w", i, provider.ErrInvalid)
		}
		cm := map[string]any{"path": path, "line": int(line), "body": body}
		if s := stringArg(m, "side"); s != "" {
			cm["side"] = s
		}
		if sl, ok := m["startLine"].(float64); ok && sl >= 1 {
			cm["start_line"] = int(sl)
		}
		if s := stringArg(m, "startSide"); s != "" {
			cm["start_side"] = s
		}
		out = append(out, cm)
	}
	return out, nil
}

// threadQuery reads a review thread: its PR (to check it is the target's) and its last 100
// comments (for the reply's marker).
const threadQuery = `query ApplyThread($thread: ID!) {
  node(id: $thread) {
    __typename
    ... on PullRequestReviewThread {
      isResolved
      pullRequest { number url repository { nameWithOwner } }
      comments(last: 100) { nodes { author { __typename login } body url } }
    }
  }
}`

const threadReplyMutation = `mutation ApplyThreadReply($thread: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $thread, body: $body}) { comment { url } }
}`

const resolveThreadMutation = `mutation ApplyResolveThread($thread: ID!) {
  resolveReviewThread(input: {threadId: $thread}) { thread { isResolved } }
}`

type gqlThread struct {
	Typename    string `json:"__typename"`
	IsResolved  bool   `json:"isResolved"`
	PullRequest struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"pullRequest"`
	Comments struct {
		Nodes []struct {
			Author *gqlActor `json:"author"`
			Body   string    `json:"body"`
			URL    string    `json:"url"`
		} `json:"nodes"`
	} `json:"comments"`
}

// thread reads args.thread (a review thread's node ID) and checks it is on the target PR.
func (c *Conn) thread(ctx context.Context, tok string, e *effect) (string, *gqlThread, error) {
	id := stringArg(e.req.Args, "thread")
	if id == "" {
		return "", nil, fmt.Errorf("args.thread (the review thread's node ID) is required: %w", provider.ErrInvalid)
	}
	var data struct {
		Node *gqlThread `json:"node"`
	}
	if err := c.minter.GraphQL(ctx, tok, "ApplyThread", threadQuery, map[string]any{"thread": id}, &data); err != nil {
		return "", nil, fmt.Errorf("read the thread: %w", err)
	}
	t := data.Node
	switch {
	case t == nil || t.Typename != "PullRequestReviewThread":
		return "", nil, fmt.Errorf("thread %q: %w", id, provider.ErrNotFound)
	case !strings.EqualFold(t.PullRequest.Repository.NameWithOwner, e.repo) || t.PullRequest.Number != e.number:
		return "", nil, fmt.Errorf("thread %q is on %s#%d, not the target: %w", id, t.PullRequest.Repository.NameWithOwner, t.PullRequest.Number, provider.ErrInvalid)
	}
	return id, t, nil
}

// applyThreadReply replies args.body to the review thread args.thread, unless the App's reply
// with the key is among its last 100 comments.
func (c *Conn) applyThreadReply(ctx context.Context, e *effect) (provider.EffectResult, error) {
	body := stringArg(e.req.Args, "body")
	if strings.TrimSpace(body) == "" {
		return provider.EffectResult{}, fmt.Errorf("args.body is required: %w", provider.ErrInvalid)
	}
	marked, err := markBody(body, e.key())
	if err != nil {
		return provider.EffectResult{}, err
	}
	tok, err := c.token(ctx, e, prWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	id, t, err := c.thread(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	for _, cm := range t.Comments.Nodes {
		if sameLogin(restLogin(cm.Author), e.bot) && carries(cm.Body, e.key()) {
			return c.result(provider.KindPR, e.repo, e.number, cm.URL, true), nil
		}
	}
	var data struct {
		Reply struct {
			Comment struct {
				URL string `json:"url"`
			} `json:"comment"`
		} `json:"addPullRequestReviewThreadReply"`
	}
	if err := c.minter.GraphQL(ctx, tok, "ApplyThreadReply", threadReplyMutation, map[string]any{"thread": id, "body": marked}, &data); err != nil {
		return provider.EffectResult{}, fmt.Errorf("reply to the thread: %w", err)
	}
	return c.result(provider.KindPR, e.repo, e.number, data.Reply.Comment.URL, false), nil
}

// applyThreadResolve resolves the review thread args.thread, unless it is resolved.
func (c *Conn) applyThreadResolve(ctx context.Context, e *effect) (provider.EffectResult, error) {
	tok, err := c.token(ctx, e, prWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	id, t, err := c.thread(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	if t.IsResolved {
		return c.result(provider.KindPR, e.repo, e.number, t.PullRequest.URL, true), nil
	}
	var data struct {
		Resolve struct {
			Thread struct {
				IsResolved bool `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := c.minter.GraphQL(ctx, tok, "ApplyResolveThread", resolveThreadMutation, map[string]any{"thread": id}, &data); err != nil {
		return provider.EffectResult{}, fmt.Errorf("resolve the thread: %w", err)
	}
	if !data.Resolve.Thread.IsResolved {
		return provider.EffectResult{}, fmt.Errorf("resolve the thread: GitHub left it unresolved")
	}
	return c.result(provider.KindPR, e.repo, e.number, t.PullRequest.URL, false), nil
}

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

// Effects (github-provider.md §Effects). Every op acts as the identity the request names, with
// an installation token on the one target repo carrying only the permissions the op needs,
// and is idempotent by its key:
//
//   - what it creates (comments, issues, PRs, reviews, thread replies) carries a hidden marker
//     with the key, "<!-- factory:effect <key> -->", and is looked up first: a replay finds
//     the earlier write (by the same App, so a look-alike by anyone else doesn't count) and
//     returns it with Replayed;
//   - what it changes (labels, sub-issue links, state, base, title and body, merges, resolved
//     threads) is read first: when the object is already as asked, nothing is written and the
//     result has Replayed.
//
// A rate limit, a 5xx or a transport failure retries the whole op (lookup included, so a
// write GitHub made before failing is found rather than repeated), up to maxApplyTries times,
// waiting Retry-After or X-RateLimit-Reset when GitHub says, else an exponential backoff, on
// the injected clock. A wait longer than maxRetryWait, or the last failure, is returned as a
// *provider.RetryableError with GitHub's hint.

const (
	// maxApplyTries bounds the tries of one Apply.
	maxApplyTries = 5
	// firstBackoff is the first wait without a hint from GitHub; it doubles each try.
	firstBackoff = time.Second
	// maxRetryWait is the longest wait Apply sleeps; a longer one is the caller's (the
	// Effector answers 429 with Retry-After).
	maxRetryWait = time.Minute
	// maxBodyBytes is GitHub's limit on a comment, issue, PR or review body (65 536
	// characters; counted in bytes, which is stricter).
	maxBodyBytes = 65536
	// maxCreatedPages bounds the pages of the App's newest issues create-issue looks through.
	maxCreatedPages = 3
	// maxReviewPages bounds the pages of reviews a review op looks through.
	maxReviewPages = 10
	// ReviewerIdentity is the only identity the review ops act as.
	ReviewerIdentity = "reviewer"
	// OutcomeLabelledForMerge is pr.merge's outcome when review requirements blocked the merge
	// and the merge label was added instead (Q5).
	OutcomeLabelledForMerge = "labelled-for-merge"
)

// effectMarkerPrefix starts the hidden marker of what an effect created.
const effectMarkerPrefix = "<!-- factory:effect "

// effectMarker is the hidden marker of key; the key is percent-encoded as the projection
// marker's values are, so it can't end the comment.
func effectMarker(key string) string { return effectMarkerPrefix + escape(key) + markerEnd }

// markBody appends key's marker to body. A body GitHub would refuse as too long is ErrInvalid.
func markBody(body, key string) (string, error) {
	out := strings.TrimRight(body, "\n")
	if out != "" {
		out += "\n\n"
	}
	out += effectMarker(key)
	if len(out) > maxBodyBytes {
		return "", fmt.Errorf("body is %d bytes with its marker (max %d): %w", len(out), maxBodyBytes, provider.ErrInvalid)
	}
	return out, nil
}

// carries reports whether body has key's marker.
func carries(body, key string) bool { return strings.Contains(body, effectMarker(key)) }

// Errors of pr.merge, matched with errors.Is. Each is also provider.ErrInvalid: the provider
// refused the merge, and retrying the same request won't change that.
var (
	// ErrMergeBlocked is a merge GitHub refused (405): not mergeable, required checks, or
	// review requirements without a merge label to fall back to.
	ErrMergeBlocked = errors.New("github: merge blocked")
	// ErrReviewRequired is a merge blocked by review requirements (also ErrMergeBlocked).
	ErrReviewRequired = errors.New("github: merge blocked: review required")
	// ErrHeadChanged is a merge refused because the PR's head moved past the expected SHA
	// (409).
	ErrHeadChanged = errors.New("github: merge refused: head changed")
)

// MergeError is a refused merge: errors.Is matches ErrMergeBlocked, ErrReviewRequired or
// ErrHeadChanged, and provider.ErrInvalid. It unwraps to GitHub's *tokens.APIError, if any.
type MergeError struct {
	// Status is GitHub's status (405, 409), or zero when the adapter refused before merging.
	Status int
	// ReviewRequired is set when review requirements block the merge.
	ReviewRequired bool
	Reason         string
	Err            error
}

func (e *MergeError) Error() string {
	if e.Err != nil {
		return "merge refused: " + e.Reason + ": " + e.Err.Error()
	}
	return "merge refused: " + e.Reason
}

func (e *MergeError) Unwrap() error { return e.Err }

// Is maps the refusal to the package's and the provider's errors.
func (e *MergeError) Is(target error) bool {
	switch target {
	case provider.ErrInvalid:
		return true
	case ErrHeadChanged:
		return e.Status == http.StatusConflict
	case ErrMergeBlocked:
		return e.Status != http.StatusConflict
	case ErrReviewRequired:
		return e.ReviewRequired
	}
	return false
}

// effect is an EffectRequest resolved for applying.
type effect struct {
	req      provider.EffectRequest
	identity string
	// bot is the acting App's login ("<slug>[bot]"): what it wrote is what a lookup trusts.
	bot  string
	repo string
	// number is the target issue or PR; zero for the repo ops (create-issue, pr.open).
	number int
	// kind is the target's kind: the request's, else the op's (pr for the PR, review and
	// thread ops), else issue.
	kind string
}

func (e *effect) key() string { return e.req.IdempotencyKey }

// issuePath is the target in the issues API (issues and PRs alike).
func (e *effect) issuePath() string { return fmt.Sprintf("repos/%s/issues/%d", e.repo, e.number) }

// pullPath is the target in the pulls API.
func (e *effect) pullPath() string { return fmt.Sprintf("repos/%s/pulls/%d", e.repo, e.number) }

// opFunc applies one op once; Apply retries it.
type opFunc func(c *Conn, ctx context.Context, e *effect) (provider.EffectResult, error)

// effectOps are the API-side ops.
var effectOps = map[v1alpha1.EffectOp]opFunc{
	v1alpha1.OpComment:              (*Conn).applyComment,
	v1alpha1.OpPRComment:            (*Conn).applyComment,
	v1alpha1.OpLabelAdd:             (*Conn).applyLabelAdd,
	v1alpha1.OpLabelRemove:          (*Conn).applyLabelRemove,
	v1alpha1.OpCreateIssue:          (*Conn).applyCreateIssue,
	v1alpha1.OpSubIssueAdd:          (*Conn).applySubIssueAdd,
	v1alpha1.OpClose:                (*Conn).applyClose,
	v1alpha1.OpLinkDuplicate:        (*Conn).applyLinkDuplicate,
	v1alpha1.OpPROpen:               (*Conn).applyPROpen,
	v1alpha1.OpPRUpdate:             (*Conn).applyPRUpdate,
	v1alpha1.OpPRRetarget:           (*Conn).applyPRRetarget,
	v1alpha1.OpPRMerge:              (*Conn).applyPRMerge,
	v1alpha1.OpReviewComment:        (*Conn).applyReview,
	v1alpha1.OpReviewRequestChanges: (*Conn).applyReview,
	v1alpha1.OpReviewApprove:        (*Conn).applyReview,
	v1alpha1.OpThreadReply:          (*Conn).applyThreadReply,
	v1alpha1.OpThreadResolve:        (*Conn).applyThreadResolve,
}

// prOps act on a PR whatever the target's kind says.
var prOps = []v1alpha1.EffectOp{
	v1alpha1.OpPRComment, v1alpha1.OpPRUpdate, v1alpha1.OpPRRetarget, v1alpha1.OpPRMerge,
	v1alpha1.OpReviewComment, v1alpha1.OpReviewRequestChanges, v1alpha1.OpReviewApprove,
	v1alpha1.OpThreadReply, v1alpha1.OpThreadResolve,
}

// reviewOps act only as ReviewerIdentity.
var reviewOps = []v1alpha1.EffectOp{v1alpha1.OpReviewComment, v1alpha1.OpReviewRequestChanges, v1alpha1.OpReviewApprove}

// Apply implements provider.Conn: every API-side op of contracts.md §Effect ops, as the
// request's identity (default: the author identity), idempotent by the request's key (see
// above). Git-side ops are ErrUnsupported (the agent executes them), as are targets GitHub
// has no notion of (channels, groups). Errors: a missing object is ErrNotFound, a request
// GitHub or the adapter refuses ErrInvalid (a refused merge a *MergeError), and a rate limit
// or GitHub being unavailable, after the retries, a *provider.RetryableError.
func (c *Conn) Apply(ctx context.Context, req provider.EffectRequest) (provider.EffectResult, error) {
	var e *effect
	res, err := c.retry(ctx, req.Op, func() (provider.EffectResult, error) {
		var err error
		if e == nil { // resolving learns the Apps' logins (GET /app), so it is retried too
			if e, err = c.resolveEffect(ctx, req); err != nil {
				return provider.EffectResult{}, err
			}
		}
		return effectOps[req.Op](c, ctx, e)
	})
	if err != nil {
		return provider.EffectResult{}, fmt.Errorf("github %s: apply %s on %s: %w", c.name, req.Op, req.Target.Ref, err)
	}
	c.log.DebugContext(ctx, "effect applied", "op", req.Op, "ref", req.Target.Ref, "identity", e.identity,
		"replayed", res.Replayed, "outcome", res.Outcome)
	return res, nil
}

// resolveEffect checks the request and resolves its identity, repo, number and kind.
func (c *Conn) resolveEffect(ctx context.Context, req provider.EffectRequest) (*effect, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	side, known := req.Op.Side()
	switch {
	case !known:
		return nil, fmt.Errorf("unknown op %q: %w", req.Op, provider.ErrInvalid)
	case side != v1alpha1.SideAPI:
		return nil, fmt.Errorf("%s is a git-side op (the agent executes it): %w", req.Op, provider.ErrUnsupported)
	case req.IdempotencyKey == "":
		return nil, fmt.Errorf("no idempotency key: %w", provider.ErrInvalid)
	case req.Target.Provider != "" && req.Target.Provider != c.name:
		return nil, fmt.Errorf("target is on provider %q: %w", req.Target.Provider, provider.ErrInvalid)
	}
	e := &effect{req: req, identity: req.Identity}
	if e.identity == "" {
		e.identity = c.readIdentity
	}
	if !slices.Contains(c.minter.Identities(), e.identity) {
		return nil, fmt.Errorf("unknown identity %q: %w", e.identity, provider.ErrInvalid)
	}
	if slices.Contains(reviewOps, req.Op) && e.identity != ReviewerIdentity {
		return nil, fmt.Errorf("%s acts only as the %s identity, not %q: %w", req.Op, ReviewerIdentity, e.identity, provider.ErrInvalid)
	}
	switch req.Target.Kind {
	case "", provider.KindIssue, provider.KindPR, KindRepo:
	default:
		return nil, fmt.Errorf("a %s target: %w", req.Target.Kind, provider.ErrUnsupported)
	}
	if req.Op == v1alpha1.OpCreateIssue || req.Op == v1alpha1.OpPROpen {
		e.repo, _, _ = strings.Cut(req.Target.Ref, "#")
		if !validRepo(e.repo) {
			return nil, fmt.Errorf("target %q: want a repo, owner/name: %w", req.Target.Ref, provider.ErrInvalid)
		}
	} else {
		var err error
		if e.repo, e.number, err = splitRef(req.Target.Ref); err != nil {
			return nil, err
		}
		e.kind = req.Target.Kind
		if slices.Contains(prOps, req.Op) {
			if e.kind != "" && e.kind != provider.KindPR {
				return nil, fmt.Errorf("%s needs a PR, not a %s: %w", req.Op, e.kind, provider.ErrInvalid)
			}
			e.kind = provider.KindPR
		}
		if e.kind == "" || e.kind == KindRepo {
			e.kind = provider.KindIssue
		}
	}
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return nil, err
	}
	for login, id := range bots {
		if id == e.identity {
			e.bot = login
		}
	}
	if e.bot == "" {
		return nil, fmt.Errorf("identity %q: its App's login is unknown", e.identity)
	}
	return e, nil
}

// retry runs f until it succeeds, fails for good, or maxApplyTries retryable failures. It
// waits GitHub's hint (Retry-After, X-RateLimit-Reset), else firstBackoff doubling, on the
// clock; a hint over maxRetryWait isn't waited for but returned.
func (c *Conn) retry(ctx context.Context, op v1alpha1.EffectOp, f func() (provider.EffectResult, error)) (provider.EffectResult, error) {
	backoff := firstBackoff
	for try := 1; ; try++ {
		res, err := f()
		var re *provider.RetryableError
		if err == nil || !errors.As(err, &re) || try == maxApplyTries || ctx.Err() != nil {
			return res, err
		}
		wait := backoff
		if re.RetryAfter > 0 {
			wait = re.RetryAfter
		}
		if wait > maxRetryWait {
			return res, err
		}
		c.log.DebugContext(ctx, "effect retrying", "op", op, "try", try, "wait", wait.String(), "err", err)
		t := c.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return provider.EffectResult{}, ctx.Err()
		case <-t.C():
		}
		backoff *= 2
	}
}

// token mints (or reuses) the effect identity's token on the target repo with perms.
func (c *Conn) token(ctx context.Context, e *effect, perms map[string]string) (string, error) {
	cred, err := c.minter.Token(ctx, provider.GitCredentialRequest{Identity: e.identity, Repo: e.repo, Permissions: perms, TTL: tokens.MaxTTL})
	c.record(e.identity, err)
	if err != nil {
		return "", err
	}
	return cred.Token, nil
}

// result is an effect's result on the object ref (kind, "owner/name#n").
func (c *Conn) result(kind, repo string, number int, url string, replayed bool) provider.EffectResult {
	ref := c.ref(kind, repo+"#"+strconv.Itoa(number))
	return provider.EffectResult{URL: url, Object: &ref, Replayed: replayed}
}

// ghObject is an issue or a PR as the issues API shows it.
type ghObject struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	State       string          `json:"state"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	Labels      []ghLabel       `json:"labels"`
	User        ghUser          `json:"user"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (o *ghObject) kind() string {
	if len(o.PullRequest) > 0 && string(o.PullRequest) != "null" {
		return provider.KindPR
	}
	return provider.KindIssue
}

func (o *ghObject) body() string {
	if o.Body == nil {
		return ""
	}
	return *o.Body
}

func (o *ghObject) hasLabel(name string) bool {
	return slices.ContainsFunc(o.Labels, func(l ghLabel) bool { return strings.EqualFold(l.Name, name) })
}

// object reads the target through the issues API. A target whose request named a kind it
// isn't is ErrNotFound, as Observe and Project have it.
func (c *Conn) object(ctx context.Context, tok string, e *effect) (*ghObject, error) {
	var o ghObject
	if _, err := c.minter.Get(ctx, tok, e.issuePath(), nil, &o); err != nil {
		return nil, err
	}
	if k := e.req.Target.Kind; (k == provider.KindIssue || k == provider.KindPR) && k != o.kind() {
		return nil, fmt.Errorf("%s is a %s, not a %s: %w", e.req.Target.Ref, o.kind(), k, provider.ErrNotFound)
	}
	return &o, nil
}

// ghWritten is something an App may have written: a comment, an issue, a PR or a review.
type ghWritten struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	Body        *string         `json:"body"`
	User        ghUser          `json:"user"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// findWritten lists path (at most pages pages) for the first item the effect's App wrote with
// the effect's marker.
func (c *Conn) findWritten(ctx context.Context, tok string, e *effect, path string, query url.Values, pages int, skip func(*ghWritten) bool) (*ghWritten, error) {
	var found *ghWritten
	err := c.list(ctx, tok, path, query, pages, func(item json.RawMessage) (bool, error) {
		var w ghWritten
		if err := json.Unmarshal(item, &w); err != nil {
			return false, fmt.Errorf("decode %s: %w", path, err)
		}
		if skip != nil && skip(&w) {
			return true, nil
		}
		if sameLogin(w.User.Login, e.bot) && w.Body != nil && carries(*w.Body, e.key()) {
			found = &w
			return false, nil
		}
		return true, nil
	})
	return found, err
}

// writePerms is the write permission on the target through the issues API: issues on an
// issue, pull_requests on a PR (enough for its comments and labels).
func writePerms(kind string) map[string]string { return projectPerms(kind) }

func issuesWrite() map[string]string { return map[string]string{"issues": "write"} }

// comment posts args.body on the target (comment, pr.comment), unless the App's comment with
// the key is already there.
func (c *Conn) applyComment(ctx context.Context, e *effect) (provider.EffectResult, error) {
	body := stringArg(e.req.Args, "body")
	if strings.TrimSpace(body) == "" {
		return provider.EffectResult{}, fmt.Errorf("args.body is required: %w", provider.ErrInvalid)
	}
	url, replayed, err := c.comment(ctx, e, writePerms(e.kind), body)
	if err != nil {
		return provider.EffectResult{}, err
	}
	return c.result(e.kind, e.repo, e.number, url, replayed), nil
}

// comment posts body with the effect's marker on the target unless the App already did; it
// returns the comment's URL and whether it was found.
func (c *Conn) comment(ctx context.Context, e *effect, perms map[string]string, body string) (string, bool, error) {
	marked, err := markBody(body, e.key())
	if err != nil {
		return "", false, err
	}
	tok, err := c.token(ctx, e, perms)
	if err != nil {
		return "", false, err
	}
	found, err := c.findWritten(ctx, tok, e, e.issuePath()+"/comments", nil, maxCommentPages, nil)
	if err != nil {
		return "", false, fmt.Errorf("look up the comment: %w", err)
	}
	if found != nil {
		return found.HTMLURL, true, nil
	}
	var created ghWritten
	if _, err := c.minter.Do(ctx, tok, http.MethodPost, e.issuePath()+"/comments", nil, map[string]string{"body": marked}, http.StatusCreated, &created); err != nil {
		return "", false, fmt.Errorf("create the comment: %w", err)
	}
	return created.HTMLURL, false, nil
}

// applyLabelAdd adds the labels in args.labels and args.label the target lacks.
func (c *Conn) applyLabelAdd(ctx context.Context, e *effect) (provider.EffectResult, error) {
	labels := labelsArg(e.req.Args)
	if len(labels) == 0 {
		return provider.EffectResult{}, fmt.Errorf("args.labels (or args.label) is required: %w", provider.ErrInvalid)
	}
	tok, err := c.token(ctx, e, writePerms(e.kind))
	if err != nil {
		return provider.EffectResult{}, err
	}
	o, err := c.object(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	added, err := c.addLabels(ctx, tok, e, o, labels)
	if err != nil {
		return provider.EffectResult{}, err
	}
	return c.result(o.kind(), e.repo, e.number, o.HTMLURL, !added), nil
}

// addLabels adds the labels o lacks (case-insensitively, as GitHub compares them); it reports
// whether it added any.
func (c *Conn) addLabels(ctx context.Context, tok string, e *effect, o *ghObject, labels []string) (bool, error) {
	var missing []string
	for _, l := range labels {
		if !o.hasLabel(l) && !slices.ContainsFunc(missing, func(m string) bool { return strings.EqualFold(m, l) }) {
			missing = append(missing, l)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	if _, err := c.minter.Do(ctx, tok, http.MethodPost, e.issuePath()+"/labels", nil, map[string]any{"labels": missing}, http.StatusOK, nil); err != nil {
		return false, fmt.Errorf("add labels %v: %w", missing, err)
	}
	for _, l := range missing {
		o.Labels = append(o.Labels, ghLabel{Name: l})
	}
	return true, nil
}

// applyLabelRemove removes the labels in args.labels and args.label the target has.
func (c *Conn) applyLabelRemove(ctx context.Context, e *effect) (provider.EffectResult, error) {
	labels := labelsArg(e.req.Args)
	if len(labels) == 0 {
		return provider.EffectResult{}, fmt.Errorf("args.labels (or args.label) is required: %w", provider.ErrInvalid)
	}
	tok, err := c.token(ctx, e, writePerms(e.kind))
	if err != nil {
		return provider.EffectResult{}, err
	}
	o, err := c.object(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	removed := false
	for _, l := range o.Labels {
		if !slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l.Name) }) {
			continue
		}
		_, err := c.minter.Do(ctx, tok, http.MethodDelete, e.issuePath()+"/labels/"+url.PathEscape(l.Name), nil, nil, http.StatusOK, nil)
		if err != nil && !errors.Is(err, provider.ErrNotFound) { // already gone
			return provider.EffectResult{}, fmt.Errorf("remove label %q: %w", l.Name, err)
		}
		removed = true
	}
	return c.result(o.kind(), e.repo, e.number, o.HTMLURL, !removed), nil
}

// applyCreateIssue creates an issue in the target repo (args.title, args.body, args.labels)
// unless the App's issue with the key exists: it looks through the App's newest issues
// (GET …/issues?creator=, which, unlike search, sees an issue as soon as it exists).
func (c *Conn) applyCreateIssue(ctx context.Context, e *effect) (provider.EffectResult, error) {
	title := stringArg(e.req.Args, "title")
	if strings.TrimSpace(title) == "" {
		return provider.EffectResult{}, fmt.Errorf("args.title is required: %w", provider.ErrInvalid)
	}
	body, err := markBody(stringArg(e.req.Args, "body"), e.key())
	if err != nil {
		return provider.EffectResult{}, err
	}
	tok, err := c.token(ctx, e, issuesWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	q := url.Values{"creator": {e.bot}, "state": {"all"}, "sort": {"created"}, "direction": {"desc"}}
	found, err := c.findWritten(ctx, tok, e, "repos/"+e.repo+"/issues", q, maxCreatedPages, func(w *ghWritten) bool {
		return len(w.PullRequest) > 0 && string(w.PullRequest) != "null"
	})
	if err != nil {
		return provider.EffectResult{}, fmt.Errorf("look up the issue: %w", err)
	}
	if found != nil {
		return c.result(provider.KindIssue, e.repo, found.Number, found.HTMLURL, true), nil
	}
	in := map[string]any{"title": title, "body": body}
	if labels := stringsArg(e.req.Args, "labels"); len(labels) > 0 {
		in["labels"] = labels
	}
	var created ghWritten
	if _, err := c.minter.Do(ctx, tok, http.MethodPost, "repos/"+e.repo+"/issues", nil, in, http.StatusCreated, &created); err != nil {
		return provider.EffectResult{}, fmt.Errorf("create the issue: %w", err)
	}
	return c.result(provider.KindIssue, e.repo, created.Number, created.HTMLURL, false), nil
}

// applySubIssueAdd makes args.issue (a ref or a number in the target's repo) a sub-issue of
// the target, unless it already is.
func (c *Conn) applySubIssueAdd(ctx context.Context, e *effect) (provider.EffectResult, error) {
	repo, n, err := refArg(e.req.Args, "issue", e.repo)
	switch {
	case err != nil:
		return provider.EffectResult{}, err
	case !strings.EqualFold(repo, e.repo):
		return provider.EffectResult{}, fmt.Errorf("args.issue %s#%d is not in %s: %w", repo, n, e.repo, provider.ErrInvalid)
	case n == e.number:
		return provider.EffectResult{}, fmt.Errorf("an issue can't be its own sub-issue: %w", provider.ErrInvalid)
	}
	tok, err := c.token(ctx, e, issuesWrite())
	if err != nil {
		return provider.EffectResult{}, err
	}
	parent, err := c.object(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	var child ghObject
	if _, err := c.minter.Get(ctx, tok, fmt.Sprintf("repos/%s/issues/%d", e.repo, n), nil, &child); err != nil {
		return provider.EffectResult{}, fmt.Errorf("read the sub-issue: %w", err)
	}
	if parent.kind() != provider.KindIssue || child.kind() != provider.KindIssue {
		return provider.EffectResult{}, fmt.Errorf("sub-issues link issues, not PRs: %w", provider.ErrInvalid)
	}
	linked := false
	err = c.list(ctx, tok, e.issuePath()+"/sub_issues", nil, 1, func(item json.RawMessage) (bool, error) {
		var s ghObject
		if err := json.Unmarshal(item, &s); err != nil {
			return false, fmt.Errorf("decode sub-issue: %w", err)
		}
		linked = s.ID == child.ID
		return !linked, nil
	})
	if err != nil {
		return provider.EffectResult{}, fmt.Errorf("list sub-issues: %w", err)
	}
	if !linked {
		_, err := c.minter.Do(ctx, tok, http.MethodPost, e.issuePath()+"/sub_issues", nil, map[string]int64{"sub_issue_id": child.ID}, http.StatusCreated, nil)
		var apiErr *tokens.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(apiErr.Message), "already"):
			linked = true // added since the listing
		case err != nil:
			return provider.EffectResult{}, fmt.Errorf("add the sub-issue: %w", err)
		}
	}
	return c.result(provider.KindIssue, e.repo, e.number, parent.HTMLURL, linked), nil
}

// closeReasons are the state_reason values close takes (args.reason).
var closeReasons = []string{"", "completed", "not_planned"}

// applyClose closes the target (args.reason: completed or not_planned, issues only) unless it
// is closed.
func (c *Conn) applyClose(ctx context.Context, e *effect) (provider.EffectResult, error) {
	reason := stringArg(e.req.Args, "reason")
	if !slices.Contains(closeReasons, reason) {
		return provider.EffectResult{}, fmt.Errorf("args.reason %q: want completed or not_planned: %w", reason, provider.ErrInvalid)
	}
	tok, err := c.token(ctx, e, writePerms(e.kind))
	if err != nil {
		return provider.EffectResult{}, err
	}
	o, err := c.object(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	closed, err := c.closeObject(ctx, tok, e, o, reason)
	if err != nil {
		return provider.EffectResult{}, err
	}
	return c.result(o.kind(), e.repo, e.number, o.HTMLURL, !closed), nil
}

// closeObject closes o (through the issues API, which closes PRs too) unless it is closed; it
// reports whether it closed it.
func (c *Conn) closeObject(ctx context.Context, tok string, e *effect, o *ghObject, reason string) (bool, error) {
	if o.State == "closed" {
		return false, nil
	}
	in := map[string]string{"state": "closed"}
	if reason != "" && o.kind() == provider.KindIssue {
		in["state_reason"] = reason
	}
	if _, err := c.minter.Do(ctx, tok, http.MethodPatch, e.issuePath(), nil, in, http.StatusOK, nil); err != nil {
		return false, fmt.Errorf("close: %w", err)
	}
	o.State = "closed"
	return true, nil
}

// DuplicateLabel is the label link-duplicate adds.
const DuplicateLabel = "duplicate"

// applyLinkDuplicate marks the target a duplicate of args.duplicateOf (a ref, or a number in
// the target's repo): a "Duplicate of …" comment with the key's marker, the duplicate label,
// and close as not planned; each step is skipped when already done.
func (c *Conn) applyLinkDuplicate(ctx context.Context, e *effect) (provider.EffectResult, error) {
	repo, n, err := refArg(e.req.Args, "duplicateOf", e.repo)
	if err != nil {
		return provider.EffectResult{}, err
	}
	if strings.EqualFold(repo, e.repo) && n == e.number {
		return provider.EffectResult{}, fmt.Errorf("an issue can't duplicate itself: %w", provider.ErrInvalid)
	}
	of := "#" + strconv.Itoa(n)
	if !strings.EqualFold(repo, e.repo) {
		of = repo + of
	}
	perms := writePerms(e.kind)
	_, commented, err := c.comment(ctx, e, perms, "Duplicate of "+of)
	if err != nil {
		return provider.EffectResult{}, err
	}
	tok, err := c.token(ctx, e, perms)
	if err != nil {
		return provider.EffectResult{}, err
	}
	o, err := c.object(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, err
	}
	labelled, err := c.addLabels(ctx, tok, e, o, []string{DuplicateLabel})
	if err != nil {
		return provider.EffectResult{}, err
	}
	closed, err := c.closeObject(ctx, tok, e, o, "not_planned")
	if err != nil {
		return provider.EffectResult{}, err
	}
	return c.result(o.kind(), e.repo, e.number, o.HTMLURL, commented && !labelled && !closed), nil
}

// ---- Args ----------------------------------------------------------------------------------

// stringArg returns args[key] when it is a string.
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// stringsArg returns args[key] as strings: a []string, the strings of a []any, or one string.
func stringsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return slices.Clone(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

// labelsArg is args.labels plus args.label, as the Effector checks them.
func labelsArg(args map[string]any) []string {
	ls := stringsArg(args, "labels")
	if l := stringArg(args, "label"); l != "" {
		ls = append(ls, l)
	}
	return ls
}

// boolArg returns args[key] when it is a bool.
func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// refArg reads args[key] as an issue: "owner/name#n", "#n", "n" or a number (repo then being
// defaultRepo).
func refArg(args map[string]any, key, defaultRepo string) (string, int, error) {
	bad := fmt.Errorf("args.%s: want an issue (owner/name#n, #n or n): %w", key, provider.ErrInvalid)
	switch v := args[key].(type) {
	case float64:
		if v >= 1 && v == float64(int(v)) {
			return defaultRepo, int(v), nil
		}
	case int:
		if v >= 1 {
			return defaultRepo, v, nil
		}
	case json.Number:
		if n, err := strconv.Atoi(v.String()); err == nil && n >= 1 {
			return defaultRepo, n, nil
		}
	case string:
		repo, num, found := strings.Cut(v, "#")
		if !found {
			repo, num = "", v
		}
		if repo == "" {
			repo = defaultRepo
		}
		if n, err := strconv.Atoi(num); err == nil && n >= 1 && validRepo(repo) {
			return repo, n, nil
		}
	}
	return "", 0, bad
}

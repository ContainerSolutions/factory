package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

// The CI op, ci.rerun: re-run the failed GitHub Actions workflow runs of a PR's head commit
// (github-provider.md §Effects). Its token is actions: write on the target repo, with
// pull_requests: read for the PR's head.

const (
	// OutcomeNothingToRerun is ci.rerun's outcome when the head has no failed run to re-run:
	// the latest run of every workflow passed, is still running or is at MaxRunAttempts.
	OutcomeNothingToRerun = "nothing-to-rerun"
	// OutcomePartiallyRerun starts ci.rerun's outcome when some runs were re-run and others
	// skipped (refused by GitHub, or at MaxRunAttempts); the outcome names the skipped runs:
	// "partially-rerun: skipped run 12 (refused), run 13 (attempt 3)".
	OutcomePartiallyRerun = "partially-rerun"
	// MaxRunAttempts is the attempt ci.rerun never takes a run past: a run whose latest
	// attempt is MaxRunAttempts or more is skipped, a safety net against a rule re-running a
	// failing run forever.
	MaxRunAttempts = 3
	// maxRunPages bounds the pages of a head commit's workflow runs ci.rerun reads.
	maxRunPages = 5
	// maxReruns bounds the ci.rerun effects a Conn remembers for replays (rerunLog).
	maxReruns = 1024
)

func ciRerunPerms() map[string]string {
	return map[string]string{"actions": "write", "pull_requests": "read"}
}

// rerunConclusions are the conclusions of a run ci.rerun re-runs.
var rerunConclusions = []string{"failure", "timed_out", "cancelled", "startup_failure"}

// ghWorkflowRun is a workflow run as the Actions API shows it: its latest attempt's status and
// conclusion.
type ghWorkflowRun struct {
	ID         int64     `json:"id"`
	WorkflowID int64     `json:"workflow_id"`
	RunNumber  int       `json:"run_number"`
	RunAttempt int       `json:"run_attempt"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion *string   `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
}

func (r *ghWorkflowRun) conclusion() string {
	if r.Conclusion == nil {
		return ""
	}
	return *r.Conclusion
}

// rerunLog remembers the ci.rerun effects a Conn applied that re-ran something, by effective
// key (the request's key and the head SHA), so the same key on the same head is a replay. A
// re-run leaves nothing on GitHub that carries the key (the API takes no body to put a marker
// in), so unlike the other ops the replay can't be found there; the dispatcher's Effect store
// is the durable record (see applyCIRerun). Bounded to maxReruns, the oldest forgotten first.
type rerunLog struct {
	mu    sync.Mutex
	done  map[string]provider.EffectResult
	order []string
}

func (l *rerunLog) get(key string) (provider.EffectResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, ok := l.done[key]
	if ok && res.Object != nil {
		ref := *res.Object // each caller gets its own copy
		res.Object = &ref
	}
	return res, ok
}

func (l *rerunLog) put(key string, res provider.EffectResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done == nil {
		l.done = map[string]provider.EffectResult{}
	}
	if _, ok := l.done[key]; !ok {
		l.order = append(l.order, key)
	}
	l.done[key] = res
	for len(l.order) > maxReruns {
		delete(l.done, l.order[0])
		l.order = l.order[1:]
	}
}

// applyCIRerun re-runs the failed workflow runs of the PR's head commit: of the head's runs,
// the latest of each workflow and event (latestRuns, the superseding rule of checksOf), and of
// those the completed ones that concluded failure, timed_out, cancelled or startup_failure,
// below MaxRunAttempts. A failure or timed_out run is re-run with POST …/rerun-failed-jobs; a
// cancelled or startup_failure one with POST …/rerun (a run cancelled by its concurrency group,
// or one that failed to start, may have no failed job). With nothing to re-run (every latest
// run passed, is still running or is at MaxRunAttempts) the outcome is OutcomeNothingToRerun,
// not an error. A run GitHub refuses (403, 422: over 30 days old, already re-running, …) is
// skipped and the others are still re-run; the outcome then names it (OutcomePartiallyRerun).
// Only when nothing was re-run and something was refused is the op ErrInvalid. The result's
// URL is the PR's checks page. A closed or merged PR is ErrInvalid.
//
// Idempotency: the effective key is the request's key and the head SHA. The Conn remembers the
// effective keys it re-ran something with (rerunLog), so the same key on the same head is a
// replay (Replayed, nothing posted). This covers what the dispatcher's Effect store doesn't:
// a rule's effect is applied before it is recorded and again when its reconcile retries, and
// an Effector call whose Apply succeeded but whose record failed is applied again. On the
// Effector path the store answers first: CreateEffect replays any recorded effectID(task, key)
// before the provider is called, and every successful Apply is recorded (nothing-to-rerun
// included), so a --key acts once per task whatever the head; to re-run a new head in the
// same task the agent uses a per-head key (--key rerun-<sha>) or no key. The Conn's memory is
// the process's; a replay it forgot still posts nothing while the runs it re-ran are queued or
// running, since only completed runs are re-run.
//
// Errors: a 404 is ErrNotFound; a 403 or 422 is ErrInvalid, naming Actions: write when the
// App lacks it (as does a token the installation can't be granted).
func (c *Conn) applyCIRerun(ctx context.Context, e *effect) (provider.EffectResult, error) {
	tok, err := c.token(ctx, e, ciRerunPerms())
	if err != nil {
		if errors.Is(err, provider.ErrInvalid) {
			return provider.EffectResult{}, fmt.Errorf("ci.rerun needs the %s identity's App to have %s on %s: %w", e.identity, actionsWrite, e.repo, err)
		}
		return provider.EffectResult{}, err
	}
	p, err := c.pull(ctx, tok, e)
	if err != nil {
		return provider.EffectResult{}, ciError(err, e.repo, "Pull requests: read")
	}
	if p.Merged || p.State != "open" {
		state := p.State
		if p.Merged {
			state = "merged"
		}
		return provider.EffectResult{}, fmt.Errorf("%s#%d is %s: no CI to re-run: %w", e.repo, e.number, state, provider.ErrInvalid)
	}
	key := e.key() + "@" + p.Head.SHA
	if res, ok := c.reruns.get(key); ok {
		res.Replayed = true
		return res, nil
	}
	runs, err := c.headRuns(ctx, tok, e.repo, p.Head.SHA)
	if err != nil {
		return provider.EffectResult{}, ciError(err, e.repo, actionsWrite)
	}
	res := c.result(provider.KindPR, e.repo, e.number, strings.TrimSuffix(p.HTMLURL, "/")+"/checks", false)
	var (
		rerun    []int64
		skipped  []string // "run <id> (<why>)", for the outcome
		refusals []error
	)
	for _, r := range latestRuns(runs) {
		if r.Status != "completed" || !slices.Contains(rerunConclusions, r.conclusion()) {
			continue
		}
		if r.RunAttempt >= MaxRunAttempts {
			skipped = append(skipped, fmt.Sprintf("run %d (attempt %d)", r.ID, r.RunAttempt))
			continue
		}
		path := fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", e.repo, r.ID)
		if wholeRerun(r.conclusion()) {
			path = fmt.Sprintf("repos/%s/actions/runs/%d/rerun", e.repo, r.ID)
		}
		if _, err := c.minter.Do(ctx, tok, http.MethodPost, path, nil, nil, http.StatusCreated, nil); err != nil {
			err = fmt.Errorf("re-run workflow run %d: %w", r.ID, ciError(err, e.repo, actionsWrite))
			if !errors.Is(err, provider.ErrInvalid) {
				return provider.EffectResult{}, err // not found, or retryable: the whole op again
			}
			skipped = append(skipped, fmt.Sprintf("run %d (refused)", r.ID))
			refusals = append(refusals, err)
			continue
		}
		rerun = append(rerun, r.ID)
	}
	ref := e.repo + "#" + strconv.Itoa(e.number)
	switch {
	case len(rerun) == 0 && len(refusals) > 0:
		return provider.EffectResult{}, fmt.Errorf("no workflow run could be re-run: %w", errors.Join(refusals...))
	case len(rerun) == 0:
		res.Outcome = OutcomeNothingToRerun
		return res, nil
	case len(skipped) > 0:
		res.Outcome = OutcomePartiallyRerun + ": skipped " + strings.Join(skipped, ", ")
	}
	c.reruns.put(key, res)
	c.log.DebugContext(ctx, "workflow runs re-run", "ref", ref, "head", p.Head.SHA, "runs", rerun, "skipped", skipped, "refusals", refusals)
	return res, nil
}

// wholeRerun reports whether a run with conclusion is re-run whole (POST …/rerun) rather than
// its failed jobs: a cancelled run or one that failed to start may have no failed job.
func wholeRerun(conclusion string) bool {
	return conclusion == "cancelled" || conclusion == "startup_failure"
}

// headRuns lists the workflow runs of the head commit sha (up to maxRunPages pages).
func (c *Conn) headRuns(ctx context.Context, tok, repo, sha string) ([]ghWorkflowRun, error) {
	q := url.Values{"head_sha": {sha}, "per_page": {strconv.Itoa(perPage)}}
	var out []ghWorkflowRun
	for range maxRunPages {
		var page struct {
			WorkflowRuns []ghWorkflowRun `json:"workflow_runs"`
		}
		h, err := c.minter.Get(ctx, tok, "repos/"+repo+"/actions/runs", q, &page)
		if err != nil {
			return nil, fmt.Errorf("list workflow runs of %s: %w", sha, err)
		}
		out = append(out, page.WorkflowRuns...)
		next, ok := nextQuery(h)
		if !ok {
			break
		}
		q = next
	}
	return out, nil
}

// latestRuns are the runs no later run of the same workflow and event supersedes, in their
// order: checksOf's rule for check suites (a run cancelled by its concurrency group, or
// re-triggered, is superseded by the later run). The runs API lists a run from its creation,
// before it has jobs, so a later run supersedes here from the start as its suite does in
// latestSuites.
func latestRuns(runs []ghWorkflowRun) []ghWorkflowRun {
	kind := func(r ghWorkflowRun) string { return strconv.FormatInt(r.WorkflowID, 10) + "/" + r.Event }
	order := func(r ghWorkflowRun) runOrder { return runOrder{number: r.RunNumber, created: r.CreatedAt, id: r.ID} }
	latest := map[string]int{}
	for i, r := range runs {
		if j, seen := latest[kind(r)]; !seen || order(r).after(order(runs[j])) {
			latest[kind(r)] = i
		}
	}
	var out []ghWorkflowRun
	for i, r := range runs {
		if latest[kind(r)] == i {
			out = append(out, r)
		}
	}
	return out
}

// actionsWrite is the App permission ci.rerun's Actions calls need, as GitHub's App settings
// name it.
const actionsWrite = "Actions: write"

// ciError maps a GitHub refusal of ci.rerun's calls: a 403 that isn't a rate limit is
// ErrInvalid, naming perm when GitHub says the App may not do it; anything else as is (404
// ErrNotFound, 422 ErrInvalid, rate limits and 5xx retryable).
func ciError(err error, repo, perm string) error {
	if !errors.Is(err, tokens.ErrForbidden) {
		return err
	}
	var apiErr *tokens.APIError
	if errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "not accessible by integration") {
		return fmt.Errorf("the App lacks %s on %s (grant it in the GitHub App's permissions): %w: %w", perm, repo, err, provider.ErrInvalid)
	}
	return fmt.Errorf("%w: %w", err, provider.ErrInvalid)
}

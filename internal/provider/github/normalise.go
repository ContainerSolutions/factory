package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
)

// Object kinds the normaliser emits besides provider.KindIssue and provider.KindPR.
const (
	KindRelease    = "release"
	KindDeployment = "deployment"
)

// trustedTiers are the tiers whose own text (an issue body, a comment) is inlined into the
// facts; anyone else's is the content ref (github-provider.md §Ingress, untrusted text).
var trustedTiers = []string{"maintainer", "bot"}

// draft is one Event to be: its object and facts.
type draft struct {
	obj        v1alpha1.ObjectRef
	facts      map[string]any
	contentRef string
	// effect is the key of the factory effect that created obj (Data.Effect).
	effect string
}

// Normalise implements provider.Conn: the mapping table of github-provider.md §Ingress and
// normalisation. A delivery becomes one Event per object it is about (a check suite one per
// PR, a deployment one per merged PR it contains), numbered in object-ref order:
// ID provider.EventID(ev.ID, n), Source and Data.Provider the Provider's name, Subject the
// object ref, Time the delivery's ReceivedAt. The actor is "<provider>:<login>", its tier the
// first Provider trust rule it matches (DefaultTier when none does), and the factory's own
// Apps (by their bot login) set IsFactory and Identity. Data.Raw holds the payload, which
// rules never see; Data.Facts is what they read (event.*).
//
// A delivery whose event or action isn't in the table maps to no Events and no error. A body
// that isn't a payload, or a delivery for another Provider, is provider.ErrInvalid. GitHub
// calls the mapping needs (the factory Apps' logins, group members for trust, a release's
// previous tag, the PRs a deployment contains) return their errors, so the delivery is retried.
func (c *Conn) Normalise(ctx context.Context, ev provider.RawEvent) ([]v1alpha1.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ev.Provider != c.name {
		return nil, fmt.Errorf("github %s: normalise %s: delivery is for provider %q: %w", c.name, ev.ID, ev.Provider, provider.ErrInvalid)
	}
	event := ev.Type
	if event == "" {
		event = ev.Headers.Get(headerEvent)
	}
	if event == "" {
		return nil, fmt.Errorf("github %s: normalise %s: no event name: %w", c.name, ev.ID, provider.ErrInvalid)
	}
	var e envelope
	if err := json.Unmarshal(ev.Body, &e); err != nil {
		return nil, fmt.Errorf("github %s: normalise %s: not a %s payload: %w", c.name, ev.ID, event, provider.ErrInvalid)
	}
	typ, err := neutralType(event, &e)
	if err != nil {
		return nil, fmt.Errorf("github %s: normalise %s: %w", c.name, ev.ID, err)
	}
	if typ == "" {
		c.log.DebugContext(ctx, "delivery maps to no event", "delivery", ev.ID, "event", event, "action", safeAction(e.Action))
		return nil, nil
	}
	actor, err := c.actor(ctx, &e)
	if err != nil {
		return nil, fmt.Errorf("github %s: normalise %s: actor: %w", c.name, ev.ID, err)
	}
	drafts, err := c.drafts(ctx, typ, &e, actor)
	if err != nil {
		return nil, fmt.Errorf("github %s: normalise %s (%s): %w", c.name, ev.ID, typ, err)
	}
	slices.SortStableFunc(drafts, func(a, b draft) int { return strings.Compare(a.obj.Ref, b.obj.Ref) })

	var raw bytes.Buffer
	if err := json.Compact(&raw, ev.Body); err != nil {
		return nil, fmt.Errorf("github %s: normalise %s: %w", c.name, ev.ID, provider.ErrInvalid)
	}
	at := ev.ReceivedAt
	if at.IsZero() {
		at = c.clock.Now()
	}
	out := make([]v1alpha1.Event, 0, len(drafts))
	for n, d := range drafts {
		facts, err := jsonFacts(d.facts)
		if err != nil {
			return nil, fmt.Errorf("github %s: normalise %s: facts: %w", c.name, ev.ID, err)
		}
		var a *v1alpha1.Actor
		if actor != nil {
			cp := *actor
			a = &cp
		}
		obj := d.obj
		out = append(out, v1alpha1.Event{
			TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
			ID:       provider.EventID(ev.ID, n),
			Source:   c.name,
			Type:     typ,
			Subject:  obj.Ref,
			Time:     at.UTC(),
			Data: v1alpha1.EventData{
				Provider:   c.name,
				Actor:      a,
				Object:     &obj,
				Repo:       repoOf(obj.Ref),
				ContentRef: d.contentRef,
				Facts:      facts,
				Effect:     d.effect,
				Raw:        slices.Clone(raw.Bytes()),
			},
		})
	}
	return out, nil
}

// neutralType maps a GitHub event and action to the neutral type; "" for none. It returns an
// ErrInvalid error when a mapped event lacks the parts the mapping reads.
func neutralType(event string, e *envelope) (v1alpha1.EventType, error) {
	var (
		t    v1alpha1.EventType
		need = map[string]bool{}
	)
	switch event {
	case "issues":
		t = map[string]v1alpha1.EventType{
			"opened": v1alpha1.EventIssueOpened, "closed": v1alpha1.EventIssueClosed, "labeled": v1alpha1.EventIssueLabeled,
		}[e.Action]
		need["issue"] = e.Issue != nil
		need["label"] = e.Label != nil || t != v1alpha1.EventIssueLabeled
	case "issue_comment":
		if e.Action == "created" {
			t = v1alpha1.EventIssueCommented
		}
		need["issue"], need["comment"] = e.Issue != nil, e.Comment != nil
	case "pull_request":
		t = map[string]v1alpha1.EventType{
			"opened": v1alpha1.EventPROpened, "synchronize": v1alpha1.EventPRUpdated,
			"ready_for_review": v1alpha1.EventPRReadyForReview, "labeled": v1alpha1.EventPRLabeled,
			"closed": v1alpha1.EventPRClosed,
		}[e.Action]
		need["pull_request"] = e.PullRequest != nil
		need["label"] = e.Label != nil || t != v1alpha1.EventPRLabeled
		if t == v1alpha1.EventPRClosed && e.PullRequest != nil && e.PullRequest.Merged {
			t = v1alpha1.EventPRMerged
		}
	case "pull_request_review":
		if e.Action == "submitted" {
			t = v1alpha1.EventPRReviewSubmitted
		}
		need["pull_request"], need["review"] = e.PullRequest != nil, e.Review != nil
	case "check_suite", "check_run":
		if e.Action == "completed" {
			t = v1alpha1.EventPRChecksCompleted
		}
		need[event] = (event == "check_suite" && e.CheckSuite != nil) || (event == "check_run" && e.CheckRun != nil)
	case "release":
		if e.Action == "published" {
			t = v1alpha1.EventReleasePublished
		}
		need["release"] = e.Release != nil && e.Release.TagName != ""
	case "deployment_status":
		if e.DeploymentStatus != nil {
			switch e.DeploymentStatus.State {
			case "success":
				t = v1alpha1.EventDeploySucceeded
			case "failure", "error":
				t = v1alpha1.EventDeployFailed
			}
		}
		need["deployment"] = e.Deployment != nil && e.Deployment.SHA != ""
	}
	if t == "" {
		return "", nil
	}
	need["repository"] = e.Repository != nil && validRepo(e.Repository.FullName)
	for _, part := range slices.Sorted(maps.Keys(need)) {
		if !need[part] {
			return "", fmt.Errorf("%s.%s payload without a valid %s: %w", event, e.Action, part, provider.ErrInvalid)
		}
	}
	return t, nil
}

// drafts builds the Events' objects and facts for a mapped delivery.
func (c *Conn) drafts(ctx context.Context, typ v1alpha1.EventType, e *envelope, actor *v1alpha1.Actor) ([]draft, error) {
	repo := e.Repository.FullName
	sender := ""
	if e.Sender != nil {
		sender = e.Sender.Login
	}
	switch typ {
	case v1alpha1.EventIssueOpened, v1alpha1.EventIssueClosed, v1alpha1.EventIssueLabeled:
		is := e.Issue
		ref := fmt.Sprintf("github://%s/issues/%d#body", repo, is.Number)
		body := ""
		if is.Body != nil {
			body = *is.Body
		}
		facts := map[string]any{"issue": map[string]any{
			"title":  is.Title,
			"labels": labelNames(is.Labels),
			"body":   inline(actor, sameLogin(is.User.Login, sender), body, ref),
		}}
		if typ == v1alpha1.EventIssueLabeled {
			facts["label"] = e.Label.Name
		}
		if typ == v1alpha1.EventIssueClosed && c.isTrainRepo(repo) {
			if train, ok := trainFacts(body); ok {
				facts["train"] = train
			}
		}
		d := draft{obj: c.ref(provider.KindIssue, fmt.Sprintf("%s#%d", repo, is.Number)), facts: facts, contentRef: ref}
		if typ == v1alpha1.EventIssueOpened {
			key, err := c.createdBy(ctx, is.User.Login, body)
			if err != nil {
				return nil, err
			}
			d.effect = key
		}
		return []draft{d}, nil

	case v1alpha1.EventIssueCommented:
		is, cm := e.Issue, e.Comment
		kind := provider.KindIssue
		if len(is.PullRequest) > 0 && !bytes.Equal(is.PullRequest, []byte("null")) {
			kind = provider.KindPR
		}
		ref := fmt.Sprintf("github://%s/issues/%d#comment-%d", repo, is.Number, cm.ID)
		facts := map[string]any{"text": inline(actor, sameLogin(cm.User.Login, sender), cm.Body, ref)}
		key, err := c.createdBy(ctx, cm.User.Login, cm.Body)
		if err != nil {
			return nil, err
		}
		return []draft{{obj: c.ref(kind, fmt.Sprintf("%s#%d", repo, is.Number)), facts: facts, contentRef: ref, effect: key}}, nil

	case v1alpha1.EventPROpened, v1alpha1.EventPRUpdated, v1alpha1.EventPRReadyForReview,
		v1alpha1.EventPRLabeled, v1alpha1.EventPRMerged, v1alpha1.EventPRClosed:
		pr := e.PullRequest
		facts := map[string]any{"pr": map[string]any{"draft": pr.Draft, "head": pr.Head.SHA, "base": pr.Base.Ref}}
		if typ == v1alpha1.EventPRLabeled {
			facts["label"] = e.Label.Name
		}
		d := draft{
			obj: c.ref(provider.KindPR, fmt.Sprintf("%s#%d", repo, pr.Number)), facts: facts,
			contentRef: fmt.Sprintf("github://%s/pull/%d#body", repo, pr.Number),
		}
		// Only the event that creates the PR: its body keeps pr.open's marker, but a later
		// push or edit wasn't caused by that effect.
		if typ == v1alpha1.EventPROpened {
			key, err := c.createdBy(ctx, pr.User.Login, deref(pr.Body))
			if err != nil {
				return nil, err
			}
			d.effect = key
		}
		return []draft{d}, nil

	case v1alpha1.EventPRReviewSubmitted:
		pr, rv := e.PullRequest, e.Review
		facts := map[string]any{"review": map[string]any{"state": strings.ToLower(rv.State), "commit": rv.CommitID}}
		key, err := c.createdBy(ctx, rv.User.Login, deref(rv.Body))
		if err != nil {
			return nil, err
		}
		return []draft{{
			obj: c.ref(provider.KindPR, fmt.Sprintf("%s#%d", repo, pr.Number)), facts: facts,
			contentRef: fmt.Sprintf("github://%s/pull/%d#review-%d", repo, pr.Number, rv.ID), effect: key,
		}}, nil

	case v1alpha1.EventPRChecksCompleted:
		checks := e.CheckSuite
		if checks == nil {
			checks = e.CheckRun
		}
		var out []draft
		seen := map[int]bool{}
		for _, pr := range checks.PullRequests {
			// A suite also lists PRs of other repos whose head is this one's branch.
			if pr.Number <= 0 || seen[pr.Number] || (pr.Base.Repo.ID != 0 && pr.Base.Repo.ID != e.Repository.ID) {
				continue
			}
			seen[pr.Number] = true
			out = append(out, draft{
				obj:   c.ref(provider.KindPR, fmt.Sprintf("%s#%d", repo, pr.Number)),
				facts: map[string]any{"checks": map[string]any{"state": strings.ToLower(checks.Conclusion)}},
			})
		}
		return out, nil

	case v1alpha1.EventReleasePublished:
		facts, err := c.releaseFacts(ctx, repo, e.Release)
		if err != nil {
			return nil, err
		}
		return []draft{{obj: c.ref(KindRelease, repo+"#"+e.Release.TagName), facts: map[string]any{"release": facts}}}, nil

	case v1alpha1.EventDeploySucceeded, v1alpha1.EventDeployFailed:
		return c.deployDrafts(ctx, repo, e.Deployment, e.DeploymentStatus)
	}
	return nil, nil
}

// effectKeyRE matches an effect marker (effectMarker) and captures its encoded key.
var effectKeyRE = regexp.MustCompile(`<!-- factory:effect ([^ ]*) -->`)

// createdBy is the key of the factory effect that created an object authored by author with
// body: its first effect marker, percent-decoded, when author is one of the factory's own
// Apps, as the effects' replay lookup requires; "" otherwise. Anyone can paste a marker, so
// one in someone else's text is ignored, as is one that doesn't decode.
func (c *Conn) createdBy(ctx context.Context, author, body string) (string, error) {
	m := effectKeyRE.FindStringSubmatch(body)
	if m == nil || author == "" {
		return "", nil
	}
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return "", err
	}
	if _, ok := bots[strings.ToLower(author)]; !ok {
		return "", nil
	}
	key, err := url.PathUnescape(m[1])
	if err != nil {
		return "", nil
	}
	return key, nil
}

func (c *Conn) ref(kind, ref string) v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{Provider: c.name, Kind: kind, Ref: ref}
}

func (c *Conn) isTrainRepo(repo string) bool {
	return slices.ContainsFunc(c.trainRepos, func(r string) bool { return strings.EqualFold(r, repo) })
}

// inline is text when its author sent the event and the actor's tier is trusted, else the
// content ref.
func inline(a *v1alpha1.Actor, authored bool, text, ref string) string {
	if authored && a != nil && slices.Contains(trustedTiers, a.Tier) {
		return text
	}
	return ref
}

func labelNames(ls []ghLabel) []any {
	out := make([]any, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func sameLogin(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// repoOf is the repo of a ref ("owner/name#42" → "owner/name").
func repoOf(ref string) string {
	repo, _, _ := strings.Cut(ref, "#")
	return repo
}

func validRepo(full string) bool {
	owner, name, ok := strings.Cut(full, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(name, "/# ")
}

// safeAction is an action fit for a log line.
func safeAction(a string) string {
	if eventName.MatchString(a) {
		return a
	}
	return "(other)"
}

// jsonFacts is facts as the store keeps them (a JSON round-trip: numbers float64, lists
// []any), so rules see the same values before and after a restart.
func jsonFacts(facts map[string]any) (map[string]any, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

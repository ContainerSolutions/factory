package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github"
	"github.com/compliance-framework/factory/internal/provider/github/githubtest"
	"github.com/compliance-framework/factory/internal/rules"
)

var update = flag.Bool("update", false, "rewrite the golden Events under testdata/golden")

// testTrust exercises every trust kind, in the Provider's order.
var testTrust = []v1alpha1.TrustRule{
	{Tier: "maintainer", Group: "maintainers"},
	{Tier: "docs", Group: "docs-team"},
	{Tier: "contributor", AuthorAssociation: []string{"CONTRIBUTOR", "COLLABORATOR"}},
	{Tier: "bot", Bots: []string{"ccf-release-bot[bot]", "release-please[bot]"}},
	{Tier: "slack-member", WorkspaceMember: true},
	{Tier: "anyone"},
}

// worldEnv is newEnv with the org, its teams and the repos' releases and PRs the payloads under
// testdata/webhooks refer to.
func worldEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.p.Spec.Trust = testTrust
	s := e.srv
	s.AddOrgMember(org, "admin", "admin")
	s.AddOrgMember(org, "alice", "member")
	s.AddOrgMember(org, "bob", "member")
	s.AddTeam(org, "docs", "bob", "alice", "bob")
	s.AddReleases(org+"/api",
		githubtest.Release{Tag: "v1.5.0-rc.1", Prerelease: true},
		githubtest.Release{Tag: "v2.0.0", Draft: true},
		githubtest.Release{Tag: "v1.4.0"},
		githubtest.Release{Tag: "v1.4.0-rc.1", Prerelease: true},
		githubtest.Release{Tag: "agent-v9.0.0"},
		githubtest.Release{Tag: "v1.3.2"},
		githubtest.Release{Tag: "v1.3.10"}, // semver order, not string order
		githubtest.Release{Tag: "v1.3.1"},
		githubtest.Release{Tag: "nightly-2026-10-01", Prerelease: true},
	)
	s.AddReleases(org+"/helm-charts",
		githubtest.Release{Tag: "ccf-v3.0.0"},
		githubtest.Release{Tag: "agent-v1.0.1"},
		githubtest.Release{Tag: "agent-v1.0.0"},
		githubtest.Release{Tag: "ccf-v2.1.0"},
	)
	const deployed = "abc123abc123abc123abc123abc123abc123abc1"
	s.AddPulls(docs,
		githubtest.PullRequest{Number: 93, Base: "main"}, // closed without merging
		githubtest.PullRequest{Number: 90, MergedAt: t0.Add(-time.Hour), MergeCommit: deployed, Base: "main"},
		githubtest.PullRequest{Number: 92, MergedAt: t0.Add(-2 * time.Hour), MergeCommit: "m92", Base: "main"},
		githubtest.PullRequest{Number: 91, MergedAt: t0.Add(-3 * time.Hour), MergeCommit: "m91", Base: "main"},
	)
	s.SetAncestor(docs, "m91", deployed)
	return e
}

func readWebhook(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawEvent is the delivery of testdata/webhooks/<name>.json: its event is the name up to the
// first dot.
func rawEvent(t *testing.T, name, delivery string) provider.RawEvent {
	t.Helper()
	event, _, _ := strings.Cut(name, ".")
	return provider.RawEvent{
		ID: delivery, Provider: prov, Type: event,
		Headers: http.Header{"X-Github-Event": {event}, "X-Github-Delivery": {strings.TrimPrefix(delivery, "gh-")}},
		Body:    readWebhook(t, name), ReceivedAt: t0,
	}
}

// TestNormaliseGolden normalises every recorded payload (one or more per mapping row, and
// events the table drops) to its golden Events, testdata/golden/<name>.json (-update rewrites
// them). Data.Raw is checked against the payload and left out of the goldens.
func TestNormaliseGolden(t *testing.T) {
	e := worldEnv(t)
	c := e.connect(t)
	files, err := filepath.Glob("testdata/webhooks/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no recorded payloads: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			ev := rawEvent(t, name, "gh-"+strings.ReplaceAll(name, ".", "-"))
			got, err := c.Normalise(context.Background(), ev)
			if err != nil {
				t.Fatal(err)
			}
			var raw bytes.Buffer
			if err := json.Compact(&raw, ev.Body); err != nil {
				t.Fatal(err)
			}
			for i := range got {
				if !bytes.Equal(got[i].Data.Raw, raw.Bytes()) {
					t.Errorf("event %d: Raw isn't the payload", i)
				}
				got[i].Data.Raw = nil
			}
			if got == nil {
				got = []v1alpha1.Event{}
			}
			b, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, '\n')
			golden := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, b, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if !bytes.Equal(b, want) {
				t.Errorf("%s differs (run with -update to accept):\n got %s\nwant %s", golden, b, want)
			}
		})
	}
}

// TestNormaliseRows checks the mapping table row by row on the goldens' key facts, so a wrong
// -update can't pass silently.
func TestNormaliseRows(t *testing.T) {
	e := worldEnv(t)
	c := e.connect(t)
	type want struct {
		typ   v1alpha1.EventType
		refs  []string
		facts map[string]any // checked with reflect.DeepEqual on each event's facts, key by key
		actor v1alpha1.Actor
		// effect is Data.Effect: set from a marker only on the event that creates the object,
		// and only when one of the factory's Apps wrote it.
		effect string
	}
	anyone := func(login string, bot bool) v1alpha1.Actor {
		return v1alpha1.Actor{ID: prov + ":" + login, Tier: "anyone", IsBot: bot}
	}
	factory := func(login, identity string) v1alpha1.Actor {
		return v1alpha1.Actor{ID: prov + ":" + login, Tier: "anyone", IsBot: true, IsFactory: true, Identity: identity}
	}
	semverFacts := func(major, minor, patch float64, pre bool, bump string) map[string]any {
		return map[string]any{"major": major, "minor": minor, "patch": patch, "prerelease": pre, "bump": bump}
	}
	deploy := map[string]any{"environment": "github-pages", "sha": "abc123abc123abc123abc123abc123abc123abc1", "url": "https://compliance-framework.github.io/docs/"}
	trainRepos := []any{
		map[string]any{"name": "api", "versions": map[string]any{".": "1.4.0"}, "bump": "minor"},
		map[string]any{"name": "agent", "versions": map[string]any{".": "0.9.2"}, "bump": "patch"},
		map[string]any{"name": "helm-charts", "versions": map[string]any{"charts/agent": "1.0.1", "charts/ccf": "3.0.0"}, "bump": "major"},
		map[string]any{"name": "ui", "versions": map[string]any{}, "bump": "none"},
	}
	tests := map[string]want{
		"issues.opened": {typ: v1alpha1.EventIssueOpened, refs: []string{docs + "#42"}, actor: anyone("someone", false), facts: map[string]any{
			"issue": map[string]any{"title": "Broken link", "labels": []any{}, "body": "github://" + docs + "/issues/42#body"},
		}},
		// The author App opened it: its marker names the effect (percent-decoded).
		"issues.opened-factory": {typ: v1alpha1.EventIssueOpened, refs: []string{docs + "#42"}, actor: factory("ccf-factory[bot]", "author"),
			effect: "evt/github-public/gh-epic-57/plan-epic/0"},
		"issues.opened-maintainer": {typ: v1alpha1.EventIssueOpened, refs: []string{docs + "#70"},
			actor: v1alpha1.Actor{ID: prov + ":admin", Tier: "maintainer"}, facts: map[string]any{
				"issue": map[string]any{"title": "Add i18n with a PT-BR translation", "labels": []any{}, "body": "Translate the docs to PT-BR, one PR per section."},
			}},
		// A maintainer labelling someone else's issue: the body stays a ref.
		"issues.labeled": {typ: v1alpha1.EventIssueLabeled, refs: []string{docs + "#42"},
			actor: v1alpha1.Actor{ID: prov + ":admin", Tier: "maintainer"}, facts: map[string]any{
				"label": "bug", "issue": map[string]any{"title": "Broken link", "labels": []any{"bug"}, "body": "github://" + docs + "/issues/42#body"},
			}},
		"issues.closed": {typ: v1alpha1.EventIssueClosed, refs: []string{docs + "#42"},
			actor: v1alpha1.Actor{ID: prov + ":alice", Tier: "docs"}},
		"issues.closed-train": {typ: v1alpha1.EventIssueClosed, refs: []string{org + "/workflows#123"},
			actor: v1alpha1.Actor{ID: prov + ":ccf-release-bot[bot]", Tier: "bot", IsBot: true}, facts: map[string]any{
				"train": map[string]any{"month": "2026-11", "manifest": "repos.yaml", "dryRun": false, "repos": trainRepos},
			}},
		"issues.closed-train-dry-run": {typ: v1alpha1.EventIssueClosed, refs: []string{org + "/workflows#130"},
			actor: v1alpha1.Actor{ID: prov + ":ccf-release-bot[bot]", Tier: "bot", IsBot: true}, facts: map[string]any{
				"train": map[string]any{"month": "2026-12", "manifest": "repos.mock.yaml", "dryRun": true, "repos": trainRepos},
			}},
		"issue_comment.created": {typ: v1alpha1.EventIssueCommented, refs: []string{docs + "#42"}, actor: anyone("someone", false), facts: map[string]any{
			"text": "github://" + docs + "/issues/42#comment-3401112222",
		}},
		"issue_comment.created-pr": {typ: v1alpha1.EventIssueCommented, refs: []string{docs + "#57"},
			actor: v1alpha1.Actor{ID: prov + ":admin", Tier: "maintainer"}, facts: map[string]any{"text": "/factory approve implement"}},
		"issue_comment.created-factory": {typ: v1alpha1.EventIssueCommented, refs: []string{docs + "#57"}, actor: factory("ccf-factory[bot]", "author"),
			facts: map[string]any{"text": "github://" + docs + "/issues/57#comment-3401113333"}, effect: "tsk_01JAB3K7Q2M8V9W4XYZ5N6P7RS/pr.comment/1"},
		"pull_request.opened": {typ: v1alpha1.EventPROpened, refs: []string{docs + "#88"},
			actor: v1alpha1.Actor{ID: prov + ":carol", Tier: "contributor"}, facts: map[string]any{
				"pr": map[string]any{"draft": false, "head": "c4r01c4r01c4r01c4r01c4r01c4r01c4r01c4r0", "base": "main"},
			}},
		"pull_request.opened-factory": {typ: v1alpha1.EventPROpened, refs: []string{docs + "#57"}, actor: factory("ccf-factory[bot]", "author"),
			effect: "tsk_01JAB3K7Q2M8V9W4XYZ5N6P7RS/pr.open/1"},
		// The same marker in carol's PR body: forgeable, so ignored.
		"pull_request.opened-forged-marker": {typ: v1alpha1.EventPROpened, refs: []string{docs + "#88"},
			actor: v1alpha1.Actor{ID: prov + ":carol", Tier: "contributor"}},
		// A push to the factory's PR: the body keeps pr.open's marker, but the push isn't that effect.
		"pull_request.synchronize":      {typ: v1alpha1.EventPRUpdated, refs: []string{docs + "#57"}, actor: factory("ccf-factory[bot]", "author")},
		"pull_request.ready_for_review": {typ: v1alpha1.EventPRReadyForReview, refs: []string{docs + "#88"}},
		"pull_request.labeled":          {typ: v1alpha1.EventPRLabeled, refs: []string{docs + "#88"}, facts: map[string]any{"label": "documentation"}},
		"pull_request.closed-merged":    {typ: v1alpha1.EventPRMerged, refs: []string{docs + "#57"}},
		"pull_request.closed":           {typ: v1alpha1.EventPRClosed, refs: []string{docs + "#88"}},
		"pull_request_review.submitted": {typ: v1alpha1.EventPRReviewSubmitted, refs: []string{docs + "#90"},
			actor: factory("ccf-factory-reviewer[bot]", "reviewer"), facts: map[string]any{
				"review": map[string]any{"state": "changes_requested", "commit": "f00df00df00df00df00df00df00df00df00df00d"},
			}, effect: "tsk_01JAB3RVW0000000000000000/review/1"},
		"pull_request_review.submitted-approved": {typ: v1alpha1.EventPRReviewSubmitted, refs: []string{docs + "#88"}, facts: map[string]any{
			"review": map[string]any{"state": "approved", "commit": "c4r01c4r01c4r01c4r01c4r01c4r01c4r01c4r0"},
		}},
		// One Event per PR of the suite in this repo, in ref order; the api PR is skipped.
		"check_suite.completed": {typ: v1alpha1.EventPRChecksCompleted, refs: []string{docs + "#10", docs + "#82", docs + "#9"},
			actor: anyone("github-actions[bot]", true), facts: map[string]any{"checks": map[string]any{"state": "success"}}},
		"check_run.completed": {typ: v1alpha1.EventPRChecksCompleted, refs: []string{docs + "#90"}, facts: map[string]any{"checks": map[string]any{"state": "failure"}}},
		"release.published": {typ: v1alpha1.EventReleasePublished, refs: []string{org + "/api#v1.4.0"}, facts: map[string]any{
			"release": map[string]any{"tag": "v1.4.0", "previousTag": "v1.3.10", "semver": semverFacts(1, 4, 0, false, "minor")},
		}},
		"release.published-prerelease": {typ: v1alpha1.EventReleasePublished, refs: []string{org + "/api#v1.5.0-rc.1"}, facts: map[string]any{
			"release": map[string]any{"tag": "v1.5.0-rc.1", "previousTag": "v1.4.0", "semver": semverFacts(1, 5, 0, true, "minor")},
		}},
		"release.published-component": {typ: v1alpha1.EventReleasePublished, refs: []string{org + "/helm-charts#ccf-v3.0.0"}, facts: map[string]any{
			"release": map[string]any{"tag": "ccf-v3.0.0", "previousTag": "ccf-v2.1.0", "semver": semverFacts(3, 0, 0, false, "major")},
		}},
		"release.published-not-semver": {typ: v1alpha1.EventReleasePublished, refs: []string{org + "/api#nightly-2026-10-16"}, facts: map[string]any{
			"release": map[string]any{"tag": "nightly-2026-10-16"},
		}},
		// Linked to the merged PRs the deployed commit contains (#90 is it, #91 an ancestor).
		"deployment_status.success": {typ: v1alpha1.EventDeploySucceeded, refs: []string{docs + "#90", docs + "#91"},
			actor: anyone("github-actions[bot]", true), facts: map[string]any{"deploy": deploy}},
		"deployment_status.failure": {typ: v1alpha1.EventDeployFailed, refs: []string{docs + "#d-992"}, facts: map[string]any{"deploy": map[string]any{
			"environment": "github-pages", "sha": "bad000bad000bad000bad000bad000bad000bad0", "url": "https://github.com/" + docs + "/actions/runs/11223344",
		}}},
		// The train extractor only reads the train repos.
		"issues.closed-train-outside-workflows": {typ: v1alpha1.EventIssueClosed, refs: []string{docs + "#43"}},
	}
	for _, dropped := range []string{
		"ping", "issues.edited", "pull_request.edited", "pull_request_review.dismissed", "pull_request_review_thread.resolved",
		"check_suite.requested", "release.created", "deployment_status.in_progress", "sub_issues.sub_issue_added",
	} {
		tests[dropped] = want{}
	}
	files, _ := filepath.Glob("testdata/webhooks/*.json")
	if len(files) != len(tests) {
		t.Errorf("%d recorded payloads, %d rows checked: keep them in step", len(files), len(tests))
	}
	for name, w := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := c.Normalise(context.Background(), rawEvent(t, name, "gh-d1"))
			if err != nil {
				t.Fatal(err)
			}
			if w.typ == "" {
				if len(got) != 0 {
					t.Fatalf("got %d events, want none", len(got))
				}
				return
			}
			var refs []string
			for n, ev := range got {
				refs = append(refs, ev.Data.Object.Ref)
				if ev.Type != w.typ || ev.ID != provider.EventID("gh-d1", n) || ev.Source != prov || ev.Data.Provider != prov ||
					ev.Subject != ev.Data.Object.Ref || !ev.Time.Equal(t0) || ev.Data.Repo != strings.Split(ev.Data.Object.Ref, "#")[0] {
					t.Errorf("event %d = %+v", n, ev)
				}
				if w.actor.ID != "" && !reflect.DeepEqual(*ev.Data.Actor, w.actor) {
					t.Errorf("actor = %+v, want %+v", *ev.Data.Actor, w.actor)
				}
				if ev.Data.Effect != w.effect {
					t.Errorf("effect = %q, want %q", ev.Data.Effect, w.effect)
				}
				for k, v := range w.facts {
					if !reflect.DeepEqual(ev.Data.Facts[k], v) {
						t.Errorf("facts[%s] = %#v, want %#v", k, ev.Data.Facts[k], v)
					}
				}
			}
			if !slices.Equal(refs, w.refs) {
				t.Errorf("objects = %v, want %v", refs, w.refs)
			}
		})
	}
}

func TestNormaliseRejects(t *testing.T) {
	e := worldEnv(t)
	c := e.connect(t)
	ctx := context.Background()
	ok := rawEvent(t, "issues.opened", "gh-1")
	for name, ev := range map[string]provider.RawEvent{
		"other provider": func() provider.RawEvent { ev := ok; ev.Provider = "github-enterprise"; return ev }(),
		"not JSON":       func() provider.RawEvent { ev := ok; ev.Body = []byte("payload=%7B%7D"); return ev }(),
		"no event name": func() provider.RawEvent {
			ev := ok
			ev.Type, ev.Headers = "", nil
			return ev
		}(),
		"issues without issue": func() provider.RawEvent {
			ev := ok
			ev.Body = []byte(`{"action":"opened","repository":{"full_name":"o/r"}}`)
			return ev
		}(),
		"labeled without label":    {ID: "gh-2", Provider: prov, Type: "issues", Body: []byte(`{"action":"labeled","issue":{"number":1},"repository":{"full_name":"o/r"}}`)},
		"bad repository":           {ID: "gh-3", Provider: prov, Type: "issues", Body: []byte(`{"action":"opened","issue":{"number":1},"repository":{"full_name":"o"}}`)},
		"release without tag":      {ID: "gh-4", Provider: prov, Type: "release", Body: []byte(`{"action":"published","release":{},"repository":{"full_name":"o/r"}}`)},
		"deployment without a sha": {ID: "gh-5", Provider: prov, Type: "deployment_status", Body: []byte(`{"action":"created","deployment_status":{"state":"success"},"deployment":{},"repository":{"full_name":"o/r"}}`)},
	} {
		if _, err := c.Normalise(ctx, ev); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("%s: Normalise = %v, want ErrInvalid", name, err)
		}
	}
	// The event name can come from the header when the delivery has no Type.
	hdr := ok
	hdr.Type = ""
	if evs, err := c.Normalise(ctx, hdr); err != nil || len(evs) != 1 {
		t.Errorf("event from header: %v, %v", evs, err)
	}

	// GitHub failing a call the mapping needs: the error is returned, so the delivery is
	// retried.
	e.srv.FailNext(githubtest.RouteReleases, githubtest.Failure{Status: 502})
	if _, err := c.Normalise(ctx, rawEvent(t, "release.published", "gh-6")); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("releases down: %v, want retryable", err)
	}
	e.srv.FailNext(githubtest.RouteCompare, githubtest.Failure{Status: 500})
	if _, err := c.Normalise(ctx, rawEvent(t, "deployment_status.success", "gh-7")); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("compare down: %v, want retryable", err)
	}
	ctxDone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Normalise(ctxDone, ok); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestFactoryIdentities(t *testing.T) {
	e := worldEnv(t)
	c := e.connect(t)
	e.srv.FailNext(githubtest.RouteApp, githubtest.Failure{Status: 502})
	if _, err := c.Normalise(context.Background(), rawEvent(t, "pull_request.opened-factory", "gh-1")); !errors.Is(err, provider.ErrRetryable) {
		t.Fatalf("GET /app failing: %v, want retryable (never guess whether the factory sent it)", err)
	}
	for range 3 {
		evs, err := c.Normalise(context.Background(), rawEvent(t, "pull_request.opened-factory", "gh-1"))
		if err != nil || !evs[0].Data.Actor.IsFactory || evs[0].Data.Actor.Identity != "author" {
			t.Fatalf("Normalise = %v, %v", evs, err)
		}
	}
	// 1 failed + one per identity, then cached.
	if n := e.srv.Count(githubtest.RouteApp); n != 3 {
		t.Errorf("%d GET /app, want 3", n)
	}
}

func TestTier(t *testing.T) {
	e := worldEnv(t)
	c := e.connect(t)
	ctx := context.Background()
	tests := []struct {
		login, assoc, want string
	}{
		{"admin", "MEMBER", "maintainer"}, // group: orgRole admin
		{"ADMIN", "", "maintainer"},       // logins compare case-insensitively
		{"alice", "MEMBER", "docs"},       // group: team (the unknown team source is skipped)
		{"carol", "CONTRIBUTOR", "contributor"},
		{"dave", "collaborator", "contributor"},
		{"carol", "", "anyone"}, // association unknown (not the author)
		{"ccf-release-bot[bot]", "NONE", "bot"},
		{"dependabot[bot]", "NONE", "anyone"},
		{"someone", "NONE", "anyone"},
	}
	for _, tt := range tests {
		got, err := c.Tier(ctx, tt.login, tt.assoc)
		if err != nil || got != tt.want {
			t.Errorf("Tier(%s, %s) = %q, %v; want %q", tt.login, tt.assoc, got, err, tt.want)
		}
	}

	// Matchers are ANDed; with no rule matching, DefaultTier.
	e2 := worldEnv(t)
	e2.p.Spec.Trust = []v1alpha1.TrustRule{{Tier: "member-bot", Group: "maintainers", Bots: []string{"admin"}}, {Tier: "contributor", AuthorAssociation: []string{"CONTRIBUTOR"}}}
	c2 := e2.connect(t)
	for login, want := range map[string]string{"admin": "member-bot", "alice": github.DefaultTier} {
		if got, err := c2.Tier(ctx, login, "MEMBER"); err != nil || got != want {
			t.Errorf("ANDed Tier(%s) = %q, %v; want %q", login, got, err, want)
		}
	}

	// Group members are cached for the Group's TTL (default 10 minutes).
	before := e.srv.Count(githubtest.RouteMembers)
	_, _ = c.Tier(ctx, "admin", "")
	if n := e.srv.Count(githubtest.RouteMembers); n != before {
		t.Errorf("cached: %d member listings, want %d", n, before)
	}
	e.clk.Advance(github.DefaultGroupTTL)
	_, _ = c.Tier(ctx, "admin", "")
	if n := e.srv.Count(githubtest.RouteMembers); n != before+1 {
		t.Errorf("after the TTL: %d member listings, want %d", n, before+1)
	}

	// A GitHub failure resolving a group is an error (the delivery is retried), not a lower tier.
	e3 := worldEnv(t)
	c3 := e3.connect(t)
	e3.srv.FailNext(githubtest.RouteMembers, githubtest.Failure{Status: 502})
	if _, err := c3.Tier(ctx, "admin", ""); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("members down: %v, want retryable", err)
	}

	// No GroupLookup: group rules never match.
	e4 := worldEnv(t)
	e4.adapter = github.NewAdapter(github.Options{HTTPClient: e4.srv.Client(), Clock: e4.clk})
	c4 := e4.connect(t)
	if got, _ := c4.Tier(ctx, "admin", ""); got != "anyone" {
		t.Errorf("no groups: %q, want anyone", got)
	}
}

func TestResolveGroup(t *testing.T) {
	e := worldEnv(t)
	for i := range 5 {
		e.srv.AddOrgMember(org, string(rune('m'+i))+"-member", "member")
	}
	e.srv.SetPageSize(2)
	c := e.connect(t)
	tests := []struct {
		name string
		src  v1alpha1.GroupSource
		want []string
		err  error
	}{
		{"org admins", v1alpha1.GroupSource{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: org, Role: "admin"}}, []string{"admin"}, nil},
		{"org members", v1alpha1.GroupSource{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: org, Role: "member"}},
			[]string{"admin", "alice", "bob", "m-member", "n-member", "o-member", "p-member", "q-member"}, nil},
		{"team", v1alpha1.GroupSource{Provider: prov, Team: org + "/docs"}, []string{"alice", "bob"}, nil},
		{"users", v1alpha1.GroupSource{Provider: prov, Users: []string{"y", "x", "y"}}, []string{"x", "y"}, nil},
		{"unknown team", v1alpha1.GroupSource{Provider: prov, Team: org + "/nope"}, nil, provider.ErrNotFound},
		{"org not installed", v1alpha1.GroupSource{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: "elsewhere", Role: "admin"}}, nil, provider.ErrNotFound},
		{"bad role", v1alpha1.GroupSource{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: org, Role: "owner"}}, nil, provider.ErrInvalid},
		{"bad team", v1alpha1.GroupSource{Provider: prov, Team: "docs"}, nil, provider.ErrInvalid},
		{"user group", v1alpha1.GroupSource{Provider: prov, UserGroup: "@x"}, nil, provider.ErrUnsupported},
		{"two sources", v1alpha1.GroupSource{Provider: prov, Team: org + "/docs", Users: []string{"u"}}, nil, provider.ErrInvalid},
		{"no source", v1alpha1.GroupSource{Provider: prov}, nil, provider.ErrInvalid},
		{"other provider", v1alpha1.GroupSource{Provider: "slack-ccf", Users: []string{"u"}}, nil, provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ResolveGroup(context.Background(), tt.src)
			if !errors.Is(err, tt.err) {
				t.Fatalf("ResolveGroup err = %v, want %v", err, tt.err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResolveGroup = %v, want %v", got, tt.want)
			}
		})
	}
	// The member listings use one org token with members: read only, and no repositories.
	var orgBodies []string
	for _, r := range e.srv.TokenRequests() {
		if !r.HasRepositories {
			orgBodies = append(orgBodies, string(r.Body))
		}
	}
	if len(orgBodies) != 1 || orgBodies[0] != `{"permissions":{"members":"read"}}` {
		t.Errorf("org token requests = %v, want one {members: read}", orgBodies)
	}
}

// TestEndToEndPoCRules evaluates normalised deliveries through the PoC catalog and checks they
// reach the decisions the rules fixtures expect for the hand-written Events.
func TestEndToEndPoCRules(t *testing.T) {
	cat, errs := catalog.LoadDir(os.DirFS("../../catalog/testdata/poc"), catalog.Options{})
	if cat == nil {
		t.Fatalf("catalog: %v", errs)
	}
	groups := map[string]v1alpha1.GroupSpec{}
	for name, g := range cat.Groups {
		groups[name] = g.Spec
	}
	e := worldEnv(t)
	e.adapter = github.NewAdapter(github.Options{HTTPClient: e.srv.Client(), Clock: e.clk, Groups: provider.StaticGroups(groups)})
	p := *cat.Providers[prov]
	gh := *p.Spec.GitHub
	gh.APIURL = e.srv.URL
	gh.Identities = e.p.Spec.GitHub.Identities
	p.Spec.GitHub = &gh
	e.secrets[gh.Webhook.SecretRef.Name+"/"+gh.Webhook.SecretRef.Key] = []byte(hookSecret)
	e.p = p
	c := e.connect(t)

	cases := os.DirFS("../../rules/testdata/cases")
	for _, tt := range []struct{ fixture, webhook, delivery string }{
		{"poc/plan-maintainer-epics", "issues.opened-maintainer", "gh-1"},
		{"poc/fix-release-docs-review", "pull_request_review.submitted", "gh-10"},
		{"poc/train-prod", "issues.closed-train", "gh-5"},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			f, err := rules.ReadFixture(cases, tt.fixture)
			if err != nil {
				t.Fatal(err)
			}
			evs, err := c.Normalise(context.Background(), rawEvent(t, tt.webhook, tt.delivery))
			if err != nil || len(evs) != 1 {
				t.Fatalf("Normalise = %v, %v", evs, err)
			}
			if evs[0].Data.Object.Ref != f.Event.Data.Object.Ref || evs[0].Type != f.Event.Type ||
				evs[0].Data.Actor.IsFactory != f.Event.Data.Actor.IsFactory {
				t.Fatalf("normalised %+v, the fixture has %+v", evs[0], f.Event)
			}
			f.Event = evs[0]
			got, err := f.Evaluate(context.Background(), cat)
			if err != nil {
				t.Fatal(err)
			}
			b, err := rules.Golden(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, f.Expected) {
				t.Errorf("decisions differ from %s/expected.json:\n got %s\nwant %s", tt.fixture, b, f.Expected)
			}
		})
	}
}

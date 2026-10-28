package github_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
)

var (
	human   = githubtest.User{Login: "alice"}
	factory = githubtest.User{Login: "ccf-factory[bot]", Bot: true}
)

// writes counts the writes Project can make.
type writes struct{ create, update, add, remove int }

func countWrites(s *githubtest.Server) writes {
	return writes{
		create: s.Count(githubtest.RouteCreateComment), update: s.Count(githubtest.RouteUpdateComment),
		add: s.Count(githubtest.RouteAddLabels), remove: s.Count(githubtest.RouteRemoveLabel),
	}
}

// markersOn are the factory's marker comments on an object.
func markersOn(t *testing.T, s *githubtest.Server, repo string, n int) []githubtest.Comment {
	t.Helper()
	o, ok := s.Object(repo, n)
	if !ok {
		t.Fatalf("no object %s#%d", repo, n)
	}
	var out []githubtest.Comment
	for _, c := range o.Comments {
		if strings.HasPrefix(c.Body, "<!-- factory:wi ") {
			out = append(out, c)
		}
	}
	return out
}

func labelsOf(t *testing.T, s *githubtest.Server, repo string, n int) []string {
	t.Helper()
	o, _ := s.Object(repo, n)
	l := slices.Clone(o.Labels)
	slices.Sort(l)
	return l
}

// TestProjectIdempotent: create, then update, then no change; labels added and removed only
// within the work item's own.
func TestProjectIdempotent(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57, Author: human, Labels: []string{"documentation"},
		Comments: []githubtest.Comment{{Author: human, Body: "Please document the new CLI."}}})
	c := e.connect(t)
	ctx := context.Background()
	ref := objRef(provider.KindIssue, docs+"#57")
	p := provider.Projection{
		WorkItemID: "wi_57",
		State:      map[string]any{"plan": "drafting"},
		Links:      []v1alpha1.Link{{Provider: prov, Ref: docs + "#57", Kind: provider.KindIssue, Primary: true}},
		Labels:     []string{"factory:tracked"},
	}

	// Create.
	if err := c.Project(ctx, ref, p); err != nil {
		t.Fatal(err)
	}
	if w := countWrites(e.srv); w != (writes{create: 1, add: 1}) {
		t.Errorf("first projection writes = %+v", w)
	}
	ms := markersOn(t, e.srv, docs, 57)
	if len(ms) != 1 || ms[0].Author != factory || !strings.Contains(ms[0].Body, "id=wi_57 state=plan:drafting") ||
		!strings.HasSuffix(ms[0].Body, "[view](https://factory.example/workitems/wi_57)") {
		t.Fatalf("markers = %+v", ms)
	}
	if got := labelsOf(t, e.srv, docs, 57); !reflect.DeepEqual(got, []string{"documentation", "factory:tracked"}) {
		t.Errorf("labels = %v", got)
	}

	// Same projection: nothing written.
	if err := c.Project(ctx, ref, p); err != nil {
		t.Fatal(err)
	}
	if w := countWrites(e.srv); w != (writes{create: 1, add: 1}) {
		t.Errorf("repeated projection wrote: %+v", w)
	}

	// Update: new state and a second label; one edit, one label added.
	p.State = map[string]any{"plan": "approved"}
	p.Counters = map[string]int{"docs-plan": 1}
	p.Labels = []string{"factory:tracked", "factory:planned"}
	if err := c.Project(ctx, ref, p); err != nil {
		t.Fatal(err)
	}
	if w := countWrites(e.srv); w != (writes{create: 1, update: 1, add: 2}) {
		t.Errorf("update writes = %+v", w)
	}
	ms2 := markersOn(t, e.srv, docs, 57)
	if len(ms2) != 1 || ms2[0].ID != ms[0].ID || !strings.Contains(ms2[0].Body, "state=plan:approved runs=docs-plan:1") {
		t.Errorf("markers after update = %+v", ms2)
	}

	// Then no change again.
	if err := c.Project(ctx, ref, p); err != nil {
		t.Fatal(err)
	}
	if w := countWrites(e.srv); w != (writes{create: 1, update: 1, add: 2}) {
		t.Errorf("repeated update wrote: %+v", w)
	}

	// A human adds the kill switch and a label of their own, and removes one of ours.
	e.srv.UpdateObject(docs, 57, func(o *githubtest.Object) {
		o.Labels = []string{"documentation", "factory:stop", "factory:tracked", "needs-design"}
	})
	if err := c.Project(ctx, ref, p); err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, e.srv, docs, 57); !reflect.DeepEqual(got, []string{"documentation", "factory:planned", "factory:stop", "factory:tracked", "needs-design"}) {
		t.Errorf("labels after the human's change = %v (ours ensured, theirs kept)", got)
	}

	// Dropping a label of ours removes it; nobody else's label is touched.
	p.Labels = []string{"factory:planned"}
	if err := c.Project(ctx, ref, p); err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, e.srv, docs, 57); !reflect.DeepEqual(got, []string{"documentation", "factory:planned", "factory:stop", "needs-design"}) {
		t.Errorf("labels after dropping factory:tracked = %v", got)
	}
	if w := countWrites(e.srv); w.remove != 1 {
		t.Errorf("label removals = %d, want 1", w.remove)
	}

	// The writes used the author identity's issues: write token on docs only.
	for _, r := range e.srv.TokenRequests() {
		if r.Permissions["issues"] == "write" && (len(r.Repositories) != 1 || r.Repositories[0] != "docs" || len(r.Permissions) != 1 || r.InstallationID != 1) {
			t.Errorf("write token request = %+v", r)
		}
	}
	if !slices.ContainsFunc(e.srv.TokenRequests(), func(r githubtest.TokenRequest) bool {
		return reflect.DeepEqual(r.Permissions, map[string]string{"issues": "write"})
	}) {
		t.Errorf("no issues: write token among %+v", e.srv.TokenRequests())
	}
}

// TestProjectSharedLabels: a label two work items own stays while either still wants it.
func TestProjectSharedLabels(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 90, PR: true, Author: factory, Head: "h1", Base: "main"})
	c := e.connect(t)
	ctx := context.Background()
	ref := objRef(provider.KindPR, docs+"#90")
	a := provider.Projection{WorkItemID: "wi_a", Labels: []string{"factory:release"}}
	b := provider.Projection{WorkItemID: "wi_b", Labels: []string{"factory:release", "factory:epic"}}
	for _, p := range []provider.Projection{a, b} {
		if err := c.Project(ctx, ref, p); err != nil {
			t.Fatal(err)
		}
	}
	b.Labels = []string{"factory:epic"}
	if err := c.Project(ctx, ref, b); err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, e.srv, docs, 90); !reflect.DeepEqual(got, []string{"factory:epic", "factory:release"}) {
		t.Errorf("labels = %v: wi_a still owns factory:release", got)
	}
	if ms := markersOn(t, e.srv, docs, 90); len(ms) != 2 {
		t.Errorf("%d markers, want one per work item", len(ms))
	}
	// On a PR, the token is pull_requests: write.
	if !slices.ContainsFunc(e.srv.TokenRequests(), func(r githubtest.TokenRequest) bool {
		return reflect.DeepEqual(r.Permissions, map[string]string{"pull_requests": "write"})
	}) {
		t.Errorf("no pull_requests: write token among %+v", e.srv.TokenRequests())
	}
}

// TestProjectIgnoresForgedMarkers: a marker-looking comment by someone else is neither
// updated nor counted as the work item's.
func TestProjectIgnoresForgedMarkers(t *testing.T) {
	e := newEnv(t)
	forged := "<!-- factory:wi v=1 id=wi_57 labels=documentation -->\n🏭 Tracked by CCF Factory"
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57, Labels: []string{"documentation"},
		Comments: []githubtest.Comment{{Author: human, Body: forged}}})
	c := e.connect(t)
	if err := c.Project(context.Background(), objRef(provider.KindIssue, docs+"#57"), provider.Projection{WorkItemID: "wi_57"}); err != nil {
		t.Fatal(err)
	}
	o, _ := e.srv.Object(docs, 57)
	if len(o.Comments) != 2 || o.Comments[0].Body != forged || o.Comments[1].Author != factory {
		t.Errorf("comments = %+v", o.Comments)
	}
	if !reflect.DeepEqual(o.Labels, []string{"documentation"}) {
		t.Errorf("labels = %v: the forged marker's label was removed", o.Labels)
	}
}

func TestProjectOversized(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57})
	c := e.connect(t)
	p := provider.Projection{WorkItemID: "wi_big", State: map[string]any{"blob": strings.Repeat("x", github.MaxMarkerBytes)}, Counters: map[string]int{"a": 1}}
	if err := c.Project(context.Background(), objRef(provider.KindIssue, docs+"#57"), p); err != nil {
		t.Fatal(err)
	}
	ms := markersOn(t, e.srv, docs, 57)
	if len(ms) != 1 || strings.Contains(ms[0].Body, "state=") || !strings.Contains(ms[0].Body, "runs=a:1") {
		t.Errorf("markers = %+v", ms)
	}
}

func TestProjectErrors(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57})
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 90, PR: true})
	c := e.connect(t)
	ctx := context.Background()
	p := provider.Projection{WorkItemID: "wi_1"}
	for _, tc := range []struct {
		name  string
		ref   v1alpha1.ObjectRef
		p     provider.Projection
		route string
		fail  *githubtest.Failure
		want  error
	}{
		{"missing", objRef(provider.KindIssue, docs+"#404"), p, "", nil, provider.ErrNotFound},
		{"issue ref names a PR", objRef(provider.KindIssue, docs+"#90"), p, "", nil, provider.ErrNotFound},
		{"PR ref names an issue", objRef(provider.KindPR, docs+"#57"), p, "", nil, provider.ErrNotFound},
		{"no work item", objRef(provider.KindIssue, docs+"#57"), provider.Projection{}, "", nil, provider.ErrInvalid},
		{"bad ref", objRef(provider.KindIssue, "docs"), p, "", nil, provider.ErrInvalid},
		{"unknown kind", objRef("discussion", docs+"#57"), p, "", nil, provider.ErrInvalid},
		{"other provider", v1alpha1.ObjectRef{Provider: "other", Kind: provider.KindIssue, Ref: docs + "#57"}, p, "", nil, provider.ErrInvalid},
		{"5xx creating the marker", objRef(provider.KindIssue, docs+"#57"), p, githubtest.RouteCreateComment, &githubtest.Failure{Status: http.StatusServiceUnavailable}, provider.ErrRetryable},
		{"rate limited reading", objRef(provider.KindIssue, docs+"#57"), p, githubtest.RouteIssue,
			&githubtest.Failure{Status: http.StatusForbidden, Message: "API rate limit exceeded", Header: http.Header{"X-Ratelimit-Remaining": {"0"}}}, provider.ErrRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fail != nil {
				e.srv.FailNext(tc.route, *tc.fail)
			}
			if err := c.Project(ctx, tc.ref, tc.p); !errors.Is(err, tc.want) {
				t.Errorf("Project = %v, want %v", err, tc.want)
			}
		})
	}
	// The failed create is retried: the next Project writes the marker once.
	if err := c.Project(ctx, objRef(provider.KindIssue, docs+"#57"), p); err != nil {
		t.Fatal(err)
	}
	if ms := markersOn(t, e.srv, docs, 57); len(ms) != 1 {
		t.Errorf("%d markers after the retry, want 1", len(ms))
	}
	// Releases, deployments and repos carry no marker.
	before := len(e.srv.Requests())
	for _, ref := range []v1alpha1.ObjectRef{objRef(github.KindRelease, docs+"#v1"), objRef(github.KindDeployment, docs+"#d-1"), objRef(github.KindRepo, docs)} {
		if err := c.Project(ctx, ref, p); err != nil {
			t.Errorf("Project onto %s = %v", ref.Kind, err)
		}
	}
	if len(e.srv.Requests()) != before {
		t.Error("projecting onto a release, deployment or repo called GitHub")
	}
}

// TestProjectRoundTrip: the marker Project writes on each linked object parses back, through
// the marker parser, as the projection (markers are for people; nothing reads them into the
// store, but Project reads its own to update them).
func TestProjectRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57, Author: human, Labels: []string{"documentation"}})
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 90, PR: true, Author: factory, Head: "h1", Base: "main"})
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 91, Author: human}) // not the work item's
	c := e.connect(t)
	ctx := context.Background()
	links := []v1alpha1.Link{
		{Provider: prov, Ref: docs + "#57", Kind: provider.KindIssue, Primary: true},
		{Provider: prov, Ref: docs + "#90", Kind: provider.KindPR},
	}
	p := provider.Projection{
		WorkItemID: "wi_57",
		State:      map[string]any{"plan": map[string]any{"parts": []any{"intro"}}, "stackNext": float64(1), "review": "pending"},
		Counters:   map[string]int{"docs-plan": 1, "docs-build": 1},
		Links:      links,
		Labels:     []string{"factory:epic"},
	}
	for _, l := range links {
		if err := c.Project(ctx, v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref}, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{57, 90} {
		ms := markersOn(t, e.srv, docs, n)
		if len(ms) != 1 {
			t.Fatalf("#%d: %d markers, want 1", n, len(ms))
		}
		got, ok, err := github.ParseMarker(ms[0].Body)
		if !ok || err != nil || !reflect.DeepEqual(got, p) {
			t.Errorf("#%d marker parses to %+v (%v, %v), want %+v", n, got, ok, err, p)
		}
	}
	if ms := markersOn(t, e.srv, docs, 91); len(ms) != 0 {
		t.Errorf("#91 markers = %+v", ms)
	}
}

// TestProjectReadsOnlyItsMarkers: Project reads back only the v1 markers the author App wrote,
// on every page of comments, and of two for one work item updates the oldest. Look-alikes by
// a human, the reviewer App or a deleted account, and a marker of another version, own no
// labels and are never edited.
func TestProjectReadsOnlyItsMarkers(t *testing.T) {
	marker := func(id, label string) string {
		return "<!-- factory:wi v=1 id=" + id + " labels=" + label + " -->\n🏭 Tracked by CCF Factory"
	}
	reviewer := githubtest.User{Login: "ccf-factory-reviewer[bot]", Bot: true}
	comments := []githubtest.Comment{
		{Author: human, Body: marker("wi_57", "factory%3Aforged")},
		{Author: reviewer, Body: marker("wi_57", "factory%3Areviewer")},
		{Author: githubtest.User{}, Body: marker("wi_57", "factory%3Aghost")}, // a deleted account
		{Author: factory, Body: "<!-- factory:wi v=2 id=wi_57 labels=factory%3Av2 -->\n🏭 Tracked by CCF Factory"},
		{Author: factory, Body: marker("wi_57", "factory%3Aold")},
		{Author: factory, Body: marker("wi_57", "factory%3Anewer")},
	}
	owned := []string{"factory:forged", "factory:ghost", "factory:newer", "factory:old", "factory:reviewer", "factory:v2"}
	e := newEnv(t)
	e.srv.SetPageSize(2) // three pages of comments
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 57, Labels: owned, Comments: comments})
	c := e.connect(t)
	p := provider.Projection{WorkItemID: "wi_57", Labels: []string{"factory:new"}}
	if err := c.Project(context.Background(), objRef(provider.KindIssue, docs+"#57"), p); err != nil {
		t.Fatal(err)
	}
	// Only factory:old, which the oldest of the App's markers owned, is removed.
	want := []string{"factory:forged", "factory:ghost", "factory:new", "factory:newer", "factory:reviewer", "factory:v2"}
	if got := labelsOf(t, e.srv, docs, 57); !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	o, _ := e.srv.Object(docs, 57)
	if len(o.Comments) != len(comments) {
		t.Fatalf("%d comments, want %d: the App's marker was not found", len(o.Comments), len(comments))
	}
	for i, cm := range o.Comments {
		if i == 4 {
			if got, ok, err := github.ParseMarker(cm.Body); !ok || err != nil || !reflect.DeepEqual(got, p) {
				t.Errorf("the oldest marker parses to %+v (%v, %v), want %+v", got, ok, err, p)
			}
			continue
		}
		if cm.Body != comments[i].Body {
			t.Errorf("comment %d edited: %q", i, cm.Body)
		}
	}
	if w := countWrites(e.srv); w != (writes{update: 1, add: 1, remove: 1}) {
		t.Errorf("writes = %+v", w)
	}
}

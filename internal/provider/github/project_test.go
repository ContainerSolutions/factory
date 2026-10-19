package github_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github"
	"github.com/compliance-framework/factory/internal/provider/github/githubtest"
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

// TestRebuildRoundTrip: what Project writes, List finds and Observe reads back as the
// projection (contracts.md: rebuild = List → Observe → Projections).
func TestRebuildRoundTrip(t *testing.T) {
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
	// The PR is merged and closed later: it still carries its marker, so List finds it.
	e.srv.UpdateObject(docs, 90, func(o *githubtest.Object) { o.State = "merged" })

	refs, err := c.List(ctx, v1alpha1.Scope{Repos: []string{docs}})
	if err != nil {
		t.Fatal(err)
	}
	want := []v1alpha1.ObjectRef{objRef(provider.KindIssue, docs+"#57"), objRef(provider.KindPR, docs+"#90"), objRef(provider.KindIssue, docs+"#91")}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("List = %+v, want %+v", refs, want)
	}
	got := map[string][]provider.Projection{}
	for _, r := range refs {
		o, err := c.Observe(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		got[r.Ref] = o.Projections
	}
	for _, l := range links {
		if !reflect.DeepEqual(got[l.Ref], []provider.Projection{p}) {
			t.Errorf("%s projections = %+v, want %+v", l.Ref, got[l.Ref], p)
		}
	}
	if got[docs+"#91"] != nil {
		t.Errorf("#91 projections = %+v", got[docs+"#91"])
	}
}

func TestList(t *testing.T) {
	e := newEnv(t)
	e.p.Spec.Scope = &v1alpha1.ProviderScope{Orgs: []string{org}}
	now := t0
	old := now.Add(-github.ListWindow - 24*time.Hour)
	marker := githubtest.Comment{Author: factory, Body: "<!-- factory:wi v=1 id=wi_x -->\n🏭 Tracked by CCF Factory"}
	for _, o := range []githubtest.Object{
		{Repo: docs, Number: 1},
		{Repo: docs, Number: 2, PR: true},
		{Repo: docs, Number: 3, State: "closed", Comments: []githubtest.Comment{marker}},
		{Repo: docs, Number: 4, State: "closed"},
		{Repo: docs, Number: 5, UpdatedAt: old},
		{Repo: docs, Number: 6, State: "merged", PR: true, Comments: []githubtest.Comment{marker}, UpdatedAt: old},
		{Repo: org + "/mock-docs", Number: 1},
		{Repo: org + "/mock-docs", Number: 2},
		{Repo: org + "/mock-docs", Number: 3},
		{Repo: org + "/mock-api", Number: 1},
		{Repo: org + "/workflows", Number: 9},
	} {
		e.srv.AddObject(o)
	}
	e.srv.AddAccountRepos(org, "docs", "mock-docs", "mock-api", "workflows")
	e.srv.SetPageSize(2)
	c := e.connect(t)
	ctx := context.Background()
	refs := func(rs ...string) []v1alpha1.ObjectRef {
		var out []v1alpha1.ObjectRef
		for _, r := range rs {
			kind := provider.KindIssue
			if r == docs+"#2" {
				kind = provider.KindPR
			}
			out = append(out, objRef(kind, r))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		scope v1alpha1.Scope
		want  []v1alpha1.ObjectRef
	}{
		// Open ones in the window, and closed ones with a marker; not #4 (closed), #5 and #6
		// (too old). Three pages of open ones.
		{"one repo", v1alpha1.Scope{Repos: []string{docs}}, refs(docs+"#1", docs+"#2", docs+"#3")},
		{"glob and exclude", v1alpha1.Scope{Repos: []string{org + "/mock-*"}, Exclude: []string{org + "/mock-api"}},
			refs(org+"/mock-docs#1", org+"/mock-docs#2", org+"/mock-docs#3")},
		{"owner glob uses the Provider's orgs", v1alpha1.Scope{Repos: []string{"*/workflows"}}, refs(org + "/workflows#9")},
		{"outside the Provider's orgs", v1alpha1.Scope{Repos: []string{"someone-else/tool"}}, nil},
		{"every repo", v1alpha1.Scope{Exclude: []string{org + "/{docs,mock-*}"}}, refs(org + "/workflows#9")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.List(ctx, tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("List = %+v, want %+v", got, tc.want)
			}
		})
	}
	if n := e.srv.Count(githubtest.RouteSearch); n < 6 {
		t.Errorf("%d searches: pagination not exercised", n)
	}
	for _, r := range e.srv.TokenRequests() {
		if r.HasRepositories && !reflect.DeepEqual(r.Permissions, map[string]string{"checks": "read", "issues": "read", "pull_requests": "read"}) {
			t.Errorf("List token request %+v", r)
		}
	}
}

func TestListErrors(t *testing.T) {
	e := newEnv(t)
	e.srv.AddObject(githubtest.Object{Repo: docs, Number: 1})
	c := e.connect(t) // no Provider scope.orgs
	ctx := context.Background()
	for name, scope := range map[string]v1alpha1.Scope{
		"owner glob without orgs": {Repos: []string{"*/docs"}},
		"empty without orgs":      {},
		"bad pattern":             {Repos: []string{org + "/{docs"}},
		"bad exclude":             {Repos: []string{docs}, Exclude: []string{"re:("}},
	} {
		if _, err := c.List(ctx, scope); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	e.srv.FailNext(githubtest.RouteSearch, githubtest.Failure{Status: http.StatusForbidden, Message: "You have exceeded a secondary rate limit", Header: http.Header{"Retry-After": {"60"}}})
	if _, err := c.List(ctx, v1alpha1.Scope{Repos: []string{docs}}); !errors.Is(err, provider.ErrRetryable) {
		t.Errorf("rate limited search: %v, want ErrRetryable", err)
	}
	// A repo the App isn't installed on is skipped.
	got, err := c.List(ctx, v1alpha1.Scope{Repos: []string{docs, "someone-else/tool"}})
	if err != nil || !reflect.DeepEqual(got, []v1alpha1.ObjectRef{objRef(provider.KindIssue, docs+"#1")}) {
		t.Errorf("List = %+v, %v", got, err)
	}
}

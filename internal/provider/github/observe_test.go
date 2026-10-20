package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github"
	"github.com/compliance-framework/factory/internal/provider/github/githubtest"
)

const (
	workflows   = org + "/workflows"
	mergeCommit = "abc1230000000000000000000000000000000089"
	deployedSHA = "de9100000000000000000000000000000000beef"
)

// observeCases are the recorded GraphQL responses under testdata/graphql: <name>.json answers
// Observe for ref, <name>.comments-2.json and <name>.threads-2.json the follow-up pages.
var observeCases = []struct {
	name string
	ref  v1alpha1.ObjectRef
}{
	{"issue-epic", objRef(provider.KindIssue, docs+"#57")},
	{"issue-train", objRef(provider.KindIssue, workflows+"#123")},
	{"pr-review", objRef(provider.KindPR, docs+"#90")},
	{"pr-draft", objRef(provider.KindPR, docs+"#91")},
	{"pr-merged", objRef(provider.KindPR, docs+"#89")},
	{"pr-closed", objRef("", docs+"#88")},
}

func objRef(kind, ref string) v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{Provider: prov, Kind: kind, Ref: ref}
}

func readGraphQL(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "graphql", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// observeEnv is newEnv with the recorded responses canned and the deployments the merged PR's
// observation reads.
func observeEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	for _, tc := range observeCases {
		repo, num, _ := strings.Cut(tc.ref.Ref, "#")
		owner, name, _ := strings.Cut(repo, "/")
		n, _ := strconv.Atoi(num)
		main := readGraphQL(t, tc.name)
		e.srv.SetGraphQL("Observe", map[string]any{"owner": owner, "name": name, "number": n}, main)
		var obj struct {
			Data struct {
				Repository struct {
					Object struct {
						ID       string `json:"id"`
						Comments struct {
							PageInfo struct {
								EndCursor string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"comments"`
						Threads struct {
							PageInfo struct {
								EndCursor string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"issueOrPullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(main, &obj); err != nil {
			t.Fatal(err)
		}
		o := obj.Data.Repository.Object
		for suffix, follow := range map[string]struct{ op, cursor string }{
			"comments-2": {"ObserveComments", o.Comments.PageInfo.EndCursor},
			"threads-2":  {"ObserveThreads", o.Threads.PageInfo.EndCursor},
		} {
			b, err := os.ReadFile(filepath.Join("testdata", "graphql", tc.name+"."+suffix+".json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			e.srv.SetGraphQL(follow.op, map[string]any{"id": o.ID, "after": follow.cursor}, b)
		}
	}
	// docs' Pages deployments: the latest of main is what Observe reports; a newer one to
	// another environment and one of another ref don't count.
	e.srv.AddDeployment(docs, githubtest.Deployment{ID: 700, SHA: "0ld0000000000000000000000000000000000000", Ref: "main", Environment: github.DefaultDeployEnvironment,
		Statuses: []githubtest.DeploymentStatus{{State: "inactive"}}})
	e.srv.AddDeployment(docs, githubtest.Deployment{ID: 701, SHA: deployedSHA, Ref: "main", Environment: github.DefaultDeployEnvironment,
		Statuses: []githubtest.DeploymentStatus{{State: "success", EnvironmentURL: "https://docs.compliance-framework.example/"}, {State: "in_progress"}}})
	e.srv.AddDeployment(docs, githubtest.Deployment{ID: 702, SHA: "pr0000000000000000000000000000000000000f", Ref: "fx/preview", Environment: github.DefaultDeployEnvironment})
	e.srv.AddDeployment(docs, githubtest.Deployment{ID: 703, SHA: "57a9000000000000000000000000000000000000", Ref: "main", Environment: "staging"})
	e.srv.SetAncestor(docs, mergeCommit, deployedSHA)
	return e
}

// TestObserveGolden observes each recorded response into its golden Observation,
// testdata/golden/observe/<name>.json (-update rewrites them).
func TestObserveGolden(t *testing.T) {
	e := observeEnv(t)
	c := e.connect(t)
	for _, tc := range observeCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Observe(context.Background(), tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, '\n')
			golden := filepath.Join("testdata", "golden", "observe", tc.name+".json")
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

// TestObserveFields checks what the rules read independently of the goldens, so a wrong
// -update can't pass silently.
func TestObserveFields(t *testing.T) {
	e := observeEnv(t)
	c := e.connect(t)
	ctx := context.Background()
	observe := func(ref v1alpha1.ObjectRef) provider.Observation {
		t.Helper()
		o, err := c.Observe(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	t.Run("approvals", func(t *testing.T) {
		pr := observe(objRef(provider.KindPR, docs+"#90")).PR
		// The reviewer App's latest opinion is its approval of the head (its comments after it
		// don't change it), the author App's approval maps to "author", bob's approval was
		// dismissed, carol's review is pending and a deleted account counts for nobody.
		want := []provider.Approval{
			{Identity: "alice", Commit: "c0ffee0000000000000000000000000000000001"},
			{Identity: "author", Commit: "c0ffee0000000000000000000000000000000002"},
			{Identity: "reviewer", Commit: "c0ffee0000000000000000000000000000000003"},
		}
		if !reflect.DeepEqual(pr.ApprovedBy, want) {
			t.Errorf("approvedBy = %+v, want %+v", pr.ApprovedBy, want)
		}
		if pr.Head != want[2].Commit || pr.Base != "main" || pr.Draft {
			t.Errorf("head/base/draft = %s %s %v", pr.Head, pr.Base, pr.Draft)
		}
		if pr.UnresolvedThreads != 3 {
			t.Errorf("unresolvedThreads = %d, want 3 (2 on the first page, 1 on the second)", pr.UnresolvedThreads)
		}
		if pr.Checks != (provider.Checks{State: "success", Settled: true}) {
			t.Errorf("checks = %+v", pr.Checks)
		}
	})

	t.Run("pr author and state", func(t *testing.T) {
		o := observe(objRef(provider.KindPR, docs+"#90"))
		if o.State != provider.StateOpen || o.Issue != nil || o.Deploy != nil {
			t.Errorf("state %s, issue %v, deploy %v", o.State, o.Issue, o.Deploy)
		}
		if a := o.Author; a == nil || *a != (v1alpha1.Actor{ID: prov + ":ccf-factory[bot]", IsBot: true, IsFactory: true, Identity: "author"}) {
			t.Errorf("author = %+v", a)
		}
		draft := observe(objRef(provider.KindPR, docs+"#91"))
		if !draft.PR.Draft || draft.PR.Checks != (provider.Checks{State: "pending"}) || draft.PR.ApprovedBy != nil {
			t.Errorf("draft PR = %+v", draft.PR)
		}
		if a := draft.Author; a == nil || a.IsFactory || a.IsBot || a.ID != prov+":dave" {
			t.Errorf("draft author = %+v", a)
		}
		closed := observe(objRef("", docs+"#88"))
		if closed.Ref.Kind != provider.KindPR || closed.State != provider.StateClosed || closed.PR.Checks != (provider.Checks{}) || closed.PR.MergeCommit != "" {
			t.Errorf("closed PR = %+v %+v", closed, closed.PR)
		}
	})

	t.Run("merged PR and deploy", func(t *testing.T) {
		o := observe(objRef(provider.KindPR, docs+"#89"))
		if o.State != provider.StateMerged || o.PR.MergeCommit != mergeCommit || !o.PR.MergedAt.Equal(time.Date(2026, 10, 16, 9, 29, 58, 0, time.UTC)) {
			t.Errorf("merged PR = %s %+v", o.State, o.PR)
		}
		want := &provider.DeployObservation{State: "success", SHA: deployedSHA, URL: "https://docs.compliance-framework.example/", ContainsMerge: true}
		if !reflect.DeepEqual(o.Deploy, want) {
			t.Errorf("deploy = %+v, want %+v", o.Deploy, want)
		}
	})

	t.Run("issue", func(t *testing.T) {
		o := observe(objRef(provider.KindIssue, docs+"#57"))
		want := &provider.IssueObservation{
			SubIssues: []v1alpha1.ObjectRef{objRef(provider.KindIssue, docs+"#58"), objRef(provider.KindIssue, docs+"#59")},
			LinkedPRs: []v1alpha1.ObjectRef{objRef(provider.KindPR, docs+"#90"), objRef(provider.KindPR, org+"/mock-docs#12")},
			Comments:  6,
		}
		if !reflect.DeepEqual(o.Issue, want) || o.PR != nil {
			t.Errorf("issue = %+v, want %+v", o.Issue, want)
		}
		if !reflect.DeepEqual(o.Labels, []string{"documentation", "factory:epic"}) || o.State != provider.StateOpen {
			t.Errorf("labels %v state %s", o.Labels, o.State)
		}
	})

	t.Run("projections", func(t *testing.T) {
		// Only the author App's v1 markers count: mallory's look-alike, the v2 marker and the
		// deleted account's are ignored, and of two markers for wi_epic57 the oldest wins.
		ps := observe(objRef(provider.KindIssue, docs+"#57")).Projections
		want := []provider.Projection{{
			WorkItemID: "wi_epic57",
			State: map[string]any{
				"plan": map[string]any{"parts": []any{"intro", "install"}, "size": float64(2)}, "stackNext": float64(1),
				"stackTotal": float64(2), "review": "pending",
			},
			Counters: map[string]int{"docs-plan": 1, "docs-build": 2},
			Links: []v1alpha1.Link{
				{Provider: prov, Ref: docs + "#57", Kind: provider.KindIssue, Primary: true},
				{Provider: prov, Ref: docs + "#90", Kind: provider.KindPR},
			},
			Labels: []string{"factory:epic"},
		}}
		if !reflect.DeepEqual(ps, want) {
			t.Errorf("projections = %+v, want %+v", ps, want)
		}
		// A marker on the second page of comments is read.
		train := observe(objRef(provider.KindIssue, workflows+"#123"))
		if len(train.Projections) != 1 || train.Projections[0].WorkItemID != "wi_train123" ||
			train.Projections[0].State["train"] != workflows+"#123" || train.State != provider.StateClosed {
			t.Errorf("train projections = %+v", train.Projections)
		}
		if a := train.Author; a == nil || !a.IsBot || a.IsFactory || a.ID != prov+":ccf-release-bot[bot]" {
			t.Errorf("train author = %+v", a)
		}
	})

	t.Run("tokens", func(t *testing.T) {
		// Observe reads with the author identity's read-only token on the object's repo (the
		// commits it reaches need contents, their check suites checks; never statuses, which
		// the App isn't granted), and the deployment with contents and deployments read.
		var sawObserve, sawDeploy bool
		for _, r := range e.srv.TokenRequests() {
			switch {
			case reflect.DeepEqual(r.Permissions, map[string]string{"checks": "read", "contents": "read", "issues": "read", "pull_requests": "read"}):
				sawObserve = true
			case reflect.DeepEqual(r.Permissions, map[string]string{"contents": "read", "deployments": "read"}):
				sawDeploy = true
			case r.Permissions["members"] != "":
			default:
				t.Errorf("unexpected token request %+v", r)
			}
			if r.Permissions["members"] == "" && (len(r.Repositories) != 1 || r.InstallationID != 1) {
				t.Errorf("token request %+v: want one repo on the author installation", r)
			}
		}
		if !sawObserve || !sawDeploy {
			t.Errorf("token requests = %+v", e.srv.TokenRequests())
		}
	})
}

func TestObserveErrors(t *testing.T) {
	e := observeEnv(t)
	e.srv.SetGraphQL("Observe", map[string]any{"owner": org, "name": "docs", "number": 404}, readGraphQL(t, "not-found"))
	e.srv.SetGraphQL("Observe", map[string]any{"owner": org, "name": "docs", "number": 429},
		[]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation ID 1."}]}`))
	// As seen live when the token lacked contents: one FORBIDDEN error per commit reached.
	e.srv.SetGraphQL("Observe", map[string]any{"owner": org, "name": "docs", "number": 403},
		[]byte(`{"data":{"repository":{"issueOrPullRequest":{"commits":{"nodes":[{"commit":null}]}}}},"errors":[`+
			`{"type":"FORBIDDEN","path":["repository","issueOrPullRequest","commits","nodes",0,"commit"],"message":"Resource not accessible by integration"},`+
			`{"type":"FORBIDDEN","path":["repository","issueOrPullRequest","reviews","nodes",0,"commit"],"message":"Resource not accessible by integration"},`+
			`{"type":"FORBIDDEN","path":["repository","issueOrPullRequest","mergeCommit"],"message":"Resource not accessible by integration"}]}`))
	c := e.connect(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ref  v1alpha1.ObjectRef
		fail *githubtest.Failure
		want error
	}{
		{"missing", objRef(provider.KindIssue, docs+"#404"), nil, provider.ErrNotFound},
		{"issue ref names a PR", objRef(provider.KindIssue, docs+"#90"), nil, provider.ErrNotFound},
		{"PR ref names an issue", objRef(provider.KindPR, docs+"#57"), nil, provider.ErrNotFound},
		{"rate limited", objRef(provider.KindIssue, docs+"#429"), nil, provider.ErrRetryable},
		{"forbidden", objRef(provider.KindPR, docs+"#403"), nil, provider.ErrForbidden},
		{"5xx", objRef(provider.KindIssue, docs+"#57"), &githubtest.Failure{Status: http.StatusBadGateway, Message: "Server Error"}, provider.ErrRetryable},
		{"secondary rate limit", objRef(provider.KindIssue, docs+"#57"), &githubtest.Failure{Status: http.StatusForbidden, Message: "You have exceeded a secondary rate limit", Header: http.Header{"Retry-After": {"30"}}}, provider.ErrRetryable},
		{"bad ref", objRef(provider.KindIssue, "docs-57"), nil, provider.ErrInvalid},
		{"unknown kind", objRef("discussion", docs+"#57"), nil, provider.ErrInvalid},
		{"other provider", v1alpha1.ObjectRef{Provider: "github-enterprise", Kind: provider.KindIssue, Ref: docs + "#57"}, nil, provider.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fail != nil {
				e.srv.FailNext(githubtest.RouteGraphQL, *tc.fail)
			}
			if _, err := c.Observe(ctx, tc.ref); !errors.Is(err, tc.want) {
				t.Errorf("Observe = %v, want %v", err, tc.want)
			}
		})
	}
	// The App not installed on the repo: the token lookup says not found.
	if _, err := c.Observe(ctx, objRef(provider.KindIssue, "someone-else/tool#1")); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("not installed: %v, want ErrNotFound", err)
	}
}

func TestObserveOtherKinds(t *testing.T) {
	e := newEnv(t)
	c := e.connect(t)
	ctx := context.Background()
	repo, err := c.Observe(ctx, objRef(github.KindRepo, docs))
	if err != nil || repo.State != provider.StateOpen || repo.Ref.Kind != github.KindRepo {
		t.Errorf("repo = %+v, %v", repo, err)
	}
	before := len(e.srv.Requests())
	for _, ref := range []v1alpha1.ObjectRef{objRef(github.KindRelease, docs+"#v1.4.0"), objRef(github.KindDeployment, docs+"#d-701")} {
		o, err := c.Observe(ctx, ref)
		if err != nil || o.State != provider.StateClosed || o.Issue != nil || o.PR != nil {
			t.Errorf("%s = %+v, %v", ref.Kind, o, err)
		}
	}
	if len(e.srv.Requests()) != before {
		t.Error("observing a release or a deployment called GitHub")
	}
	if _, err := c.Observe(ctx, objRef(github.KindRepo, docs+"#1")); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("repo ref with a number: %v, want ErrInvalid", err)
	}
}

// TestObserveChecks rolls a PR head's check suites up into checks{state, settled}, the way the
// merge rules read item.pr.checks.state.
func TestObserveChecks(t *testing.T) {
	suite := func(status, conclusion string, runs int) githubtest.CheckSuite {
		return githubtest.CheckSuite{Status: status, Conclusion: conclusion, Runs: runs}
	}
	for i, tc := range []struct {
		name   string
		suites []githubtest.CheckSuite
		want   provider.Checks
	}{
		{"no suites", nil, provider.Checks{}},
		{"only suites without runs", []githubtest.CheckSuite{suite("QUEUED", "", 0)}, provider.Checks{}},
		{"success", []githubtest.CheckSuite{suite("COMPLETED", "SUCCESS", 3)}, provider.Checks{State: "success", Settled: true}},
		{"neutral and skipped pass", []githubtest.CheckSuite{suite("COMPLETED", "NEUTRAL", 1), suite("COMPLETED", "SKIPPED", 2), suite("COMPLETED", "SUCCESS", 1)},
			provider.Checks{State: "success", Settled: true}},
		{"an empty queued suite doesn't hold it", []githubtest.CheckSuite{suite("COMPLETED", "SUCCESS", 2), suite("QUEUED", "", 0)},
			provider.Checks{State: "success", Settled: true}},
		{"in progress", []githubtest.CheckSuite{suite("COMPLETED", "SUCCESS", 2), suite("IN_PROGRESS", "", 1)}, provider.Checks{State: "pending"}},
		{"queued with runs", []githubtest.CheckSuite{suite("QUEUED", "", 1)}, provider.Checks{State: "pending"}},
		{"failure", []githubtest.CheckSuite{suite("COMPLETED", "FAILURE", 2)}, provider.Checks{State: "failure", Settled: true}},
		{"failure wins over pending", []githubtest.CheckSuite{suite("IN_PROGRESS", "", 1), suite("COMPLETED", "FAILURE", 1)},
			provider.Checks{State: "failure", Settled: true}},
		{"cancelled", []githubtest.CheckSuite{suite("COMPLETED", "CANCELLED", 1)}, provider.Checks{State: "failure", Settled: true}},
		{"timed out", []githubtest.CheckSuite{suite("COMPLETED", "TIMED_OUT", 1)}, provider.Checks{State: "failure", Settled: true}},
		{"action required", []githubtest.CheckSuite{suite("COMPLETED", "ACTION_REQUIRED", 1)}, provider.Checks{State: "failure", Settled: true}},
		{"startup failure", []githubtest.CheckSuite{suite("COMPLETED", "STARTUP_FAILURE", 1)}, provider.Checks{State: "failure", Settled: true}},
		{"stale", []githubtest.CheckSuite{suite("COMPLETED", "STALE", 1)}, provider.Checks{State: "failure", Settled: true}},
		{"unknown conclusion", []githubtest.CheckSuite{suite("COMPLETED", "SOMETHING_NEW", 1)}, provider.Checks{State: "pending"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			n := 100 + i
			e.srv.AddObject(githubtest.Object{Repo: docs, Number: n, PR: true, State: "open", Author: githubtest.User{Login: "dave"},
				Head: "c0ffee00000000000000000000000000000000aa", Base: "main", CheckSuites: tc.suites})
			o, err := e.connect(t).Observe(context.Background(), objRef(provider.KindPR, docs+"#"+strconv.Itoa(n)))
			if err != nil {
				t.Fatal(err)
			}
			if o.PR == nil || o.PR.Checks != tc.want {
				t.Errorf("checks = %+v, want %+v", o.PR, tc.want)
			}
		})
	}
}

// TestObserveFieldPermissions checks the query against the permissions its fields need (the
// githubtest server answers FORBIDDEN as GitHub does): Observe's token covers them, and an
// installation without one of them fails with ErrInvalid before any GitHub call, not
// FORBIDDEN on every PR.
func TestObserveFieldPermissions(t *testing.T) {
	ctx := context.Background()
	ref := objRef(provider.KindPR, docs+"#60")
	pr := githubtest.Object{Repo: docs, Number: 60, PR: true, State: "open", Author: githubtest.User{Login: "dave"}, Head: "c0ffee00000000000000000000000000000000bb", Base: "main",
		CheckSuites: []githubtest.CheckSuite{{Status: "COMPLETED", Conclusion: "SUCCESS", Runs: 1}}}
	e := newEnv(t)
	e.srv.AddObject(pr)
	if o, err := e.connect(t).Observe(ctx, ref); err != nil || o.PR.Checks.State != "success" {
		t.Fatalf("Observe = %+v, %v", o.PR, err)
	}
	if q := e.srv.GraphQLRequests(); len(q) != 1 || q[0].Operation != "Observe" {
		t.Errorf("GraphQL requests = %+v", q)
	}
	for _, missing := range []string{"contents", "checks"} {
		t.Run("installation without "+missing, func(t *testing.T) {
			e := newEnv(t)
			e.srv.AddObject(pr)
			perms := map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read", "members": "read", "checks": "read"}
			delete(perms, missing)
			e.srv.SetInstallationPermissions(1, perms)
			_, err := e.connect(t).Observe(ctx, ref)
			if !errors.Is(err, provider.ErrInvalid) || !strings.Contains(err.Error(), missing) {
				t.Errorf("Observe = %v, want ErrInvalid naming %s", err, missing)
			}
			if n := e.srv.Count(githubtest.RouteGraphQL); n != 0 {
				t.Errorf("%d GraphQL calls, want none", n)
			}
		})
	}
}

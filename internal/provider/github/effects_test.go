package github_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
)

var (
	reviewerBot = githubtest.User{Login: "ccf-factory-reviewer[bot]", Bot: true}
	bob         = githubtest.User{Login: "bob"}
)

// The key the Effector derives (eff_<sha256(task, key)[:20]>), as Apply sees it.
const effKey = "eff_0123456789abcdef0123456789abcdef01234567"

// seedEffects adds the objects the effect tests act on: issues #57..#59 and the factory's PR
// #60 (head fx/one at abc123, one review thread).
func seedEffects(s *githubtest.Server) {
	s.AddObject(githubtest.Object{Repo: docs, Number: 57, Title: "Translate the docs", Body: "Please translate.", Author: human,
		Labels: []string{"documentation", "stale"}})
	s.AddObject(githubtest.Object{Repo: docs, Number: 58, Title: "Translate the intro", Author: human})
	s.AddObject(githubtest.Object{Repo: docs, Number: 59, Title: "Translate the docs again", Author: bob})
	s.AddObject(githubtest.Object{Repo: docs, Number: 60, PR: true, Title: "docs: intro", Body: "Part 1.", Author: factory,
		Head: "abc123", HeadRef: "fx/one", Base: "main",
		Threads: []githubtest.Thread{{ID: "PRRT_1", Comments: []githubtest.Comment{{Author: human, Body: "Typo here."}}}}})
}

func effectReq(op v1alpha1.EffectOp, kind, ref, identity string, args map[string]any) provider.EffectRequest {
	return provider.EffectRequest{
		Op: op, Target: v1alpha1.ObjectRef{Provider: prov, Kind: kind, Ref: ref}, Args: args, Identity: identity, IdempotencyKey: effKey,
	}
}

// writeCounts counts the requests on every write route.
func writeCounts(s *githubtest.Server) map[string]int {
	out := map[string]int{}
	for _, r := range []string{
		githubtest.RouteCreateComment, githubtest.RouteUpdateComment, githubtest.RouteAddLabels, githubtest.RouteRemoveLabel,
		githubtest.RouteCreateIssue, githubtest.RouteUpdateIssue, githubtest.RouteAddSubIssue, githubtest.RouteCreatePull,
		githubtest.RouteUpdatePull, githubtest.RouteMerge, githubtest.RouteCreateReview,
	} {
		out[r] = s.Count(r)
	}
	for _, q := range s.GraphQLRequests() {
		if strings.HasPrefix(q.Operation, "ApplyThreadReply") || strings.HasPrefix(q.Operation, "ApplyResolveThread") {
			out["graphql:"+q.Operation]++
		}
	}
	return out
}

func marker(key string) string { return "<!-- factory:effect " + key + " -->" }

func obj(t *testing.T, s *githubtest.Server, n int) githubtest.Object {
	t.Helper()
	o, ok := s.Object(docs, n)
	if !ok {
		t.Fatalf("no object %s#%d", docs, n)
	}
	return o
}

// TestApplyOps applies every API-side op against githubtest, checks what it did, the token it
// used (the request's identity, the one target repo, the minimal permissions), and that a
// replay with the same key writes nothing and returns the same result with Replayed.
func TestApplyOps(t *testing.T) {
	issueWrite := map[string]string{"issues": "write"}
	prWrite := map[string]string{"pull_requests": "write"}
	tests := []struct {
		name   string
		seed   func(*githubtest.Server)
		req    provider.EffectRequest
		perms  map[string]string
		writes map[string]int // the first Apply's writes
		object string         // the result's object ref
		url    string         // a prefix of the result's URL
		check  func(*testing.T, *githubtest.Server)
	}{
		{
			name:   "comment",
			req:    effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", map[string]any{"body": "Planned."}),
			perms:  issueWrite,
			writes: map[string]int{githubtest.RouteCreateComment: 1},
			object: docs + "#57", url: githubtest.WebURL + "/" + docs + "/issues/57#issuecomment-",
			check: func(t *testing.T, s *githubtest.Server) {
				cs := obj(t, s, 57).Comments
				if len(cs) != 1 || cs[0].Author != factory || cs[0].Body != "Planned.\n\n"+marker(effKey) {
					t.Errorf("comments %+v", cs)
				}
			},
		},
		{
			name:   "pr.comment as the reviewer",
			req:    effectReq(v1alpha1.OpPRComment, provider.KindPR, docs+"#60", "reviewer", map[string]any{"body": "Looks good."}),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteCreateComment: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60#issuecomment-",
			check: func(t *testing.T, s *githubtest.Server) {
				cs := obj(t, s, 60).Comments
				if len(cs) != 1 || cs[0].Author != reviewerBot {
					t.Errorf("comments %+v", cs)
				}
			},
		},
		{
			name:   "label.add",
			req:    effectReq(v1alpha1.OpLabelAdd, provider.KindIssue, docs+"#57", "author", map[string]any{"labels": []any{"needs-human", "Documentation"}, "label": "plan:drafting"}),
			perms:  issueWrite,
			writes: map[string]int{githubtest.RouteAddLabels: 1},
			object: docs + "#57", url: githubtest.WebURL + "/" + docs + "/issues/57",
			check: func(t *testing.T, s *githubtest.Server) {
				if got := labelsOf(t, s, docs, 57); !slices.Equal(got, []string{"documentation", "needs-human", "plan:drafting", "stale"}) {
					t.Errorf("labels %v", got)
				}
			},
		},
		{
			name:   "label.remove",
			req:    effectReq(v1alpha1.OpLabelRemove, provider.KindIssue, docs+"#57", "author", map[string]any{"labels": []any{"STALE", "absent"}}),
			perms:  issueWrite,
			writes: map[string]int{githubtest.RouteRemoveLabel: 1},
			object: docs + "#57", url: githubtest.WebURL + "/" + docs + "/issues/57",
			check: func(t *testing.T, s *githubtest.Server) {
				if got := labelsOf(t, s, docs, 57); !slices.Equal(got, []string{"documentation"}) {
					t.Errorf("labels %v", got)
				}
			},
		},
		{
			name:   "create-issue",
			req:    effectReq(v1alpha1.OpCreateIssue, "", docs, "author", map[string]any{"title": "Translate the guides", "body": "Scope: guides.", "labels": []any{"documentation"}}),
			perms:  issueWrite,
			writes: map[string]int{githubtest.RouteCreateIssue: 1},
			object: docs + "#61", url: githubtest.WebURL + "/" + docs + "/issues/61",
			check: func(t *testing.T, s *githubtest.Server) {
				o := obj(t, s, 61)
				if o.PR || o.Author != factory || o.Title != "Translate the guides" || o.Body != "Scope: guides.\n\n"+marker(effKey) ||
					!slices.Equal(o.Labels, []string{"documentation"}) {
					t.Errorf("issue %+v", o)
				}
			},
		},
		{
			name:   "sub-issue.add",
			req:    effectReq(v1alpha1.OpSubIssueAdd, provider.KindIssue, docs+"#57", "author", map[string]any{"issue": docs + "#58"}),
			perms:  issueWrite,
			writes: map[string]int{githubtest.RouteAddSubIssue: 1},
			object: docs + "#57", url: githubtest.WebURL + "/" + docs + "/issues/57",
			check: func(t *testing.T, s *githubtest.Server) {
				if got := obj(t, s, 57).SubIssues; !slices.Equal(got, []int{58}) {
					t.Errorf("sub-issues %v", got)
				}
			},
		},
		{
			name:   "close",
			req:    effectReq(v1alpha1.OpClose, provider.KindIssue, docs+"#58", "author", map[string]any{"reason": "not_planned"}),
			perms:  issueWrite,
			writes: map[string]int{githubtest.RouteUpdateIssue: 1},
			object: docs + "#58", url: githubtest.WebURL + "/" + docs + "/issues/58",
			check: func(t *testing.T, s *githubtest.Server) {
				if o := obj(t, s, 58); o.State != "closed" || o.StateReason != "not_planned" {
					t.Errorf("state %s %s", o.State, o.StateReason)
				}
			},
		},
		{
			name:  "link-duplicate",
			req:   effectReq(v1alpha1.OpLinkDuplicate, provider.KindIssue, docs+"#59", "author", map[string]any{"duplicateOf": float64(57)}),
			perms: issueWrite,
			writes: map[string]int{
				githubtest.RouteCreateComment: 1, githubtest.RouteAddLabels: 1, githubtest.RouteUpdateIssue: 1,
			},
			object: docs + "#59", url: githubtest.WebURL + "/" + docs + "/issues/59",
			check: func(t *testing.T, s *githubtest.Server) {
				o := obj(t, s, 59)
				if o.State != "closed" || o.StateReason != "not_planned" || !slices.Equal(o.Labels, []string{"duplicate"}) ||
					len(o.Comments) != 1 || o.Comments[0].Body != "Duplicate of #57\n\n"+marker(effKey) {
					t.Errorf("issue %+v", o)
				}
			},
		},
		{
			name: "pr.open",
			req: effectReq(v1alpha1.OpPROpen, "", docs+"#57", "author", map[string]any{
				"head": "fx/two", "base": "fx/one", "title": "docs: guides", "body": "Part 2.", "draft": true, "labels": []any{"factory:tracked"},
			}),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteCreatePull: 1, githubtest.RouteAddLabels: 1},
			object: docs + "#61", url: githubtest.WebURL + "/" + docs + "/pull/61",
			check: func(t *testing.T, s *githubtest.Server) {
				o := obj(t, s, 61)
				if !o.PR || o.Author != factory || o.HeadRef != "fx/two" || o.Base != "fx/one" || !o.Draft ||
					o.Body != "Part 2.\n\n"+marker(effKey) || !slices.Equal(o.Labels, []string{"factory:tracked"}) {
					t.Errorf("PR %+v", o)
				}
			},
		},
		{
			name: "pr.update keeps the markers",
			seed: func(s *githubtest.Server) {
				s.UpdateObject(docs, 60, func(o *githubtest.Object) { o.Body = "Part 1.\n\n" + marker("eff_open") })
			},
			req:    effectReq(v1alpha1.OpPRUpdate, provider.KindPR, docs+"#60", "author", map[string]any{"title": "docs: the intro", "body": "Part 1 of 3."}),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteUpdatePull: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60",
			check: func(t *testing.T, s *githubtest.Server) {
				if o := obj(t, s, 60); o.Title != "docs: the intro" || o.Body != "Part 1 of 3.\n\n"+marker("eff_open") {
					t.Errorf("PR %q %q", o.Title, o.Body)
				}
			},
		},
		{
			name:   "pr.retarget",
			req:    effectReq(v1alpha1.OpPRRetarget, provider.KindPR, docs+"#60", "reviewer", map[string]any{"base": "release"}),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteUpdatePull: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60",
			check: func(t *testing.T, s *githubtest.Server) {
				if o := obj(t, s, 60); o.Base != "release" {
					t.Errorf("base %q", o.Base)
				}
			},
		},
		{
			name: "pr.merge with the repo's method",
			seed: func(s *githubtest.Server) {
				s.SetRepo(docs, githubtest.Repo{AllowMergeCommit: true, AllowRebaseMerge: true})
			},
			req:    effectReq(v1alpha1.OpPRMerge, provider.KindPR, docs+"#60", "reviewer", nil),
			perms:  map[string]string{"contents": "write", "pull_requests": "write"},
			writes: map[string]int{githubtest.RouteMerge: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60",
			check: func(t *testing.T, s *githubtest.Server) {
				if o := obj(t, s, 60); o.State != "merged" || o.MergeMethod != "merge" {
					t.Errorf("PR %s by %q", o.State, o.MergeMethod)
				}
			},
		},
		{
			name: "review.comment with inline comments",
			req: effectReq(v1alpha1.OpReviewComment, provider.KindPR, docs+"#60", "reviewer", map[string]any{
				"body": "A few notes.", "comments": []any{map[string]any{"path": "docs/intro.md", "line": float64(3), "body": "Typo."}},
			}),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteCreateReview: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60#pullrequestreview-",
			check: func(t *testing.T, s *githubtest.Server) {
				rs := obj(t, s, 60).Reviews
				want := []githubtest.ReviewComment{{Path: "docs/intro.md", Line: 3, Body: "Typo."}}
				if len(rs) != 1 || rs[0].Author != reviewerBot || rs[0].State != "COMMENTED" || rs[0].CommitID != "abc123" ||
					rs[0].Body != "A few notes.\n\n"+marker(effKey) || !reflect.DeepEqual(rs[0].Comments, want) {
					t.Errorf("reviews %+v", rs)
				}
			},
		},
		{
			name:   "review.request-changes",
			req:    effectReq(v1alpha1.OpReviewRequestChanges, provider.KindPR, docs+"#60", "reviewer", map[string]any{"body": "Fix the typo.", "commit": "abc123"}),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteCreateReview: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60#pullrequestreview-",
			check: func(t *testing.T, s *githubtest.Server) {
				if rs := obj(t, s, 60).Reviews; len(rs) != 1 || rs[0].State != "CHANGES_REQUESTED" {
					t.Errorf("reviews %+v", rs)
				}
			},
		},
		{
			name:   "review.approve",
			req:    effectReq(v1alpha1.OpReviewApprove, provider.KindPR, docs+"#60", "reviewer", nil),
			perms:  prWrite,
			writes: map[string]int{githubtest.RouteCreateReview: 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60#pullrequestreview-",
			check: func(t *testing.T, s *githubtest.Server) {
				if rs := obj(t, s, 60).Reviews; len(rs) != 1 || rs[0].State != "APPROVED" || rs[0].Body != marker(effKey) {
					t.Errorf("reviews %+v", rs)
				}
			},
		},
		{
			name:   "thread.reply",
			req:    effectReq(v1alpha1.OpThreadReply, provider.KindPR, docs+"#60", "author", map[string]any{"thread": "PRRT_1", "body": "Fixed."}),
			perms:  prWrite,
			writes: map[string]int{"graphql:ApplyThreadReply": 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60#discussion_r",
			check: func(t *testing.T, s *githubtest.Server) {
				cs := obj(t, s, 60).Threads[0].Comments
				if len(cs) != 2 || cs[1].Author != factory || cs[1].Body != "Fixed.\n\n"+marker(effKey) {
					t.Errorf("thread %+v", cs)
				}
			},
		},
		{
			name:   "thread.resolve",
			req:    effectReq(v1alpha1.OpThreadResolve, provider.KindPR, docs+"#60", "reviewer", map[string]any{"thread": "PRRT_1"}),
			perms:  prWrite,
			writes: map[string]int{"graphql:ApplyResolveThread": 1},
			object: docs + "#60", url: githubtest.WebURL + "/" + docs + "/pull/60",
			check: func(t *testing.T, s *githubtest.Server) {
				if !obj(t, s, 60).Threads[0].Resolved {
					t.Error("thread not resolved")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedEffects(e.srv)
			if tt.seed != nil {
				tt.seed(e.srv)
			}
			c := e.connect(t)
			ctx := context.Background()
			before := writeCounts(e.srv)

			res, err := c.Apply(ctx, tt.req)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if res.Replayed || res.Object == nil || res.Object.Ref != tt.object || res.Object.Provider != prov || !strings.HasPrefix(res.URL, tt.url) {
				t.Errorf("result %+v (object %+v), want object %s and URL %s…", res, res.Object, tt.object, tt.url)
			}
			after := writeCounts(e.srv)
			diff := map[string]int{}
			for k, v := range after {
				if d := v - before[k]; d != 0 {
					diff[k] = d
				}
			}
			if !maps.Equal(diff, tt.writes) {
				t.Errorf("writes %v, want %v", diff, tt.writes)
			}
			tt.check(t, e.srv)

			// Every token: the request's identity's App, the one repo, the minimal permissions.
			wantInstall := int64(1)
			if tt.req.Identity == "reviewer" {
				wantInstall = 2
			}
			trs := e.srv.TokenRequests()
			if len(trs) == 0 {
				t.Fatal("no token minted")
			}
			for _, tr := range trs {
				if tr.InstallationID != wantInstall || !slices.Equal(tr.Repositories, []string{"docs"}) || !maps.Equal(tr.Permissions, tt.perms) {
					t.Errorf("token request %+v, want installation %d, [docs], %v", tr, wantInstall, tt.perms)
				}
			}

			// The replay writes nothing and returns the same object and URL.
			again, err := c.Apply(ctx, tt.req)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if !again.Replayed || again.URL != res.URL || !reflect.DeepEqual(again.Object, res.Object) || again.Outcome != res.Outcome {
				t.Errorf("replay %+v, want %+v with Replayed", again, res)
			}
			if w := writeCounts(e.srv); !maps.Equal(w, after) {
				t.Errorf("the replay wrote: %v, then %v", after, w)
			}
		})
	}
}

// A look-alike marker by someone else doesn't make a key replayed: the App's comment is
// still written, and only it counts afterwards.
func TestApplyForgedMarker(t *testing.T) {
	e := newEnv(t)
	seedEffects(e.srv)
	e.srv.UpdateObject(docs, 57, func(o *githubtest.Object) {
		o.Comments = append(o.Comments, githubtest.Comment{ID: 900, Author: human, Body: "fake\n\n" + marker(effKey)})
	})
	c := e.connect(t)
	req := effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", map[string]any{"body": "Planned."})
	res, err := c.Apply(context.Background(), req)
	if err != nil || res.Replayed {
		t.Fatalf("Apply = %+v, %v; want a new comment", res, err)
	}
	if cs := obj(t, e.srv, 57).Comments; len(cs) != 2 || cs[1].Author != factory {
		t.Errorf("comments %+v", cs)
	}
	if again, err := c.Apply(context.Background(), req); err != nil || !again.Replayed || again.URL != res.URL {
		t.Errorf("replay = %+v, %v", again, err)
	}
	// Another key is another comment.
	other := req
	other.IdempotencyKey = "evt/gh-1/0/plan-maintainer-epics/0"
	if res, err := c.Apply(context.Background(), other); err != nil || res.Replayed {
		t.Errorf("other key = %+v, %v", res, err)
	}
	if cs := obj(t, e.srv, 57).Comments; len(cs) != 3 || !strings.HasSuffix(cs[2].Body, marker("evt%2Fgh-1%2F0%2Fplan-maintainer-epics%2F0")) {
		t.Errorf("comments %+v", cs)
	}
}

// pr.merge: blocked by review requirements it falls back to the merge label (Q5); without one
// it is refused; other refusals are typed.
func TestApplyMerge(t *testing.T) {
	ctx := context.Background()
	merge := func(args map[string]any) provider.EffectRequest {
		return effectReq(v1alpha1.OpPRMerge, provider.KindPR, docs+"#60", "reviewer", args)
	}
	t.Run("fallback to the merge label", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		e.srv.UpdateObject(docs, 60, func(o *githubtest.Object) { o.MergeBlock = githubtest.MergeReviewRequired })
		c := e.connect(t)
		req := merge(map[string]any{"mergeLabel": "automerge"})
		res, err := c.Apply(ctx, req)
		if err != nil || res.Outcome != github.OutcomeLabelledForMerge || res.Replayed {
			t.Fatalf("Apply = %+v, %v; want labelled-for-merge", res, err)
		}
		if o := obj(t, e.srv, 60); o.State != "open" || !slices.Equal(o.Labels, []string{"automerge"}) || o.MergeMethod != "" {
			t.Errorf("PR %s %v %q", o.State, o.Labels, o.MergeMethod)
		}
		if o := obj(t, e.srv, 60); e.srv.Count(githubtest.RouteMerge) != 1 || o.MergeMethod != "" {
			t.Errorf("merge attempts %d", e.srv.Count(githubtest.RouteMerge))
		}
		again, err := c.Apply(ctx, req)
		if err != nil || !again.Replayed || again.Outcome != github.OutcomeLabelledForMerge || again.URL != res.URL {
			t.Errorf("replay = %+v, %v", again, err)
		}
		if n := e.srv.Count(githubtest.RouteAddLabels); n != 1 {
			t.Errorf("label adds %d, want 1", n)
		}
		// Once the review requirement is met, the same key merges.
		e.srv.UpdateObject(docs, 60, func(o *githubtest.Object) { o.MergeBlock = "" })
		if res, err := c.Apply(ctx, req); err != nil || res.Outcome != "" || res.Replayed || obj(t, e.srv, 60).State != "merged" {
			t.Errorf("after approval = %+v, %v", res, err)
		}
		if res, err := c.Apply(ctx, req); err != nil || !res.Replayed {
			t.Errorf("merged replay = %+v, %v", res, err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		tests := []struct {
			name  string
			seed  func(*githubtest.Object)
			args  map[string]any
			is    []error
			isNot []error
		}{
			{name: "review required, no label", seed: func(o *githubtest.Object) { o.MergeBlock = githubtest.MergeReviewRequired },
				is: []error{github.ErrReviewRequired, github.ErrMergeBlocked, provider.ErrInvalid}, isNot: []error{github.ErrHeadChanged}},
			{name: "not mergeable", seed: func(o *githubtest.Object) { o.MergeBlock = githubtest.MergeNotMergeable },
				args: map[string]any{"mergeLabel": "automerge"},
				is:   []error{github.ErrMergeBlocked, provider.ErrInvalid}, isNot: []error{github.ErrReviewRequired, github.ErrHeadChanged}},
			{name: "head moved", args: map[string]any{"sha": "old000"},
				is: []error{github.ErrHeadChanged, provider.ErrInvalid}, isNot: []error{github.ErrMergeBlocked}},
			{name: "closed", seed: func(o *githubtest.Object) { o.State = "closed" },
				is: []error{github.ErrMergeBlocked, provider.ErrInvalid}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				e := newEnv(t)
				seedEffects(e.srv)
				if tt.seed != nil {
					e.srv.UpdateObject(docs, 60, tt.seed)
				}
				c := e.connect(t)
				_, err := c.Apply(ctx, merge(tt.args))
				for _, want := range tt.is {
					if !errors.Is(err, want) {
						t.Errorf("err = %v, want %v", err, want)
					}
				}
				for _, not := range tt.isNot {
					if errors.Is(err, not) {
						t.Errorf("err = %v, is %v", err, not)
					}
				}
				var me *github.MergeError
				if !errors.As(err, &me) {
					t.Errorf("err = %T, want a *MergeError", err)
				}
				if o := obj(t, e.srv, 60); o.State == "merged" || len(o.Labels) != 0 {
					t.Errorf("PR changed: %s %v", o.State, o.Labels)
				}
			})
		}
	})
	t.Run("only a squash allowed", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		e.srv.SetRepo(docs, githubtest.Repo{AllowSquashMerge: true, AllowRebaseMerge: true})
		c := e.connect(t)
		if _, err := c.Apply(ctx, merge(nil)); err != nil {
			t.Fatal(err)
		}
		if o := obj(t, e.srv, 60); o.MergeMethod != "squash" {
			t.Errorf("method %q", o.MergeMethod)
		}
	})
	t.Run("no method allowed", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		e.srv.SetRepo(docs, githubtest.Repo{})
		c := e.connect(t)
		if _, err := c.Apply(ctx, merge(nil)); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("err = %v, want ErrInvalid", err)
		}
	})
}

// Rate limits and 5xx retry the whole op on the clock: waiting Retry-After, the primary
// limit's reset, or an exponential backoff; bounded, after which the RetryableError (with
// GitHub's hint) is returned.
func TestApplyRetries(t *testing.T) {
	ctx := context.Background()
	comment := effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", map[string]any{"body": "Planned."})
	type result struct {
		res provider.EffectResult
		err error
	}
	start := func(c *github.Conn) chan result {
		done := make(chan result, 1)
		go func() {
			res, err := c.Apply(ctx, comment)
			done <- result{res, err}
		}()
		return done
	}
	// wait advances the clock by d once Apply's backoff timer exists.
	wait := func(e *env, d time.Duration) {
		e.clk.BlockUntil(1)
		e.clk.Advance(d)
	}

	t.Run("Retry-After", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		c := e.connect(t)
		e.srv.FailNext(githubtest.RouteCreateComment, githubtest.Failure{Status: http.StatusForbidden,
			Message: "You have exceeded a secondary rate limit.", Header: http.Header{"Retry-After": {"30"}}})
		done := start(c)
		e.clk.BlockUntil(1)
		e.clk.Advance(29 * time.Second)
		select {
		case r := <-done:
			t.Fatalf("returned before Retry-After: %+v", r)
		case <-time.After(20 * time.Millisecond):
		}
		e.clk.Advance(time.Second)
		r := <-done
		if r.err != nil || r.res.Replayed {
			t.Fatalf("Apply = %+v, %v", r.res, r.err)
		}
		if n := len(obj(t, e.srv, 57).Comments); n != 1 || e.srv.Count(githubtest.RouteCreateComment) != 2 {
			t.Errorf("%d comments after %d creates", n, e.srv.Count(githubtest.RouteCreateComment))
		}
	})
	t.Run("X-RateLimit-Reset", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		c := e.connect(t)
		reset := e.clk.Now().Add(20 * time.Second).Unix()
		e.srv.FailNext(githubtest.RouteComments, githubtest.Failure{Status: http.StatusForbidden, Message: "API rate limit exceeded",
			Header: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {itoa(reset)}}})
		done := start(c)
		wait(e, 20*time.Second)
		if r := <-done; r.err != nil {
			t.Fatalf("Apply: %v", r.err)
		}
		if n := len(obj(t, e.srv, 57).Comments); n != 1 {
			t.Errorf("%d comments", n)
		}
	})
	t.Run("5xx backoff, then success", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		c := e.connect(t)
		for range 2 {
			e.srv.FailNext(githubtest.RouteCreateComment, githubtest.Failure{Status: http.StatusBadGateway, Message: "Server Error"})
		}
		done := start(c)
		wait(e, time.Second)
		wait(e, 2*time.Second)
		if r := <-done; r.err != nil {
			t.Fatalf("Apply: %v", r.err)
		}
		if n := e.srv.Count(githubtest.RouteCreateComment); n != 3 {
			t.Errorf("%d creates, want 3", n)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		c := e.connect(t)
		for range 5 {
			e.srv.FailNext(githubtest.RouteCreateComment, githubtest.Failure{Status: http.StatusServiceUnavailable, Message: "Unavailable"})
		}
		done := start(c)
		for _, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
			wait(e, d)
		}
		r := <-done
		if !errors.Is(r.err, provider.ErrRetryable) {
			t.Fatalf("Apply = %v, want retryable", r.err)
		}
		if n := e.srv.Count(githubtest.RouteCreateComment); n != 5 {
			t.Errorf("%d creates, want 5", n)
		}
		if n := len(obj(t, e.srv, 57).Comments); n != 0 {
			t.Errorf("%d comments", n)
		}
	})
	t.Run("a long Retry-After is the caller's", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		c := e.connect(t)
		e.srv.FailNext(githubtest.RouteCreateComment, githubtest.Failure{Status: http.StatusTooManyRequests,
			Message: "Too Many Requests", Header: http.Header{"Retry-After": {"600"}}})
		_, err := c.Apply(ctx, comment)
		var re *provider.RetryableError
		if !errors.As(err, &re) || re.RetryAfter != 10*time.Minute {
			t.Fatalf("Apply = %v, want retryable after 10m", err)
		}
		if e.clk.Timers() != 0 {
			t.Errorf("%d timers left", e.clk.Timers())
		}
	})
	t.Run("cancelled while waiting", func(t *testing.T) {
		e := newEnv(t)
		seedEffects(e.srv)
		c := e.connect(t)
		e.srv.FailNext(githubtest.RouteCreateComment, githubtest.Failure{Status: http.StatusBadGateway, Message: "Server Error"})
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := c.Apply(cctx, comment)
			done <- err
		}()
		e.clk.BlockUntil(1)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Apply = %v, want cancelled", err)
		}
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestApplyRefused: requests the adapter or GitHub refuses, with the provider's errors.
func TestApplyRefused(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		seed func(*githubtest.Server)
		req  provider.EffectRequest
		is   error
	}{
		{name: "git-side op", req: effectReq(v1alpha1.OpPushBranch, "", docs, "author", nil), is: provider.ErrUnsupported},
		{name: "unknown op", req: effectReq("teleport", "", docs+"#57", "author", nil), is: provider.ErrInvalid},
		{name: "no key", req: func() provider.EffectRequest {
			r := effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", map[string]any{"body": "x"})
			r.IdempotencyKey = ""
			return r
		}(), is: provider.ErrInvalid},
		{name: "another provider", req: func() provider.EffectRequest {
			r := effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", map[string]any{"body": "x"})
			r.Target.Provider = "github-enterprise"
			return r
		}(), is: provider.ErrInvalid},
		{name: "unknown identity", req: effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "admin", map[string]any{"body": "x"}), is: provider.ErrInvalid},
		{name: "approve as the author", req: effectReq(v1alpha1.OpReviewApprove, provider.KindPR, docs+"#60", "author", nil), is: provider.ErrInvalid},
		{name: "a channel", req: effectReq(v1alpha1.OpComment, "channel", "#ccf-dev", "author", map[string]any{"body": "x"}), is: provider.ErrUnsupported},
		{name: "no body", req: effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", nil), is: provider.ErrInvalid},
		{name: "no labels", req: effectReq(v1alpha1.OpLabelAdd, provider.KindIssue, docs+"#57", "author", nil), is: provider.ErrInvalid},
		{name: "bad ref", req: effectReq(v1alpha1.OpComment, provider.KindIssue, "docs#x", "author", map[string]any{"body": "x"}), is: provider.ErrInvalid},
		{name: "missing issue", req: effectReq(v1alpha1.OpLabelAdd, provider.KindIssue, docs+"#999", "author", map[string]any{"label": "x"}), is: provider.ErrNotFound},
		{name: "an issue ref naming a PR", req: effectReq(v1alpha1.OpClose, provider.KindIssue, docs+"#60", "author", nil), is: provider.ErrNotFound},
		{name: "a PR op on an issue", req: effectReq(v1alpha1.OpPRRetarget, provider.KindIssue, docs+"#57", "author", map[string]any{"base": "x"}), is: provider.ErrInvalid},
		{name: "a PR op on an issue number", req: effectReq(v1alpha1.OpPRRetarget, "", docs+"#57", "author", map[string]any{"base": "x"}), is: provider.ErrNotFound},
		{name: "GitHub's 422", seed: func(s *githubtest.Server) {
			s.FailNext(githubtest.RouteCreateIssue, githubtest.Failure{Status: http.StatusUnprocessableEntity, Message: "Validation Failed"})
		}, req: effectReq(v1alpha1.OpCreateIssue, "", docs, "author", map[string]any{"title": "x"}), is: provider.ErrInvalid},
		{name: "no title", req: effectReq(v1alpha1.OpCreateIssue, "", docs, "author", nil), is: provider.ErrInvalid},
		{name: "create-issue on a ref", req: effectReq(v1alpha1.OpCreateIssue, "", "docs", "author", map[string]any{"title": "x"}), is: provider.ErrInvalid},
		{name: "sub-issue in another repo", req: effectReq(v1alpha1.OpSubIssueAdd, provider.KindIssue, docs+"#57", "author", map[string]any{"issue": org + "/website#3"}), is: provider.ErrInvalid},
		{name: "a PR as sub-issue", req: effectReq(v1alpha1.OpSubIssueAdd, provider.KindIssue, docs+"#57", "author", map[string]any{"issue": "#60"}), is: provider.ErrInvalid},
		{name: "bad close reason", req: effectReq(v1alpha1.OpClose, provider.KindIssue, docs+"#57", "author", map[string]any{"reason": "bored"}), is: provider.ErrInvalid},
		{name: "duplicate of itself", req: effectReq(v1alpha1.OpLinkDuplicate, provider.KindIssue, docs+"#57", "author", map[string]any{"duplicateOf": "#57"}), is: provider.ErrInvalid},
		{name: "a human's PR on the head", seed: func(s *githubtest.Server) {
			s.AddObject(githubtest.Object{Repo: docs, Number: 70, PR: true, Author: human, HeadRef: "fx/two", Base: "main"})
		}, req: effectReq(v1alpha1.OpPROpen, "", docs, "author", map[string]any{"head": "fx/two", "base": "main", "title": "x"}), is: provider.ErrInvalid},
		{name: "a thread of another PR", seed: func(s *githubtest.Server) {
			s.AddObject(githubtest.Object{Repo: docs, Number: 71, PR: true, Author: human, Threads: []githubtest.Thread{{ID: "PRRT_9"}}})
		}, req: effectReq(v1alpha1.OpThreadResolve, provider.KindPR, docs+"#60", "reviewer", map[string]any{"thread": "PRRT_9"}), is: provider.ErrInvalid},
		{name: "a missing thread", req: effectReq(v1alpha1.OpThreadResolve, provider.KindPR, docs+"#60", "reviewer", map[string]any{"thread": "PRRT_0"}), is: provider.ErrNotFound},
		{name: "bad inline comment", req: effectReq(v1alpha1.OpReviewComment, provider.KindPR, docs+"#60", "reviewer", map[string]any{
			"body": "x", "comments": []any{map[string]any{"path": "a.md"}},
		}), is: provider.ErrInvalid},
		{name: "oversized body", req: effectReq(v1alpha1.OpComment, provider.KindIssue, docs+"#57", "author", map[string]any{"body": strings.Repeat("x", 65530)}), is: provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedEffects(e.srv)
			if tt.seed != nil {
				tt.seed(e.srv)
			}
			c := e.connect(t)
			before := writeCounts(e.srv)
			if _, err := c.Apply(ctx, tt.req); !errors.Is(err, tt.is) {
				t.Errorf("Apply = %v, want %v", err, tt.is)
			}
			after := writeCounts(e.srv)
			for k, v := range after {
				if k != githubtest.RouteCreateIssue && v != before[k] {
					t.Errorf("wrote on %s", k)
				}
			}
		})
	}
}

// The sub-issue link GitHub reports as already there (a race with the listing) is success.
func TestApplySubIssueAlreadyLinked(t *testing.T) {
	e := newEnv(t)
	seedEffects(e.srv)
	c := e.connect(t)
	e.srv.FailNext(githubtest.RouteAddSubIssue, githubtest.Failure{Status: http.StatusUnprocessableEntity, Message: "Validation Failed: sub-issue already exists"})
	res, err := c.Apply(context.Background(), effectReq(v1alpha1.OpSubIssueAdd, provider.KindIssue, docs+"#57", "author", map[string]any{"issue": "58"}))
	if err != nil || !res.Replayed {
		t.Errorf("Apply = %+v, %v; want replayed", res, err)
	}
}

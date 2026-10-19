package effector

import (
	"context"
	"regexp"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Op is one Effector op: a CLI command (Words), an MCP tool (Name), its params, and what it
// does. Both front-ends are generated from the ops table.
type Op struct {
	// Words are the CLI command after `factory`: {"pr", "open"}.
	Words []string
	// Effects are the effect ops it may send (the review op picks one by its event); empty
	// for local ops.
	Effects []v1alpha1.EffectOp
	Help    string
	Params  []Param
	run     func(ctx context.Context, e *Effector, a args) (map[string]any, error)
}

// Name is the op's MCP tool name: its words joined by "_", with "-" as "_" (tool names
// must suit every harness's function-name rules).
func (o *Op) Name() string {
	return strings.ReplaceAll(strings.Join(o.Words, "_"), "-", "_")
}

// Command is the op's CLI command: "factory pr open".
func (o *Op) Command() string { return "factory " + strings.Join(o.Words, " ") }

// Ops returns the op table, in its fixed order.
func Ops() []*Op { return ops }

func lookup(name string) (*Op, bool) {
	for _, o := range ops {
		if o.Name() == name || strings.Join(o.Words, " ") == name {
			return o, true
		}
	}
	return nil, false
}

// Common params.
var (
	pKey = Param{Name: "key", Help: "a stable key for this logical step (e.g. plan-summary): the effect is applied once per key, " +
		"however often it is sent; without it, a repeated call is a new effect once the previous one succeeded"}
	pTarget = Param{Name: "target", Help: "the object to act on: owner/name#N, or #N or N in the task's repo; default: the task's primary object"}
	pPR     = Param{Name: "pr", Required: true, Help: "the pull request: N, #N or owner/name#N"}
	pBody   = Param{Name: "body", Kind: Text, Required: true, Help: "the text (Markdown)"}
)

func optional(p Param) Param {
	p.Required = false
	return p
}

// reviewComments is the JSON Schema of a review's inline comments.
var reviewComments = map[string]any{
	"type": "array",
	"items": map[string]any{
		"type":     "object",
		"required": []any{"path", "line", "body"},
		"properties": map[string]any{
			"path":      map[string]any{"type": "string"},
			"line":      map[string]any{"type": "integer"},
			"body":      map[string]any{"type": "string"},
			"side":      map[string]any{"type": "string", "enum": []any{"LEFT", "RIGHT"}},
			"startLine": map[string]any{"type": "integer"},
			"startSide": map[string]any{"type": "string", "enum": []any{"LEFT", "RIGHT"}},
		},
	},
}

// Review events and the effect op each sends.
var reviewOps = map[string]v1alpha1.EffectOp{
	"approve":         v1alpha1.OpReviewApprove,
	"request-changes": v1alpha1.OpReviewRequestChanges,
	"comment":         v1alpha1.OpReviewComment,
}

var ops = []*Op{
	{
		Words: []string{"comment"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpComment},
		Help:   "Comment on the task's primary object (or --target).",
		Params: []Param{pBody, pTarget, pKey},
		run: apiOp(v1alpha1.OpComment, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("target"))
			return t, map[string]any{"body": a.str("body")}, err
		}),
	},
	{
		Words: []string{"pr", "comment"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpPRComment},
		Help:   "Comment on a pull request's conversation.",
		Params: []Param{pPR, pBody, pKey},
		run: apiOp(v1alpha1.OpPRComment, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("pr"))
			return t, map[string]any{"body": a.str("body")}, err
		}),
	},
	labelOp("add", v1alpha1.OpLabelAdd, "Add labels to the task's primary object (or --target)."),
	labelOp("remove", v1alpha1.OpLabelRemove, "Remove labels from the task's primary object (or --target)."),
	{
		Words: []string{"issue", "create"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpCreateIssue},
		Help: "Create an issue in the task's repo; prints its ref.",
		Params: []Param{
			{Name: "title", Required: true, Help: "the issue's title"},
			optional(pBody),
			{Name: "labels", Kind: List, Help: "labels to set"},
			pKey,
		},
		run: apiOp(v1alpha1.OpCreateIssue, func(_ *Effector, a args) (string, map[string]any, error) {
			m := map[string]any{"title": a.str("title")}
			setIf(m, "body", a.str("body"))
			if l := a.list("labels"); len(l) > 0 {
				m["labels"] = l
			}
			return "", m, nil
		}),
	},
	{
		Words: []string{"issue", "sub-add"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpSubIssueAdd},
		Help: "Add an issue as a sub-issue of another (same repo).",
		Params: []Param{
			{Name: "parent", Required: true, Help: "the parent issue: N, #N or owner/name#N"},
			{Name: "child", Required: true, Help: "the sub-issue: N, #N or owner/name#N"},
			pKey,
		},
		run: apiOp(v1alpha1.OpSubIssueAdd, func(e *Effector, a args) (string, map[string]any, error) {
			parent, err := e.ref(a.str("parent"))
			if err != nil {
				return "", nil, err
			}
			child, err := e.ref(a.str("child"))
			return parent, map[string]any{"issue": child}, err
		}),
	},
	{
		Words: []string{"issue", "close"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpClose},
		Help: "Close the task's primary object (or --target).",
		Params: []Param{
			pTarget,
			{Name: "reason", Enum: []string{"completed", "not_planned"}, Help: "why it is closed"},
			pKey,
		},
		run: apiOp(v1alpha1.OpClose, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("target"))
			m := map[string]any{}
			setIf(m, "reason", a.str("reason"))
			return t, m, err
		}),
	},
	{
		Words: []string{"issue", "link-duplicate"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpLinkDuplicate},
		Help: "Mark the task's primary object (or --target) as a duplicate: comment, label and close.",
		Params: []Param{
			{Name: "of", Required: true, Help: "the original: N, #N or owner/name#N"},
			pTarget, pKey,
		},
		run: apiOp(v1alpha1.OpLinkDuplicate, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("target"))
			if err != nil {
				return "", nil, err
			}
			of, err := e.ref(a.str("of"))
			return t, map[string]any{"duplicateOf": of}, err
		}),
	},
	{
		Words: []string{"pr", "open"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpPROpen},
		Help: "Open a pull request in the task's repo (the open one for --head is returned if it exists); prints its ref.",
		Params: []Param{
			{Name: "head", Required: true, Help: "the branch with the changes (push it first: factory push)"},
			{Name: "base", Required: true, Help: "the branch to merge into"},
			{Name: "title", Required: true, Help: "the pull request's title"},
			optional(pBody),
			{Name: "draft", Kind: Bool, Help: "open it as a draft"},
			{Name: "labels", Kind: List, Help: "labels to set"},
			pKey,
		},
		run: apiOp(v1alpha1.OpPROpen, func(_ *Effector, a args) (string, map[string]any, error) {
			m := map[string]any{"head": a.str("head"), "base": a.str("base"), "title": a.str("title")}
			setIf(m, "body", a.str("body"))
			if a.flag("draft") {
				m["draft"] = true
			}
			if l := a.list("labels"); len(l) > 0 {
				m["labels"] = l
			}
			return "", m, nil
		}),
	},
	{
		Words: []string{"pr", "update"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpPRUpdate},
		Help: "Change a pull request's title or body.",
		Params: []Param{
			pPR,
			{Name: "title", Help: "the new title"},
			{Name: "body", Kind: Text, Help: "the new body"},
			pKey,
		},
		run: apiOp(v1alpha1.OpPRUpdate, func(e *Effector, a args) (string, map[string]any, error) {
			m := map[string]any{}
			setIf(m, "title", a.str("title"))
			setIf(m, "body", a.str("body"))
			if len(m) == 0 {
				return "", nil, usagef("pr update: give a title or a body")
			}
			t, err := e.ref(a.str("pr"))
			return t, m, err
		}),
	},
	{
		Words: []string{"pr", "retarget"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpPRRetarget},
		Help:   "Change a pull request's base branch.",
		Params: []Param{pPR, {Name: "base", Required: true, Help: "the new base branch"}, pKey},
		run: apiOp(v1alpha1.OpPRRetarget, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("pr"))
			return t, map[string]any{"base": a.str("base")}, err
		}),
	},
	{
		Words: []string{"pr", "merge"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpPRMerge},
		Help: "Merge a pull request with the repo's merge method.",
		Params: []Param{
			pPR,
			{Name: "sha", Help: "the head commit it must still be at (default: the current head)"},
			{Name: "mergeLabel", Help: "the label to add instead when a required review blocks the merge"},
			pKey,
		},
		run: apiOp(v1alpha1.OpPRMerge, func(e *Effector, a args) (string, map[string]any, error) {
			m := map[string]any{}
			setIf(m, "sha", a.str("sha"))
			setIf(m, "mergeLabel", a.str("mergeLabel"))
			t, err := e.ref(a.str("pr"))
			return t, m, err
		}),
	},
	{
		Words:   []string{"review"},
		Effects: []v1alpha1.EffectOp{v1alpha1.OpReviewApprove, v1alpha1.OpReviewRequestChanges, v1alpha1.OpReviewComment},
		Help:    "Review a pull request (reviewer tasks only), with optional inline comments.",
		Params: []Param{
			pPR,
			{Name: "event", Required: true, Enum: []string{"approve", "request-changes", "comment"}, Help: "the review's verdict"},
			{Name: "body", Kind: Text, Help: "the review's text (required but for approve)"},
			{Name: "comments", Kind: JSON, Schema: reviewComments, Help: "inline comments: [{path, line, body, side?, startLine?, startSide?}]"},
			{Name: "commit", Help: "the commit reviewed (default: the head)"},
			pKey,
		},
		run: runReview,
	},
	{
		Words: []string{"thread", "reply"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpThreadReply},
		Help: "Reply to a review thread.",
		Params: []Param{
			{Name: "thread", Required: true, Help: "the review thread's node ID"},
			pBody,
			optional(Param{Name: "pr", Help: "the thread's pull request (default: the task's primary object)"}),
			pKey,
		},
		run: apiOp(v1alpha1.OpThreadReply, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("pr"))
			return t, map[string]any{"thread": a.str("thread"), "body": a.str("body")}, err
		}),
	},
	{
		Words: []string{"thread", "resolve"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpThreadResolve},
		Help: "Resolve a review thread.",
		Params: []Param{
			{Name: "thread", Required: true, Help: "the review thread's node ID"},
			optional(Param{Name: "pr", Help: "the thread's pull request (default: the task's primary object)"}),
			pKey,
		},
		run: apiOp(v1alpha1.OpThreadResolve, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("pr"))
			return t, map[string]any{"thread": a.str("thread")}, err
		}),
	},
	{
		Words: []string{"push"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpPushBranch},
		Help: "Push a branch of the workspace's repo to origin with the task's scoped token, and record it (push-branch).",
		Params: []Param{
			{Name: "branch", Help: "the local branch (default: the current one); pushed to the same name"},
			{Name: "forceWithLease", Kind: Bool, Help: "allow a non-fast-forward push (after a rebase), unless origin moved"},
			pKey,
		},
		run: runPush,
	},
	{
		Words: []string{"stack", "push"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpStackPush},
		Help: "Push every branch of the current stack (gh stack push, or git when the repo has no gh-stack stack), and record it (stack.push).",
		Params: []Param{
			{Name: "base", Help: "the stack's trunk (default: origin's default branch)"},
			{Name: "branches", Kind: List, Help: "the stack's branches, bottom first (git only; default: the local branches between origin/<base> and HEAD)"},
			{Name: "git", Kind: Bool, Help: "use git even when gh stack is available"},
			pKey,
		},
		run: runStackPush,
	},
	{
		Words: []string{"stack", "rebase"}, Effects: []v1alpha1.EffectOp{v1alpha1.OpStackRebase},
		Help: "Rebase the current stack on its trunk (gh stack rebase, or git rebase --update-refs from the top branch), and record it (stack.rebase).",
		Params: []Param{
			{Name: "base", Help: "the stack's trunk (default: origin's default branch)"},
			{Name: "git", Kind: Bool, Help: "use git even when gh stack is available"},
			pKey,
		},
		run: runStackRebase,
	},
	{
		Words: []string{"result"},
		Help:  "Validate a Result against the task's output schema and outcomes, and copy it to $FACTORY_RESULT.",
		Params: []Param{
			{Name: "file", Help: "the Result file (JSON)"},
			{Name: "result", Kind: JSON, Schema: map[string]any{"type": "object"}, Help: "the Result itself (MCP), instead of a file"},
		},
		run: runResult,
	},
}

func labelOp(verb string, op v1alpha1.EffectOp, help string) *Op {
	return &Op{
		Words: []string{"label", verb}, Effects: []v1alpha1.EffectOp{op}, Help: help,
		Params: []Param{
			{Name: "labels", Flag: "name", Kind: List, Required: true, Help: "the labels (they must match the task's effects.labels)"},
			pTarget, pKey,
		},
		run: apiOp(op, func(e *Effector, a args) (string, map[string]any, error) {
			t, err := e.ref(a.str("target"))
			return t, map[string]any{"labels": a.list("labels")}, err
		}),
	}
}

// apiOp is the run of an API-side op: build the target and args, pre-check, apply.
func apiOp(op v1alpha1.EffectOp, build func(e *Effector, a args) (string, map[string]any, error)) func(context.Context, *Effector, args) (map[string]any, error) {
	return func(ctx context.Context, e *Effector, a args) (map[string]any, error) {
		target, m, err := build(e, a)
		if err != nil {
			return nil, err
		}
		if err := e.precheck(op, m); err != nil {
			return nil, err
		}
		return e.apply(ctx, op, target, m, a.str("key"))
	}
}

func runReview(ctx context.Context, e *Effector, a args) (map[string]any, error) {
	ev := a.str("event")
	op := reviewOps[ev]
	m := map[string]any{}
	setIf(m, "body", a.str("body"))
	setIf(m, "commit", a.str("commit"))
	if ev != "approve" && m["body"] == nil {
		return nil, usagef("review: a %s review needs a body (--body or --body-file)", ev)
	}
	if c, ok := a["comments"]; ok {
		list, ok := c.([]any)
		if !ok {
			return nil, usagef("review: comments must be a JSON array of {path, line, body}")
		}
		for i, x := range list {
			obj, ok := x.(map[string]any)
			if !ok || obj["path"] == nil || obj["line"] == nil || obj["body"] == nil {
				return nil, usagef("review: comment %d needs path, line and body", i)
			}
		}
		m["comments"] = list
	}
	t, err := e.ref(a.str("pr"))
	if err != nil {
		return nil, err
	}
	if err := e.precheck(op, m); err != nil {
		return nil, err
	}
	return e.apply(ctx, op, t, m, a.str("key"))
}

func setIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

var (
	reNumber = regexp.MustCompile(`^#?([0-9]+)$`)
	reRef    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(#[0-9]+)?$`)
	reURL    = regexp.MustCompile(`^https?://[^/]+/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:pull|issues)/([0-9]+)(?:[/#?].*)?$`)
)

// ref normalises an object reference: N and #N are in the task's repo, a GitHub issue or
// pull request URL is its owner/name#N, and owner/name[#N] is kept. "" stays "" (the task's
// primary object).
func (e *Effector) ref(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", nil
	case reNumber.MatchString(s):
		repo := ""
		if e.o.Config != nil {
			repo = e.o.Config.Repo
		}
		if repo == "" {
			return "", usagef("%q needs the task's repo, which is unknown here: use owner/name#N", s)
		}
		return repo + "#" + reNumber.FindStringSubmatch(s)[1], nil
	case reURL.MatchString(s):
		m := reURL.FindStringSubmatch(s)
		return m[1] + "#" + m[2], nil
	case reRef.MatchString(s):
		return s, nil
	}
	return "", usagef("%q is not a reference: use N, #N or owner/name#N", s)
}

package v1alpha1

import "github.com/invopop/jsonschema"

// EffectOp is a provider effect, as named in TaskSpec effects.allow and the effect action.
type EffectOp string

// Side says who executes an effect op.
type Side string

// Sides.
const (
	// SideAPI ops are executed by the dispatcher's Effector through the provider API.
	SideAPI Side = "api"
	// SideGit ops are executed by the agent in its workspace with the scoped git token; the
	// agent still records them through the Effector API for audit.
	SideGit Side = "git"
)

// Effect ops.
const (
	OpComment              EffectOp = "comment"
	OpPRComment            EffectOp = "pr.comment"
	OpLabelAdd             EffectOp = "label.add"
	OpLabelRemove          EffectOp = "label.remove"
	OpCreateIssue          EffectOp = "create-issue"
	OpSubIssueAdd          EffectOp = "sub-issue.add"
	OpClose                EffectOp = "close"
	OpLinkDuplicate        EffectOp = "link-duplicate"
	OpPROpen               EffectOp = "pr.open"
	OpPRUpdate             EffectOp = "pr.update"
	OpPRRetarget           EffectOp = "pr.retarget"
	OpPRMerge              EffectOp = "pr.merge"
	OpReviewComment        EffectOp = "review.comment"
	OpReviewRequestChanges EffectOp = "review.request-changes"
	OpReviewApprove        EffectOp = "review.approve"
	OpThreadReply          EffectOp = "thread.reply"
	OpThreadResolve        EffectOp = "thread.resolve"
	OpPushBranch           EffectOp = "push-branch"
	OpStackPush            EffectOp = "stack.push"
	OpStackRebase          EffectOp = "stack.rebase"
)

var effectOps = []struct {
	Op   EffectOp
	Side Side
}{
	{OpComment, SideAPI},
	{OpPRComment, SideAPI},
	{OpLabelAdd, SideAPI},
	{OpLabelRemove, SideAPI},
	{OpCreateIssue, SideAPI},
	{OpSubIssueAdd, SideAPI},
	{OpClose, SideAPI},
	{OpLinkDuplicate, SideAPI},
	{OpPROpen, SideAPI},
	{OpPRUpdate, SideAPI},
	{OpPRRetarget, SideAPI},
	{OpPRMerge, SideAPI},
	{OpReviewComment, SideAPI},
	{OpReviewRequestChanges, SideAPI},
	{OpReviewApprove, SideAPI},
	{OpThreadReply, SideAPI},
	{OpThreadResolve, SideAPI},
	{OpPushBranch, SideGit},
	{OpStackPush, SideGit},
	{OpStackRebase, SideGit},
}

// EffectOps lists every effect op, in a fixed order.
func EffectOps() []EffectOp {
	out := make([]EffectOp, len(effectOps))
	for i, e := range effectOps {
		out[i] = e.Op
	}
	return out
}

// Side returns who executes op; ok is false for an unknown op.
func (o EffectOp) Side() (side Side, ok bool) {
	for _, e := range effectOps {
		if e.Op == o {
			return e.Side, true
		}
	}
	return "", false
}

// Known reports whether o is a known effect op.
func (o EffectOp) Known() bool {
	_, ok := o.Side()
	return ok
}

// JSONSchema describes EffectOp as the enum of known ops.
func (EffectOp) JSONSchema() *jsonschema.Schema {
	enum := make([]any, len(effectOps))
	for i, e := range effectOps {
		enum[i] = string(e.Op)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

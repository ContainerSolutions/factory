package rules

import (
	"slices"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
)

// Fact keys the scope reads besides the repo: the Slack channel and the git branch.
const (
	factChannel = "channel"
	factBranch  = "branch"
)

// inScope reports whether a RuleSet applies to the event (03 §Evaluation semantics, step 2):
//
//   - a provider event: the RuleSet binds the event's provider instance and its scope matches
//     the event's object;
//   - a factory.* event: its scope matches any object linked to the work item, on a provider
//     the RuleSet binds.
func inScope(rs *catalog.CompiledRuleSet, ev v1alpha1.Event, wi WorkItemView) bool {
	if ev.Source == v1alpha1.FactoryProvider {
		return slices.ContainsFunc(wi.Links, func(l v1alpha1.Link) bool {
			return slices.Contains(rs.Providers, l.Provider) && matchScope(rs.Scope, refRepo(l.Ref), "", "")
		})
	}
	if !slices.Contains(rs.Providers, ev.Source) {
		return false
	}
	channel, _ := ev.Data.Facts[factChannel].(string)
	branch, _ := ev.Data.Facts[factBranch].(string)
	return matchScope(rs.Scope, eventRepo(ev), channel, branch)
}

// matchScope matches one object: a repo object against scope.repos (minus exclude) and, when
// the event names a branch, scope.branches; a channel object against scope.channels. A scope
// that names only channels holds no repo object, and one that names only repos no channel.
// An object with neither is in scope only when the scope names no repos and no channels.
func matchScope(s catalog.CompiledScope, repo, channel, branch string) bool {
	switch {
	case repo != "":
		if len(s.Repos) == 0 && len(s.Channels) > 0 {
			return false
		}
		return s.MatchRepo(repo) && (branch == "" || s.MatchBranch(branch))
	case channel != "":
		if len(s.Channels) == 0 && len(s.Repos) > 0 {
			return false
		}
		return s.MatchChannel(channel)
	default:
		return len(s.Repos) == 0 && len(s.Channels) == 0
	}
}

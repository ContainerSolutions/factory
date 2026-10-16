package v1alpha1

import (
	"strings"

	"github.com/invopop/jsonschema"
)

// EventType is a normalised event type. Rules match it in `on`.
type EventType string

// Provider event types.
const (
	EventIssueOpened    EventType = "issue.opened"
	EventIssueCommented EventType = "issue.commented"
	EventIssueLabeled   EventType = "issue.labeled"
	EventIssueClosed    EventType = "issue.closed"

	EventPROpened          EventType = "pr.opened"
	EventPRUpdated         EventType = "pr.updated"
	EventPRReadyForReview  EventType = "pr.ready_for_review"
	EventPRLabeled         EventType = "pr.labeled"
	EventPRReviewSubmitted EventType = "pr.review.submitted"
	EventPRChecksCompleted EventType = "pr.checks.completed"
	EventPRMerged          EventType = "pr.merged"
	EventPRClosed          EventType = "pr.closed"

	EventReleasePublished EventType = "release.published"

	EventDeploySucceeded EventType = "deploy.succeeded"
	EventDeployFailed    EventType = "deploy.failed"

	EventMessagePosted  EventType = "message.posted"
	EventMessageMention EventType = "message.mention"
)

// Factory event types, emitted only by the built-in factory provider.
const (
	EventFactoryRunCompleted      EventType = "factory.run.completed"
	EventFactoryRunFailed         EventType = "factory.run.failed"
	EventFactoryApprovalGranted   EventType = "factory.approval.granted"
	EventFactoryApprovalDenied    EventType = "factory.approval.denied"
	EventFactoryApprovalExpired   EventType = "factory.approval.expired"
	EventFactoryScheduleTick      EventType = "factory.schedule.tick"
	EventFactoryWorkItemResynced  EventType = "factory.workitem.resynced"
	EventFactoryCommandPause      EventType = "factory.command.pause"
	EventFactoryCommandResume     EventType = "factory.command.resume"
	EventFactoryCommandRedispatch EventType = "factory.command.redispatch"
	EventFactoryCommandCancel     EventType = "factory.command.cancel"
)

// FactoryEventPrefix starts every type only the factory provider may emit.
const FactoryEventPrefix = "factory."

// eventVocabulary is the authoritative list, with the provider types that emit each type.
var eventVocabulary = []struct {
	Type      EventType
	Providers []ProviderType
}{
	{EventIssueOpened, []ProviderType{ProviderTypeGitHub}},
	{EventIssueCommented, []ProviderType{ProviderTypeGitHub}},
	{EventIssueLabeled, []ProviderType{ProviderTypeGitHub}},
	{EventIssueClosed, []ProviderType{ProviderTypeGitHub}},
	{EventPROpened, []ProviderType{ProviderTypeGitHub}},
	{EventPRUpdated, []ProviderType{ProviderTypeGitHub}},
	{EventPRReadyForReview, []ProviderType{ProviderTypeGitHub}},
	{EventPRLabeled, []ProviderType{ProviderTypeGitHub}},
	{EventPRReviewSubmitted, []ProviderType{ProviderTypeGitHub}},
	{EventPRChecksCompleted, []ProviderType{ProviderTypeGitHub}},
	{EventPRMerged, []ProviderType{ProviderTypeGitHub}},
	{EventPRClosed, []ProviderType{ProviderTypeGitHub}},
	{EventReleasePublished, []ProviderType{ProviderTypeGitHub}},
	{EventDeploySucceeded, []ProviderType{ProviderTypeGitHub}},
	{EventDeployFailed, []ProviderType{ProviderTypeGitHub}},
	{EventMessagePosted, []ProviderType{ProviderTypeSlack}},
	{EventMessageMention, []ProviderType{ProviderTypeSlack}},
	{EventFactoryRunCompleted, []ProviderType{ProviderTypeFactory}},
	{EventFactoryRunFailed, []ProviderType{ProviderTypeFactory}},
	{EventFactoryApprovalGranted, []ProviderType{ProviderTypeFactory}},
	{EventFactoryApprovalDenied, []ProviderType{ProviderTypeFactory}},
	{EventFactoryApprovalExpired, []ProviderType{ProviderTypeFactory}},
	{EventFactoryScheduleTick, []ProviderType{ProviderTypeFactory}},
	{EventFactoryWorkItemResynced, []ProviderType{ProviderTypeFactory}},
	{EventFactoryCommandPause, []ProviderType{ProviderTypeFactory}},
	{EventFactoryCommandResume, []ProviderType{ProviderTypeFactory}},
	{EventFactoryCommandRedispatch, []ProviderType{ProviderTypeFactory}},
	{EventFactoryCommandCancel, []ProviderType{ProviderTypeFactory}},
}

var eventProviders = func() map[EventType][]ProviderType {
	m := make(map[EventType][]ProviderType, len(eventVocabulary))
	for _, e := range eventVocabulary {
		m[e.Type] = e.Providers
	}
	return m
}()

// EventTypes lists every known event type, in vocabulary order.
func EventTypes() []EventType {
	out := make([]EventType, len(eventVocabulary))
	for i, e := range eventVocabulary {
		out[i] = e.Type
	}
	return out
}

// Known reports whether t is in the vocabulary.
func (t EventType) Known() bool {
	_, ok := eventProviders[t]
	return ok
}

// IsFactory reports whether t is a factory.* type, which only the factory provider may emit.
func (t EventType) IsFactory() bool { return strings.HasPrefix(string(t), FactoryEventPrefix) }

// ProviderTypes returns the provider types that emit t, or nil for an unknown type.
func (t EventType) ProviderTypes() []ProviderType {
	return append([]ProviderType(nil), eventProviders[t]...)
}

// EmittedBy reports whether provider type p may emit t.
func (t EventType) EmittedBy(p ProviderType) bool {
	for _, q := range eventProviders[t] {
		if q == p {
			return true
		}
	}
	return false
}

// JSONSchema describes EventType as the enum of the vocabulary.
func (EventType) JSONSchema() *jsonschema.Schema {
	enum := make([]any, len(eventVocabulary))
	for i, e := range eventVocabulary {
		enum[i] = string(e.Type)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/invopop/jsonschema"
)

// ProviderType is a provider adapter type.
type ProviderType string

// Provider types. "factory" is the reserved built-in provider; the catalog can't declare it.
const (
	ProviderTypeGitHub  ProviderType = "github"
	ProviderTypeSlack   ProviderType = "slack"
	ProviderTypeFactory ProviderType = "factory"
)

// FactoryProvider is the name (and type) of the built-in provider, the only source of factory.* events.
const FactoryProvider = "factory"

// ---- Provider ------------------------------------------------------------------------------

// Provider says how to connect to one provider instance. It never holds secrets inline.
type Provider struct {
	TypeMeta
	Metadata ObjectMeta   `json:"metadata"`
	Spec     ProviderSpec `json:"spec"`
}

// GetName returns metadata.name.
func (p *Provider) GetName() string { return p.Metadata.Name }

// ProviderSpec is the spec of a Provider.
type ProviderSpec struct {
	Type   ProviderType        `json:"type" jsonschema:"enum=github,enum=slack"`
	GitHub *GitHubProviderSpec `json:"github,omitzero"`
	Slack  *SlackProviderSpec  `json:"slack,omitzero"`
	// Scope is the outer bound of what the factory sees; RuleSets can only narrow it.
	Scope *ProviderScope `json:"scope,omitzero"`
	// Trust resolves actor.tier; the first matching rule wins.
	Trust  []TrustRule   `json:"trust,omitzero"`
	Resync *ResyncPolicy `json:"resync,omitzero"`
}

// GitHubProviderSpec is the connection to a GitHub (or GHE) instance.
type GitHubProviderSpec struct {
	APIURL  string  `json:"apiURL,omitzero"`
	Webhook Webhook `json:"webhook"`
	// Identities are the GitHub Apps this provider can act as, by role (author, reviewer).
	Identities map[string]AppIdentity `json:"identities"`
}

// Webhook is where the provider's webhook is mounted, and the secret that signs it.
type Webhook struct {
	Path      string    `json:"path"`
	SecretRef SecretRef `json:"secretRef"`
}

// AppIdentity is a GitHub App.
type AppIdentity struct {
	AppID         int64     `json:"appID"`
	PrivateKeyRef SecretRef `json:"privateKeyRef"`
}

// SlackProviderSpec is the connection to a Slack workspace (future).
type SlackProviderSpec struct {
	Mode        string     `json:"mode,omitzero"`
	AppTokenRef *SecretRef `json:"appTokenRef,omitzero"`
	BotTokenRef *SecretRef `json:"botTokenRef,omitzero"`
}

// ProviderScope bounds what a provider instance covers.
type ProviderScope struct {
	Orgs     []string `json:"orgs,omitzero"`
	Channels []string `json:"channels,omitzero"`
}

// TrustRule maps actors to a trust tier. The matchers are ANDed; a rule with none matches anyone.
type TrustRule struct {
	Tier              string   `json:"tier"`
	Group             string   `json:"group,omitzero"`
	AuthorAssociation []string `json:"authorAssociation,omitzero"`
	Bots              []string `json:"bots,omitzero"`
	WorkspaceMember   bool     `json:"workspaceMember,omitzero"`
}

// ResyncPolicy says how often the provider's objects are re-observed.
type ResyncPolicy struct {
	Every Duration `json:"every"`
}

// ---- Group ---------------------------------------------------------------------------------

// Group is a named set of people, resolved per provider.
type Group struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     GroupSpec  `json:"spec"`
}

// GetName returns metadata.name.
func (g *Group) GetName() string { return g.Metadata.Name }

// Validate checks that each member source sets exactly one source.
func (g *Group) Validate() error {
	for i := range g.Spec.Members {
		if err := g.Spec.Members[i].Validate(); err != nil {
			return prefixField(fmt.Sprintf("spec.members[%d]", i), err)
		}
	}
	return nil
}

// GroupSpec is the spec of a Group.
type GroupSpec struct {
	Members []GroupSource `json:"members"`
	Cache   *GroupCache   `json:"cache,omitzero"`
}

// GroupSource is one source of members on one provider. Set exactly one of the sources.
type GroupSource struct {
	Provider  string   `json:"provider"`
	Team      string   `json:"team,omitzero" jsonschema:"minLength=1"`
	OrgRole   *OrgRole `json:"orgRole,omitzero"`
	Users     []string `json:"users,omitzero"`
	UserGroup string   `json:"userGroup,omitzero" jsonschema:"minLength=1"`
}

// groupSources are the member names of GroupSource that are sources, in order.
var groupSources = []string{"team", "orgRole", "users", "userGroup"}

// Validate checks that exactly one source is set.
func (g *GroupSource) Validate() error {
	return exactlyOne("member source", groupSources,
		g.Team != "", g.OrgRole != nil, g.Users != nil, g.UserGroup != "")
}

// JSONSchemaExtend requires exactly one source.
func (GroupSource) JSONSchemaExtend(s *jsonschema.Schema) { s.OneOf = requiredOneOf(groupSources) }

// OrgRole selects the members of an org with a role, e.g. the admins.
type OrgRole struct {
	Org  string `json:"org"`
	Role string `json:"role"`
}

// GroupCache says how long resolved members are cached.
type GroupCache struct {
	TTL Duration `json:"ttl"`
}

// ---- Template ------------------------------------------------------------------------------

// Template is the text of notifications and approval prompts, one body per provider type.
type Template struct {
	TypeMeta
	Metadata ObjectMeta   `json:"metadata"`
	Spec     TemplateSpec `json:"spec"`
}

// GetName returns metadata.name.
func (t *Template) GetName() string { return t.Metadata.Name }

// TemplateSpec is the spec of a Template.
type TemplateSpec struct {
	Bodies map[ProviderType]TemplateBody `json:"bodies"`
}

// JSONSchemaExtend limits the body keys to the provider types a catalog can use.
func (TemplateSpec) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("bodies"); ok {
		p.PropertyNames = &jsonschema.Schema{Enum: []any{string(ProviderTypeGitHub), string(ProviderTypeSlack)}}
	}
}

// TemplateBody is either an inline Go text/template (written as a string) or, for Slack,
// a Block Kit file ({blocks: path}).
type TemplateBody struct {
	Text   string
	Blocks string
}

type templateBodyObject struct {
	Blocks string `json:"blocks"`
}

// MarshalJSON writes the text as a string, or the object form.
func (b TemplateBody) MarshalJSON() ([]byte, error) {
	if b.Blocks == "" {
		return json.Marshal(b.Text)
	}
	if b.Text != "" {
		return nil, errors.New("template body: set text or blocks, not both")
	}
	return json.Marshal(templateBodyObject{Blocks: b.Blocks})
}

// UnmarshalJSON accepts a string or {blocks: path}.
func (b *TemplateBody) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*b = TemplateBody{Text: s}
		return nil
	}
	var o templateBodyObject
	if err := strictJSON(data, &o); err != nil {
		return fmt.Errorf("template body: want a string or {blocks: path}: %w", err)
	}
	if o.Blocks == "" {
		return errors.New("template body: blocks must not be empty")
	}
	*b = TemplateBody{Blocks: o.Blocks}
	return nil
}

// JSONSchema describes TemplateBody as oneOf [string, {blocks}].
func (TemplateBody) JSONSchema() *jsonschema.Schema {
	obj := &jsonschema.Schema{
		Type:                 "object",
		Properties:           jsonschema.NewProperties(),
		Required:             []string{"blocks"},
		AdditionalProperties: jsonschema.FalseSchema,
	}
	obj.Properties.Set("blocks", &jsonschema.Schema{Type: "string", MinLength: ptr(uint64(1)), Description: "Path of a Slack Block Kit JSON file."})
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string", Description: "An inline Go text/template."},
		obj,
	}}
}

// ---- Rule ----------------------------------------------------------------------------------

// Rule is when and what. It has no scope: the RuleSets that select it decide where it applies.
type Rule struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     RuleSpec   `json:"spec"`
}

// GetName returns metadata.name.
func (r *Rule) GetName() string { return r.Metadata.Name }

// Validate checks the rule's actions (exactly one member each).
func (r *Rule) Validate() error { return r.Spec.validate("spec") }

// RuleSpec is the spec of a Rule.
type RuleSpec struct {
	RuleSet string `json:"ruleSet,omitzero"`
	// ProviderSelector names the provider types or families the rule is written for
	// (validation only; RuleSets bind instances). Not needed for factory.*-only rules.
	ProviderSelector *LabelSelector `json:"providerSelector,omitzero"`
	On               []EventType    `json:"on"`
	// Where is CEL over event, actor, item, run, approval, provider, params, now.
	Where    string         `json:"where,omitzero"`
	Params   map[string]any `json:"params,omitzero"`
	Actions  []Action       `json:"actions"`
	Priority int            `json:"priority,omitzero"`
	Final    bool           `json:"final,omitzero"`
	Debounce *Debounce      `json:"debounce,omitzero"`
	// Schedule is a cron expression, only with on: [factory.schedule.tick].
	Schedule string `json:"schedule,omitzero"`
}

func (s *RuleSpec) validate(path string) error {
	for i := range s.Actions {
		if err := s.Actions[i].Validate(); err != nil {
			return prefixField(fmt.Sprintf("%s.actions[%d]", path, i), err)
		}
	}
	return nil
}

// Debounce collapses bursts of events into one action.
type Debounce struct {
	Window Duration `json:"window"`
	Key    string   `json:"key,omitzero"`
}

// Action is a tagged union: exactly one member is set. String values starting with "=" are
// CEL evaluated at runtime; "{{ params.x }}" is substituted at catalog load.
type Action struct {
	Dispatch *DispatchAction `json:"dispatch,omitzero"`
	// Set updates work-item state keys; values are literals or "=CEL".
	Set    map[string]any `json:"set,omitzero"`
	Notify *NotifyAction  `json:"notify,omitzero"`
	// RequestApproval creates an Approval (future, Q18): accepted by the schema, the engine
	// returns Unsupported in the PoC.
	RequestApproval *ApprovalSpec `json:"requestApproval,omitzero"`
	Cancel          *CancelAction `json:"cancel,omitzero"`
	Link            *LinkAction   `json:"link,omitzero"`
	Effect          *EffectAction `json:"effect,omitzero"`
}

// ActionTypes are the member names of Action, in order.
var ActionTypes = []string{"dispatch", "set", "notify", "requestApproval", "cancel", "link", "effect"}

// Type returns the name of the member that is set, or "" when none is.
func (a *Action) Type() string {
	set := a.members()
	if len(set) == 1 {
		return set[0]
	}
	return ""
}

func (a *Action) members() []string {
	var set []string
	for i, ok := range []bool{
		a.Dispatch != nil, a.Set != nil, a.Notify != nil, a.RequestApproval != nil,
		a.Cancel != nil, a.Link != nil, a.Effect != nil,
	} {
		if ok {
			set = append(set, ActionTypes[i])
		}
	}
	return set
}

// Validate checks that exactly one member is set, and the notify destinations.
func (a *Action) Validate() error {
	switch set := a.members(); len(set) {
	case 1:
	case 0:
		return &ValidationError{Message: fmt.Sprintf("action has no member: set exactly one of %v", ActionTypes)}
	default:
		return &ValidationError{Message: fmt.Sprintf("action has %d members %v: set exactly one", len(set), set)}
	}
	if a.Notify != nil {
		for i := range a.Notify.To {
			if err := a.Notify.To[i].Validate(); err != nil {
				return prefixField(fmt.Sprintf("notify.to[%d]", i), err)
			}
		}
	}
	return nil
}

// exactlyOne returns a *ValidationError unless exactly one of set is true; names labels them.
func exactlyOne(what string, names []string, set ...bool) error {
	var got []string
	for i, ok := range set {
		if ok {
			got = append(got, names[i])
		}
	}
	switch len(got) {
	case 1:
		return nil
	case 0:
		return &ValidationError{Message: fmt.Sprintf("%s sets none of %v: set exactly one", what, names)}
	default:
		return &ValidationError{Message: fmt.Sprintf("%s sets %v: set exactly one of %v", what, got, names)}
	}
}

// requiredOneOf is a schema oneOf that is satisfied when exactly one of names is present.
func requiredOneOf(names []string) []*jsonschema.Schema {
	out := make([]*jsonschema.Schema, len(names))
	for i, n := range names {
		out[i] = &jsonschema.Schema{Required: []string{n}}
	}
	return out
}

// JSONSchemaExtend enforces the one-of in the schema: every property is a member and
// additional properties are refused, so exactly one property means exactly one member.
func (Action) JSONSchemaExtend(s *jsonschema.Schema) {
	s.MinProperties = ptr(uint64(1))
	s.MaxProperties = ptr(uint64(1))
}

// DispatchAction creates a Task (Pending) in a queue.
type DispatchAction struct {
	TaskSpec string `json:"taskSpec"`
	Queue    string `json:"queue"`
	// Inputs must match the TaskSpec's declared inputs.
	Inputs   map[string]any          `json:"inputs,omitzero"`
	Harness  *ExprOr[HarnessRequest] `json:"harness,omitzero"`
	Requires *Requires               `json:"requires,omitzero"`
	// Context lists what the agent is given, e.g. event.object.
	Context []string `json:"context,omitzero"`
	Target  *Target  `json:"target,omitzero"`
}

// NotifyAction sends a templated message through the providers; no LLM involved.
type NotifyAction struct {
	Template string        `json:"template"`
	To       []Destination `json:"to"`
}

// Destination is one recipient of a notification. Set one of group, channel or item.
type Destination struct {
	Group   string      `json:"group,omitzero" jsonschema:"minLength=1"`
	Via     string      `json:"via,omitzero"`
	As      string      `json:"as,omitzero" jsonschema:"enum=mention,enum=dm"`
	Channel *ChannelRef `json:"channel,omitzero"`
	// Item comments on, or replies in, the work item's linked objects.
	Item string `json:"item,omitzero" jsonschema:"enum=primary,enum=threads,enum=all"`
}

// destinationTargets are the member names of Destination that pick the recipient.
var destinationTargets = []string{"group", "channel", "item"}

// Validate checks that exactly one of group, channel or item is set.
func (d *Destination) Validate() error {
	return exactlyOne("destination", destinationTargets, d.Group != "", d.Channel != nil, d.Item != "")
}

// JSONSchemaExtend requires exactly one of group, channel or item.
func (Destination) JSONSchemaExtend(s *jsonschema.Schema) {
	s.OneOf = requiredOneOf(destinationTargets)
}

// ChannelRef is a channel on a provider.
type ChannelRef struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// CancelAction cancels the active tasks of a task spec for the work item.
type CancelAction struct {
	TaskSpec string `json:"taskSpec"`
}

// LinkAction adds a provider object to the work item.
type LinkAction struct {
	// From is CEL giving the object ref.
	From string `json:"from"`
}

// EffectAction is a direct, no-LLM provider effect, shaped like an Effector request.
type EffectAction struct {
	Op EffectOp `json:"op"`
	// Target is the object to act on: primary, a ref, or "=CEL".
	Target string `json:"target,omitzero"`
	// Args are the op-specific arguments.
	Args map[string]any `json:"args,omitzero"`
}

// ---- RuleSet -------------------------------------------------------------------------------

// RuleSet is where rules apply: it binds provider instances, sets the scope, selects rules
// and overrides their params. Rules only fire through a RuleSet.
type RuleSet struct {
	TypeMeta
	Metadata ObjectMeta  `json:"metadata"`
	Spec     RuleSetSpec `json:"spec"`
}

// GetName returns metadata.name.
func (r *RuleSet) GetName() string { return r.Metadata.Name }

// Validate checks the inline rules' actions.
func (r *RuleSet) Validate() error {
	for i := range r.Spec.InlineRules {
		if err := r.Spec.InlineRules[i].Spec.validate(fmt.Sprintf("spec.inlineRules[%d].spec", i)); err != nil {
			return err
		}
	}
	return nil
}

// RuleSetSpec is the spec of a RuleSet.
type RuleSetSpec struct {
	// ProviderSelector binds provider instances.
	ProviderSelector LabelSelector `json:"providerSelector"`
	Scope            *Scope        `json:"scope,omitzero"`
	Rules            RuleSelection `json:"rules"`
	// Overrides are per-rule params, by rule name.
	Overrides map[string]map[string]any `json:"overrides,omitzero"`
	// Defaults are params applied to every selected rule unless the rule overrides them.
	Defaults map[string]any `json:"defaults,omitzero"`
	Budget   *Budget        `json:"budget,omitzero"`
	// StateKeys, when declared, are the only keys `set` may write.
	StateKeys []string `json:"stateKeys,omitzero"`
	// InlineRules are rules defined in the RuleSet itself, selected implicitly.
	InlineRules []InlineRule `json:"inlineRules,omitzero"`
}

// InlineRule is a Rule without its TypeMeta, embedded in a RuleSet.
type InlineRule struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     RuleSpec   `json:"spec"`
}

// Scope narrows a RuleSet to repos (globs) or channels.
type Scope struct {
	Repos    []string `json:"repos,omitzero"`
	Exclude  []string `json:"exclude,omitzero"`
	Channels []string `json:"channels,omitzero"`
	Branches []string `json:"branches,omitzero"`
}

// RuleSelection picks rules: refs ∪ selector, minus exclude.
type RuleSelection struct {
	Refs     []string       `json:"refs,omitzero"`
	Selector *LabelSelector `json:"selector,omitzero"`
	Exclude  []string       `json:"exclude,omitzero"`
}

// Budget caps spend.
type Budget struct {
	Daily *BudgetLimit `json:"daily,omitzero"`
}

// BudgetLimit is one budget window's limits; an unset limit is unlimited.
type BudgetLimit struct {
	Runs   int     `json:"runs,omitzero"`
	Tokens int64   `json:"tokens,omitzero"`
	USD    float64 `json:"usd,omitzero"`
}

// ---- TaskSpec ------------------------------------------------------------------------------

// TaskSpec is what and how for one role, with no triggers or transitions.
type TaskSpec struct {
	TypeMeta
	Metadata ObjectMeta   `json:"metadata"`
	Spec     TaskSpecSpec `json:"spec"`
}

// GetName returns metadata.name.
func (t *TaskSpec) GetName() string { return t.Metadata.Name }

// TaskSpecSpec is the spec of a TaskSpec. String fields that depend on inputs may be "=CEL".
type TaskSpecSpec struct {
	Description string `json:"description,omitzero"`
	// Inputs are the declared, typed inputs that dispatch.inputs must satisfy.
	Inputs       map[string]InputSpec `json:"inputs,omitzero"`
	Instructions *Instructions        `json:"instructions,omitzero"`
	Skills       []SkillRef           `json:"skills,omitzero"`
	// Requires lists agent labels needed; dispatch can add to it.
	Requires    *Requires         `json:"requires,omitzero"`
	Harness     *HarnessPolicy    `json:"harness,omitzero"`
	Workspace   *Workspace        `json:"workspace,omitzero"`
	Identity    string            `json:"identity,omitzero"`
	Permissions map[string]string `json:"permissions,omitzero"`
	Effects     *EffectsPolicy    `json:"effects,omitzero"`
	Limits      *Limits           `json:"limits,omitzero"`
	Output      *OutputSpec       `json:"output,omitzero"`
}

// InputSpec declares one TaskSpec input.
type InputSpec struct {
	Type     string `json:"type,omitzero" jsonschema:"enum=string,enum=number,enum=integer,enum=boolean,enum=object,enum=array"`
	Enum     []any  `json:"enum,omitzero"`
	Default  any    `json:"default,omitzero"`
	Required bool   `json:"required,omitzero"`
}

// Instructions is the prompt template.
type Instructions struct {
	Template string `json:"template"`
	Headless *bool  `json:"headless,omitzero"`
}

// SkillRef pins skills from a skills repo. Commit is filled in on a Task.
type SkillRef struct {
	Source string   `json:"source"`
	Ref    string   `json:"ref,omitzero"`
	Commit string   `json:"commit,omitzero"`
	Use    []string `json:"use,omitzero"`
}

// Requires lists the agent labels a task needs.
type Requires struct {
	Labels []string `json:"labels,omitzero"`
}

// HarnessPolicy is a TaskSpec's harness constraints and default request.
type HarnessPolicy struct {
	// Allowed are constraints: nothing outside them runs for this task spec.
	Allowed []HarnessConstraint `json:"allowed,omitzero"`
	Default *HarnessRequest     `json:"default,omitzero"`
}

// HarnessConstraint bounds what a harness may run. Strings are patterns.
type HarnessConstraint struct {
	Name            string       `json:"name"`
	Models          StringOrList `json:"models,omitzero"`
	ReasoningEffort StringOrList `json:"reasoningEffort,omitzero"`
}

// HarnessRequest asks for a harness and config. Strings are patterns; it must fall inside the
// TaskSpec's constraints.
type HarnessRequest struct {
	Name   string         `json:"name"`
	Config *HarnessConfig `json:"config,omitzero"`
}

// HarnessConfig is a requested harness config.
type HarnessConfig struct {
	Model           StringOrList `json:"model,omitzero"`
	ReasoningEffort StringOrList `json:"reasoningEffort,omitzero"`
	// Prefer orders candidate models when several match.
	Prefer []string `json:"prefer,omitzero"`
}

// Workspace says what the agent checks out.
type Workspace struct {
	// Repos are mounted repos; at most one is rw.
	Repos    []RepoMount `json:"repos,omitzero"`
	Checkout string      `json:"checkout,omitzero"`
	// PublicReads lets the agent clone public repos anonymously at runtime.
	PublicReads *bool `json:"publicReads,omitzero"`
}

// RepoMount is one repo in the workspace.
type RepoMount struct {
	// Ref is owner/repo@ref, or "=CEL".
	Ref  string `json:"ref"`
	Mode string `json:"mode,omitzero" jsonschema:"enum=ro,enum=rw"`
}

// EffectsPolicy is what a task may do.
type EffectsPolicy struct {
	Allow []EffectOp `json:"allow,omitzero"`
	// Labels are globs the label.* effects are checked against.
	Labels []string `json:"labels,omitzero"`
}

// Limits bound a run.
type Limits struct {
	MaxTurns     int      `json:"maxTurns,omitzero"`
	MaxWallClock Duration `json:"maxWallClock,omitzero"`
	MaxCostUSD   float64  `json:"maxCostUSD,omitzero"`
}

// OutputSpec is the expected Result: its spec-specific schema and outcomes.
type OutputSpec struct {
	Schema   string   `json:"schema,omitzero"`
	Outcomes []string `json:"outcomes,omitzero"`
}

// ---- Queue and Pool ------------------------------------------------------------------------

// Queue holds tasks until a pool's agents take them.
type Queue struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     QueueSpec  `json:"spec"`
}

// GetName returns metadata.name.
func (q *Queue) GetName() string { return q.Metadata.Name }

// QueueSpec is the spec of a Queue.
type QueueSpec struct {
	Pool        string       `json:"pool"`
	Concurrency int          `json:"concurrency,omitzero"`
	Priority    int          `json:"priority,omitzero"`
	Budget      *Budget      `json:"budget,omitzero"`
	Retry       *RetryPolicy `json:"retry,omitzero"`
}

// RetryReason is a failure that may be retried. Throttling never consumes an attempt.
type RetryReason string

// Retry reasons.
const (
	RetryAgentLost    RetryReason = "agent-lost"
	RetryInfraFailure RetryReason = "infra-failure"
)

// RetryPolicy says how often, and on what, a task is retried.
type RetryPolicy struct {
	Max int           `json:"max"`
	On  []RetryReason `json:"on,omitzero"`
}

// PoolMode is how a pool gets agents.
type PoolMode string

// Pool modes.
const (
	PoolRegistered PoolMode = "registered"
	PoolEphemeral  PoolMode = "ephemeral"
)

// Pool is who takes tasks out of queues and how capacity scales.
type Pool struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     PoolSpec   `json:"spec"`
}

// GetName returns metadata.name.
func (p *Pool) GetName() string { return p.Metadata.Name }

// PoolSpec is the spec of a Pool. Backend, Image, Scaling and LLMAuth are for ephemeral pools.
type PoolSpec struct {
	Mode      PoolMode          `json:"mode" jsonschema:"enum=registered,enum=ephemeral"`
	Admission *Admission        `json:"admission,omitzero"`
	Lease     *LeasePolicy      `json:"lease,omitzero"`
	Require   *PoolRequirements `json:"require,omitzero"`
	Fairness  string            `json:"fairness,omitzero"`
	Backend   *PoolBackend      `json:"backend,omitzero"`
	Image     string            `json:"image,omitzero"`
	Scaling   *Scaling          `json:"scaling,omitzero"`
	LLMAuth   *LLMAuth          `json:"llmAuth,omitzero"`
}

// Admission is how registered agents join.
type Admission struct {
	TokenRef SecretRef `json:"tokenRef"`
	// Owners is the Group whose members may register agents here.
	Owners string `json:"owners"`
}

// LeasePolicy sets lease timing.
type LeasePolicy struct {
	TTL              Duration `json:"ttl,omitzero"`
	Heartbeat        Duration `json:"heartbeat,omitzero"`
	MaxTasksPerAgent int      `json:"maxTasksPerAgent,omitzero"`
}

// PoolRequirements is the minimum an agent must offer to join.
type PoolRequirements struct {
	// Harnesses: any of.
	Harnesses []string `json:"harnesses,omitzero"`
}

// PoolBackend runs ephemeral agents.
type PoolBackend struct {
	Type          string                `json:"type"`
	GitHubActions *GitHubActionsBackend `json:"githubActions,omitzero"`
}

// GitHubActionsBackend runs agents as a workflow.
type GitHubActionsBackend struct {
	ProviderRef string `json:"providerRef"`
	Repo        string `json:"repo"`
	Workflow    string `json:"workflow"`
}

// Scaling bounds an ephemeral pool's size.
type Scaling struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// LLMAuth is the LLM credential ephemeral agents use.
type LLMAuth struct {
	APIKeyRef *SecretRef `json:"apiKeyRef,omitzero"`
}

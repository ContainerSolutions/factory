package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

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

// EffectAction is a direct, no-LLM provider effect: the op, an optional target and the
// op-specific fields written inline, e.g. { op: label.add, labels: [needs-human] }. Inline
// fields keep the catalog readable (design/04-flows.md). The Effector HTTP request is
// {op, target, args, idempotencyKey}; the dispatcher maps Args to args.
type EffectAction struct {
	Op EffectOp `json:"op"`
	// Target is the object to act on: primary, a ref, or "=CEL".
	Target string `json:"target,omitzero"`
	// Args are the op-specific fields, every key other than op and target. The catalog
	// loader checks them per op.
	Args map[string]any `json:"-"`
}

// effectReserved are the keys EffectAction itself owns; args can't use them.
var effectReserved = map[string]bool{"op": true, "target": true, "args": true}

// UnmarshalJSON reads op and target, and collects every other key into Args.
func (e *EffectAction) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, ok := fields["args"]; ok {
		return errors.New(`effect: write the op-specific fields inline next to op, not under "args"`)
	}
	raw, ok := fields["op"]
	if !ok {
		return errors.New("effect: op is required")
	}
	var out EffectAction
	if err := json.Unmarshal(raw, &out.Op); err != nil {
		return fmt.Errorf("effect: op: %w", err)
	}
	if raw, ok := fields["target"]; ok {
		if err := json.Unmarshal(raw, &out.Target); err != nil {
			return fmt.Errorf("effect: target: %w", err)
		}
	}
	for k, raw := range fields {
		if effectReserved[k] {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("effect: %s: %w", k, err)
		}
		if out.Args == nil {
			out.Args = make(map[string]any, len(fields))
		}
		out.Args[k] = v
	}
	*e = out
	return nil
}

// MarshalJSON writes op, target, then the op-specific fields inline in sorted key order.
func (e EffectAction) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	write := func(k string, v any) error {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return err
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("effect: %s: %w", k, err)
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
		return nil
	}
	b.WriteByte('{')
	if err := write("op", e.Op); err != nil {
		return nil, err
	}
	if e.Target != "" {
		if err := write("target", e.Target); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(e.Args))
	for k := range e.Args {
		if effectReserved[k] {
			return nil, fmt.Errorf("effect: args key %q collides with a field of the effect", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := write(k, e.Args[k]); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// JSONSchemaExtend allows the inline op-specific fields.
func (EffectAction) JSONSchemaExtend(s *jsonschema.Schema) {
	s.AdditionalProperties = jsonschema.TrueSchema
	s.Not = &jsonschema.Schema{Required: []string{"args"}}
	s.Description = "A direct provider effect: op, an optional target, and the op-specific fields inline (e.g. labels for label.add). The catalog loader checks those fields per op."
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

// Validate checks the limits (Limits.Validate), the workspace (Workspace.Validate), and that
// an environment's versions.input names one of the spec's inputs.
func (t *TaskSpec) Validate() error {
	if err := t.Spec.Limits.Validate(); err != nil {
		return prefixField("spec.limits", err)
	}
	ws := t.Spec.Workspace
	if ws == nil {
		return nil
	}
	if err := ws.Validate(); err != nil {
		return prefixField("spec.workspace", err)
	}
	if env := ws.Environment; env != nil && env.Versions != nil && env.Versions.Input != "" {
		if _, ok := t.Spec.Inputs[env.Versions.Input]; !ok {
			return &ValidationError{
				Field:   "spec.workspace.environment.versions.input",
				Message: fmt.Sprintf("%q is not one of spec.inputs", env.Versions.Input),
				Err:     ErrEnvVersionsInput,
			}
		}
	}
	return nil
}

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
	// PublicReads lets the agent clone public repos anonymously at runtime, and says the ro
	// repos are public: they are cloned anonymously and get no read-only token.
	PublicReads *bool `json:"publicReads,omitzero"`
	// Hosts are the extra hostnames a sandboxed harness may reach, on top of the git host, the
	// dispatcher and the local stack, e.g. registry.npmjs.org for a docs task's yarn install.
	// Each is a lowercase hostname of at least two labels with an optional port and an
	// optional single leading "*." label followed by at least two labels (*.example.com, not
	// *.com); no scheme, path, userinfo, IP literal or localhost; at most 20 (ValidateHost).
	// The compiler copies them to the Task's spec.hosts.
	// Hosts widen what a prompt-injected run could reach, so a catalog change to them needs
	// two approvals (FACTORY-CAT-002).
	Hosts []string `json:"hosts,omitzero"`
	// Environment is what the task's VM workspace brings up before the harness runs (P1 §4).
	// The compiler copies it to the Task's spec.environment.
	Environment *Environment `json:"environment,omitzero"`
}

// Environment is what a task's workspace brings up before the harness runs (P1 §4): plain
// commands run as root inside the task's VM workspace, which has its own Docker. It never runs
// on the dispatcher, and the factory knows nothing about what the commands start.
type Environment struct {
	// Versions says where component versions come from; the compiler resolves them into the
	// Task's spec.versions.
	Versions *VersionSource `json:"versions,omitzero"`
	// Setup runs first (install tools the base image lacks); Up brings the environment up;
	// Down runs best effort at the end. Each is a shell script for /bin/bash, at most 64 KiB.
	Setup string `json:"setup,omitzero"`
	Up    string `json:"up,omitzero"`
	Down  string `json:"down,omitzero"`
	// Ready is checked after Up; the run fails (environment failure) if it isn't ready in time.
	Ready *ReadyCheck `json:"ready,omitzero"`
	// Exposes are exported to the harness's environment: FACTORY_* names to values, typically
	// URLs inside the workspace (FACTORY_STACK_URL: http://localhost:8000). At most 20; no
	// name the runtime sets (ReservedEnvNames, FACTORY_VERSION_*).
	Exposes map[string]string `json:"exposes,omitzero"`
	// Egress narrows the workspace's network (P1 §3); default is open through the gateway.
	// Like hosts, a catalog change to egress.allow needs two approvals.
	Egress *EgressPolicy `json:"egress,omitzero"`
}

// VersionSource is where an environment's component versions come from.
type VersionSource struct {
	// From is latest (each component's latest final release), inputs (a map in the task's
	// inputs) or train (a release-train reference in the task's inputs).
	From string `json:"from" jsonschema:"enum=latest,enum=inputs,enum=train"`
	// Input names the TaskSpec input that holds the map (inputs) or the train reference
	// (train). Required for those two, forbidden for latest.
	Input string `json:"input,omitzero"`
}

// ReadyCheck is how the runtime waits for an environment.
type ReadyCheck struct {
	// HTTP URLs that must each answer 2xx or 3xx; at most 10, http or https.
	HTTP []string `json:"http,omitzero"`
	// Timeout for all of them together; default 15m, at most 60m.
	Timeout Duration `json:"timeout,omitzero"`
}

// EgressPolicy narrows a workspace's network.
type EgressPolicy struct {
	// Mode is default (open through the gateway, except its denied ranges) or deny-by-default
	// (only Allow, plus the git host and the dispatcher).
	Mode string `json:"mode,omitzero" jsonschema:"enum=default,enum=deny-by-default"`
	// Allow are hostnames reachable under deny-by-default; same rules as workspace.hosts
	// (ValidateHost, at most 20). Only valid with deny-by-default.
	Allow []string `json:"allow,omitzero"`
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
	// Labels are globs every label an effect can write is checked against: label.add/remove,
	// and the labels other ops carry (create-issue and pr.open labels, pr.merge's mergeLabel).
	Labels []string `json:"labels,omitzero"`
}

// Limits bound a run.
type Limits struct {
	MaxTurns     int      `json:"maxTurns,omitzero"`
	MaxWallClock Duration `json:"maxWallClock,omitzero"`
	MaxCostUSD   float64  `json:"maxCostUSD,omitzero"`
	// CPUs is the workspace VM's vCPUs for this task: at least 1, and at most the agent's
	// workspace.size.cpus (the agent caps a larger request and says so in the run log).
	// Unset means the agent's size.
	CPUs int `json:"cpus,omitzero"`
	// Memory is the workspace VM's memory for this task: a whole number of GiB in the form of
	// the agent's workspace.size.memory (e.g. "4Gi"), and at most that size (the agent caps a
	// larger request and says so in the run log). Unset means the agent's size.
	Memory string `json:"memory,omitzero"`
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

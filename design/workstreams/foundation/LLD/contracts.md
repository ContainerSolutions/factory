# LLD — Contracts: Go types, schemas, provider interface, HTTP API (FND)

> Workstream: [FND](../../definitions.md#fnd-foundation) · Tasks: FND-S1-T01, FND-S1-T02
> These contracts are the **only** coupling between workstreams. Changing them = a dedicated PR merged before dependents.

## Go types

Package `internal/apis/v1alpha1` (`APIVersion = "factory.ccf.dev/v1alpha1"`). Every kind embeds `TypeMeta`; all kinds except Task and Event have `ObjectMeta`:

```go
type TypeMeta struct { APIVersion string `json:"apiVersion,omitzero"`; Kind string `json:"kind"` } // apiVersion optional, never added on output
type ObjectMeta struct {
    Name            string            `json:"name"`
    Labels          map[string]string `json:"labels,omitzero"`
    ResourceVersion string            `json:"resourceVersion,omitzero"` // runtime kinds only
    CreatedAt       time.Time         `json:"createdAt,omitzero"`
}
```

Optional fields use `omitzero`, so an explicitly empty list or map (`labels: []`, `inputs: {}`) survives a round trip. Pointers mark optional structs and the few fields where `false` or `null` must be kept (`publicReads`, `headless`, and the nullable runtime fields below).

**Decoding** (`Decode`, `DecodeOne`, `DecodeResult`, `Encode`) uses YAML 1.2 (`go.yaml.in/yaml/v3`), so `on`, `yes` and `no` are strings and only `true`/`false` are booleans, in typed fields and free-form maps alike. A stream is read document by document (`---`, `...`, comments and directives handled by the parser; empty documents skipped). Each document becomes a generic value (keys must be strings; duplicate keys, merge keys and unknown tags are errors; timestamps stay as written), then JSON, which is decoded with `encoding/json` and `DisallowUnknownFields`, so the `json` tags are the only field names. The document's `kind` picks the type. Unknown fields, an unknown kind and an `apiVersion` other than `APIVersion` are errors: `*DocumentError{Index, Line, Kind}` wrapping `ErrUnknownKind`, `ErrMissingKind`, `ErrAPIVersion` or a `*ValidationError{Field}` from the kind's `Validate()`. `Encode` goes through JSON too: struct field order, block style, quoting only where a plain scalar would read back as another type.

**Value types:**

| Type | Shape | Used for |
|---|---|---|
| `Duration` | `string` as written; `Parse()` = `time.ParseDuration` plus a leading whole number of days (`7d`, `1d12h`) | every duration; validated by CORE, not at decode |
| `StringOrList` | a string or a list of strings; remembers which, so it round-trips | harness patterns: `models`, `model`, `reasoningEffort` (`"*"` vs `[low, medium]`) |
| `ExprOr[T]` | an expression string (`"{{ params.x }}"` at catalog load, `"=CEL"` at runtime) or a `T`; schema `oneOf [string, T]` | `dispatch.harness` (the only structured field the examples template) |
| `TemplateBody` | a string (inline Go `text/template`) or `{blocks: path}` (Slack Block Kit file) | `Template.spec.bodies` |
| `map[string]any` | free-form | `params`, `inputs`, `set`, `defaults`, `overrides`, `facts`, `verdict`, `data` |

### Config kinds (from git)

Each is `{TypeMeta, Metadata ObjectMeta, Spec <Kind>Spec}`.

| Kind | Spec fields (Go) | Source of truth |
|---|---|---|
| `Provider` | `Type ProviderType` (`github`\|`slack`); `GitHub *GitHubProviderSpec` (`APIURL`, `Webhook{Path, SecretRef}`, `Identities map[string]AppIdentity{AppID int64, PrivateKeyRef}`); `Slack *SlackProviderSpec{Mode, AppTokenRef, BotTokenRef}` (future); `Scope *ProviderScope{Orgs, Channels}`; `Trust []TrustRule{Tier, Group, AuthorAssociation, Bots, WorkspaceMember}`; `Resync *ResyncPolicy{Every Duration}` | [03 §Provider](../../../03-domain-model.md#provider) |
| `Group` | `Members []GroupSource{Provider, Team, OrgRole *OrgRole{Org, Role}, Users, UserGroup}` (exactly one of team \| orgRole \| users \| userGroup); `Cache *GroupCache{TTL Duration}` | [03 §Group](../../../03-domain-model.md#group) |
| `Template` | `Bodies map[ProviderType]TemplateBody` (`TemplateBody` = text \| `{Blocks}`) | [03 §Template](../../../03-domain-model.md#template) |
| `Rule` | `RuleSet string`; `ProviderSelector *LabelSelector`; `On []EventType`; `Where string`; `Params map[string]any`; `Actions []Action`; `Priority int`; `Final bool`; `Debounce *Debounce{Window, Key}`; `Schedule string` | [03 §Rule](../../../03-domain-model.md#rule) |
| `RuleSet` | `ProviderSelector LabelSelector`; `Scope *Scope{Repos, Exclude, Channels, Branches}`; `Rules RuleSelection{Refs, Selector *LabelSelector, Exclude}`; `Overrides map[string]map[string]any`; `Defaults map[string]any`; `Budget *Budget{Daily *BudgetLimit{Runs, Tokens, USD}}`; `StateKeys []string`; inline rules `InlineRules []InlineRule{Metadata, Spec RuleSpec}` (`inlineRules`) | [03 §RuleSet](../../../03-domain-model.md#ruleset-mandatory) |
| `TaskSpec` | `Description`; `Inputs map[string]InputSpec{Type, Enum []any, Default any, Required}`; `Instructions *Instructions{Template, Headless *bool}`; `Skills []SkillRef{Source, Ref, Commit, Use}`; `Requires *Requires{Labels}`; `Harness *HarnessPolicy{Allowed []HarnessConstraint{Name, Models, ReasoningEffort StringOrList}, Default *HarnessRequest{Name, Config *HarnessConfig{Model, ReasoningEffort StringOrList, Prefer []string}}}`; `Workspace *Workspace{Repos []RepoMount{Ref, Mode ro\|rw}, Checkout, PublicReads *bool}`; `Identity string`; `Permissions map[string]string`; `Effects *EffectsPolicy{Allow []EffectOp, Labels []string}`; `Limits *Limits{MaxTurns, MaxWallClock Duration, MaxCostUSD}`; `Output *OutputSpec{Schema, Outcomes}` | [03 §TaskSpec](../../../03-domain-model.md#taskspec) |
| `Queue` | `Pool string`; `Concurrency int`; `Priority int`; `Budget *Budget`; `Retry *RetryPolicy{Max, On []RetryReason}` | [03 §Queue and Pool](../../../03-domain-model.md#queue-and-pool) |
| `Pool` | `Mode PoolMode` (`registered`\|`ephemeral`); `Admission *Admission{TokenRef, Owners}`; `Lease *LeasePolicy{TTL, Heartbeat, MaxTasksPerAgent}`; `Require *PoolRequirements{Harnesses}`; `Fairness string`; ephemeral: `Backend *PoolBackend{Type, GitHubActions{ProviderRef, Repo, Workflow}}`, `Image`, `Scaling *Scaling{Min, Max}`, `LLMAuth *LLMAuth{APIKeyRef}` | same |

`Action` is a tagged union — exactly one member. The decoder calls `Rule.Validate()` / `RuleSet.Validate()`, which reject zero or several members with a `*ValidationError` naming `spec.actions[i]` (and `spec.actions[i].notify.to[j]` for a destination that doesn't set exactly one of group, channel, item); the schema says the same with `minProperties: 1, maxProperties: 1`. `Group.Validate()` likewise names `spec.members[i]`.

```go
type Action struct {
    Dispatch        *DispatchAction `json:"dispatch,omitzero"`        // TaskSpec, Queue, Inputs map[string]any, Harness *ExprOr[HarnessRequest], Requires *Requires, Context []string, Target *Target{Provider, Repo, Ref}
    Set             map[string]any  `json:"set,omitzero"`             // values: literal or "=CEL"
    Notify          *NotifyAction   `json:"notify,omitzero"`          // Template, To []Destination{Group, Via, As, Channel *ChannelRef{Provider, Name}, Item}: exactly one of group | channel | item
    RequestApproval *ApprovalSpec   `json:"requestApproval,omitzero"` // future (Q18): the Approval's spec; accepted by schema, engine returns Unsupported in PoC
    Cancel          *CancelAction   `json:"cancel,omitzero"`          // TaskSpec
    Link            *LinkAction     `json:"link,omitzero"`            // From string (CEL)
    Effect          *EffectAction   `json:"effect,omitzero"`          // Op EffectOp, Target string, Args map[string]any (the Effector request's shape)
}
```

### Runtime kinds (in the store)

All have the `metadata` / `spec` / `status` shape except Event (CloudEvents attributes at the top level). `status` is optional. Fields shown as `null` in [03](../../../03-domain-model.md#runtime-kinds) are pointers without `omitzero`, so they are always written (`null` until set).

| Kind | Key fields |
|---|---|
| `Event` | CloudEvents: `ID`, `Source` (provider instance or `factory`), `Type EventType`, `Subject`, `Time`; `Data EventData{Provider, Actor *Actor{ID, Tier, IsBot, IsFactory, Identity}, Object *ObjectRef{Provider, Kind, Ref}, Repo, ContentRef, Facts map[string]any, Raw json.RawMessage}`; `WorkItem string` (set for `factory.*`). No `metadata`. |
| `WorkItem` | `Spec.Links []Link{Provider, Ref, Kind, Primary}`; `Status.State map[string]any`, `Counters map[string]int`, `Approvals []ApprovalRef{Name, Phase}`, `Decisions []Decision{At, Event, RuleSet, Rule, Fired, Inputs, Rails []RailVerdict{Rail, Verdict, Reason}, Actions, Reason}` (bounded, newest `MaxDecisions` = 200), `Reporter *Actor` |
| `Task` | `Metadata TaskMeta{ID, WorkItem, Provenance{Event, RuleSet, Rule, CatalogSHA}, Labels, ResourceVersion, CreatedAt}` (named by id, no `name`); `Spec TaskRunSpec{TaskSpec TaskSpecRef{Name, Digest}, Queue, Pool, Provider, Target, Inputs, Harness{Request *HarnessRequest, Resolved *HarnessResolved (null until leased)}, Skills (with commit), Prompt{Ref, Digest}, Context []ContextItem{Kind, Provider, Ref, Trust, Mount}, Credentials{Git{Provider, Identity, Repo, ReadRepos, Permissions, TTL}, LLM: "agent-local"}, Effects, Limits, Output}`; `Status{Phase, Agent *string (null until leased), Reason, Attempts, LeaseExpiresAt}` |
| `Run` | `Spec{Task, Attempt, Agent, Owner}`; `Status{StartedAt, EndedAt, Progress []ProgressEvent{At, Kind, Message, Data} (ring buffer), Result *Result, TranscriptRef, Usage *Usage}` |
| `Agent` | `Spec{Pool, Owner, Capabilities{Harnesses []HarnessOffer{Name, Version, Auth, Models, ReasoningEffort}, Labels}, Lending *Lending{Unlimited, Queues, RuleSets, Harnesses, MaxConcurrent, MaxRunsPerDay, MaxTokensPerDay, Hours}}`; `Status{Phase (Ready\|Busy\|Throttled\|Draining\|Offline), ThrottledUntil *time.Time (null unless throttled), LastHeartbeat, Running []TaskID, Today{Runs, Tokens}}` |
| `Effect` | `Spec{Task, Op, Target ObjectRef, IdempotencyKey, Identity, Request map[string]any}`; `Status{Result{URL, Error}, At}` |
| `BudgetCounter` | name `budget.<scope>.<name>.<YYYY-MM-DD>`; `Status{Runs, Tokens, USD}` (CAS-updated by the scheduler) |
| `Approval` | type only in PoC (Q18): `Metadata{Name, Labels, …, WorkItem}`; `Spec ApprovalSpec{Name, Approvers, Quorum, Via, Template, Expires Duration}`; `Status{Phase (Pending\|Granted\|Denied\|Expired), Decisions []{By, Decision, At, Via}}` |

### Result

Exactly the JSON in [03 §Result](../../../03-domain-model.md#result): `Task`, `Status` (`done|failed|needs_input|noop`), `Outcome`, `Summary`, `Verdict map[string]any`, `Artifacts []Artifact{Kind, URL, Ref}`, `Suggest []Suggestion{TaskSpec, Reason, Inputs}`, `Question *string` (always written; `null` unless `needs_input`), `Harness *ResultHarness{Name, Version, Auth, EffectiveConfig{Model, ReasoningEffort}, Usage{InputTokens, OutputTokens, Turns, WallClock Duration, CostUSD *float64}}` (`costUSD` always written; `null` for subscription runs), plus spec-specific fields under `Data map[string]any` validated by the TaskSpec's output schema.

### Capability descriptors

```go
type AdapterCapabilities struct {          // published by each harness adapter (static per adapter version)
    Harness          string
    EffortMapping    map[string]string      // normalised effort → native value
    Features         []string               // "structured-output", "mcp", "plugins", "skills-dir"
}
type HarnessOffer struct { Name, Version, Auth string; Models, ReasoningEffort []string } // concrete values, not patterns
```

`factory validate --capabilities caps.json` validates patterns against them; the agent's `Probe` produces per-machine offers (`HarnessOffer`).

### JSON Schemas

`make schemas` runs `internal/apis/v1alpha1/schemagen`, which writes `schemas/<kind>.schema.json` for each config kind (`provider`, `group`, `template`, `rule`, `ruleset`, `taskspec`, `queue`, `pool`) and `schemas/result.schema.json` (draft 2020-12, `$id` `https://factory.ccf.dev/schemas/v1alpha1/<file>`). They are reflected from the types (invopop/jsonschema), so they match strict decoding: objects have `additionalProperties: false`, `kind` and `apiVersion` are `const`, `EventType` and `EffectOp` are enums of the vocabulary below, `Action` allows exactly one property, and `GroupSource` / `Destination` require exactly one of their sources / targets (`oneOf` of `required`). Output is deterministic; CI (`make check-generated`) fails when the committed files are stale.

## Event vocabulary

Constants in `v1alpha1/events.go`, with the emitting provider types (validation uses this map: `EventType.ProviderTypes()`, `EmittedBy(p)`, `Known()`, and `IsFactory()` for the `factory.` prefix):

| Type | Provider types |
|---|---|
| `issue.opened` `issue.commented` `issue.labeled` `issue.closed` | github |
| `pr.opened` `pr.updated` `pr.ready_for_review` `pr.labeled` `pr.review.submitted` `pr.checks.completed` `pr.merged` `pr.closed` | github |
| `release.published` | github |
| `deploy.succeeded` `deploy.failed` | github |
| `message.posted` `message.mention` | slack (future) |
| `factory.run.completed` `factory.run.failed` `factory.approval.granted` `factory.approval.denied` `factory.approval.expired` `factory.schedule.tick` `factory.workitem.resynced` `factory.command.pause` `factory.command.resume` `factory.command.redispatch` `factory.command.cancel` | **factory only** |

## Effect ops

Canonical names (as used in the PoC catalog), constants in `v1alpha1/effects.go`. `EffectOp.Side()` says who executes it: `SideAPI` (`api`) or `SideGit` (`git`).

| Op | Side | Notes |
|---|---|---|
| `comment`, `pr.comment` | API | issue / PR comment |
| `label.add`, `label.remove` | API | checked against `effects.labels` globs |
| `create-issue`, `sub-issue.add`, `close`, `link-duplicate` | API | |
| `pr.open`, `pr.update`, `pr.retarget`, `pr.merge` | API | `pr.merge` honours repo merge method; falls back to label if review requirement blocks (Q5) |
| `review.comment`, `review.request-changes`, `review.approve`, `thread.reply`, `thread.resolve` | API | reviewer identity only (TaskSpec `identity: reviewer`) |
| `push-branch`, `stack.push`, `stack.rebase` | **git (agent)** | executed in the workspace with the scoped git token; the agent still records them via `POST /effects` (op + ref) for audit |

## Provider interface

Package `internal/provider` (implemented by GH, faked by `provider/fake`):

```go
type Adapter interface {
    Type() string
    Connect(ctx context.Context, p v1alpha1.Provider, secrets SecretResolver) (Conn, error)
}

type Conn interface {
    // Ingress
    Handler(sink IngressSink) http.Handler                       // mounted at Provider.webhook.path; verifies, then sink.Accept (persists) BEFORE acking
    Recover(ctx context.Context, since time.Time, sink IngressSink) error // startup: re-ingest deliveries the store hasn't seen
    Normalise(ctx context.Context, ev RawEvent) ([]v1alpha1.Event, error)
    // Observation
    Observe(ctx context.Context, ref v1alpha1.ObjectRef) (Observation, error)
    List(ctx context.Context, scope v1alpha1.Scope) ([]v1alpha1.ObjectRef, error)
    ResolveGroup(ctx context.Context, g v1alpha1.GroupSource) ([]string, error)
    // Projection + effects
    Project(ctx context.Context, ref v1alpha1.ObjectRef, p Projection) error
    Apply(ctx context.Context, e EffectRequest) (EffectResult, error)
    // Credentials for agents
    MintGitCredential(ctx context.Context, req GitCredentialRequest) (Credential, error)
    Health() HealthStatus
}

type IngressSink interface { Accept(ctx context.Context, ev RawEvent) error } // stores a Delivery record; error → handler returns 503

type Projection struct {
    WorkItemID string
    State      map[string]any
    Counters   map[string]int
    Links      []v1alpha1.Link
    Labels     []string            // factory-owned labels to ensure (others untouched)
}
```

`provider/fake` keeps objects in memory, records effects, and can emit scripted events — used by CORE/RT tests and `factory-mock`.

## HTTP API

`api/openapi.yaml` (OpenAPI 3.1). Base path `/api/v1`. All bodies JSON; times RFC 3339; errors `application/problem+json`.

| Group | Method + path | Auth | Purpose |
|---|---|---|---|
| **agents** | `POST /agents` | join token + owner GitHub token | register; returns `{agentId, agentToken, expiresAt}` |
| | `PATCH /agents/{id}` | agent | status (`Ready`, `Throttled{until}`, `Draining`), lending update |
| | `DELETE /agents/{id}` | agent | deregister |
| **leases** | `GET /agents/{id}/lease?wait=30s` | agent | long-poll; `200 {lease, task, gitCredential}` or `204` |
| | `POST /leases/{id}/heartbeat` | agent | extend; body: progress events batch |
| | `POST /leases/{id}/complete` | agent | body: `Result` + `transcriptRef` |
| | `POST /leases/{id}/abandon` | agent | body: `{reason}` (`throttled`, `agent-error`, `shutdown`) |
| **tasks** | `GET /tasks/{id}` | agent (own lease) / UI | full Task |
| | `GET /tasks/{id}/prompt` | agent (own lease) | rendered prompt (text) |
| | `GET /tasks/{id}/context/{n}` | agent (own lease) | fetched context item (fenced, labelled trust) |
| | `PUT /runs/{id}/transcript` | agent | upload (gzip, ≤ 20 MB) |
| **effects** | `POST /tasks/{id}/effects` | agent (own lease) | `{op, target, args, idempotencyKey}` (`idempotencyKey` **required**, `minLength: 1`) → `{effectId, url}`; 400 if the key is missing, 403 if not allowed |
| **resources** | `GET /workitems`, `GET /workitems/{id}`, `GET /tasks`, `GET /runs/{id}`, `GET /queues`, `GET /pools`, `GET /agents`, `GET /catalog`, `GET /catalog/{kind}/{name}`, `GET /providers` | UI session | read models (list endpoints paginate with `?cursor=`, filter with `?labelSelector=`) |
| **dry-run** | `POST /catalog/dry-run` | UI session | `{event}` → `Evaluation` (no side effects) |
| **commands** | `POST /commands` | UI session (org admin) / CLI token | `{type, target, args}` with `type ∈ pause\|resume\|cancel\|redispatch\|dispatch\|resync-catalog` (shapes in [api-server.md §Commands](../../dispatcher-runtime/LLD/api-server.md#commands-post-commands)) → becomes a `factory.command.*` or injected event |
| **events** | `GET /stream?topics=runs/{id},workitems,agents` | UI session | SSE: `progress`, `workitem.updated`, `task.updated`, `agent.updated` |
| **auth** | `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout`, `GET /auth/me` | — | GitHub OAuth web flow for the UI |
| | `GET /auth/device-config` | — | `{clientId, scopes}` for the agent's device-flow owner login (`factory-agent register`) |
| **webhooks** | `POST /hooks/{provider}` | provider HMAC | delegated to `Conn.Handler()` (not in OpenAPI body schemas; documented as opaque) |
| **ops** | `GET /healthz`, `GET /readyz`, `GET /metrics` | none — served only on the localhost ops listener (`--ops-addr`), never on the public address | — |

Codegen: `oapi-codegen` **Echo strict server** (`echo-server` + `strict-server`) for `internal/apiserver/gen`, client for `internal/apiclient`; web uses `openapi-typescript`. `api/examples/*.json` hold one example per operation response; `factory-mock` and MSW serve them. `factory-mock` is a small `net/http` server that matches each request to its operation in `api/openapi.yaml` and answers with that operation's example; it doesn't use the generated server, so it has no Echo dependency.

**Contract versioning:** additive changes only during the PoC; a breaking change bumps the path to `/api/v2` — not expected in the PoC.

# LLD — Contracts: Go types, schemas, provider interface, HTTP API (FND)

> Workstream: [FND](../../definitions.md#fnd-foundation) · Tasks: FND-S1-T01, FND-S1-T02
> These contracts are the **only** coupling between workstreams. Changing them = a dedicated PR merged before dependents.

## Go types

Package `internal/apis/v1alpha1`. Every kind embeds:

```go
type TypeMeta struct { APIVersion string `json:"apiVersion"`; Kind string `json:"kind"` }
type ObjectMeta struct {
    Name            string            `json:"name"`
    Labels          map[string]string `json:"labels,omitempty"`
    ResourceVersion string            `json:"resourceVersion,omitempty"` // runtime kinds only
    CreatedAt       time.Time         `json:"createdAt,omitempty"`
}
```

### Config kinds (from git)

| Kind | Spec fields (Go) | Source of truth |
|---|---|---|
| `Provider` | `Type string`; `GitHub *GitHubProviderSpec` (`APIURL`, `Webhook{Path, SecretRef}`, `Identities map[string]AppIdentity{AppID, PrivateKeyRef}`); `Slack *SlackProviderSpec` (future); `Scope ProviderScope{Orgs, Channels}`; `Trust []TrustRule{Tier, Group, AuthorAssociation, Bots, WorkspaceMember}`; `Resync Duration` | [03 §Provider](../../../03-domain-model.md#provider) |
| `Group` | `Members []GroupSource{Provider, Team, OrgRole{Org, Role}, Users, UserGroup}`; `Cache Duration` | [03 §Group](../../../03-domain-model.md#group) |
| `Template` | `Bodies map[ProviderType]TemplateBody{Text string; File string}` | [03 §Template](../../../03-domain-model.md#template) |
| `Rule` | `RuleSet string`; `ProviderSelector *LabelSelector`; `On []EventType`; `Where string`; `Params map[string]any`; `Actions []Action`; `Priority int`; `Final bool`; `Debounce *Debounce{Window, Key}`; `Schedule string` | [03 §Rule](../../../03-domain-model.md#rule) |
| `RuleSet` | `ProviderSelector LabelSelector`; `Scope Scope{Repos, Exclude, Channels, Branches}`; `Rules RuleSelection{Refs, Selector *LabelSelector, Exclude}`; `Overrides map[string]map[string]any`; `Defaults map[string]any`; `Budget *Budget`; `StateKeys []string`; inline `Rules` allowed (`InlineRules []Rule`) | [03 §RuleSet](../../../03-domain-model.md#ruleset-mandatory) |
| `TaskSpec` | `Description`; `Inputs map[string]InputSpec{Type, Enum, Default, Required}`; `Instructions{Template, Headless}`; `Skills []SkillRef{Source, Ref, Use}`; `Requires{Labels}`; `Harness{Allowed []HarnessConstraint, Default HarnessRequest}`; `Workspace{Repos []RepoMount{Ref, Mode}, Checkout, PublicReads}`; `Identity string`; `Permissions map[string]string`; `Effects{Allow []EffectOp, Labels []string}`; `Limits{MaxTurns, MaxWallClock, MaxCostUSD}`; `Output{Schema, Outcomes}` | [03 §TaskSpec](../../../03-domain-model.md#taskspec) |
| `Queue` | `Pool string`; `Concurrency int`; `Priority int`; `Budget *Budget`; `Retry{Max, On []RetryReason}` | [03 §Queue and Pool](../../../03-domain-model.md#queue-and-pool) |
| `Pool` | `Mode string` (`registered`\|`ephemeral`); `Admission{TokenRef, Owners}`; `Lease{TTL, Heartbeat, MaxTasksPerAgent}`; `Require{Harnesses}`; `Fairness string`; ephemeral: `Backend`, `Image`, `Scaling`, `LLMAuth` | same |

`Action` is a tagged union — exactly one of:

```go
type Action struct {
    Dispatch        *DispatchAction        `json:"dispatch,omitempty"`        // TaskSpec, Queue, Inputs map[string]any, Harness *HarnessRequest, Requires *Requires, Context []string, Target *Target
    Set             map[string]any         `json:"set,omitempty"`             // values: literal or "=CEL"
    Notify          *NotifyAction          `json:"notify,omitempty"`          // Template, To []Destination{Group, Via, As, Channel{Provider,Name}, Item}
    RequestApproval *ApprovalAction        `json:"requestApproval,omitempty"` // future (Q18) — accepted by schema, engine returns Unsupported in PoC
    Cancel          *CancelAction          `json:"cancel,omitempty"`          // TaskSpec
    Link            *LinkAction            `json:"link,omitempty"`            // From string (CEL)
    Effect          *EffectAction          `json:"effect,omitempty"`          // Op EffectOp + op-specific fields (map)
}
```

### Runtime kinds (in the store)

| Kind | Key fields |
|---|---|
| `Event` | CloudEvents: `ID`, `Source` (provider instance or `factory`), `Type EventType`, `Subject`, `Time`; `Data EventData{Provider, Actor{ID, Tier, IsBot, IsFactory, Identity}, Object ObjectRef, Repo, ContentRef, Facts map[string]any, Raw json.RawMessage}`; `WorkItem string` (set for `factory.*`) |
| `WorkItem` | `Spec.Links []Link{Provider, Ref, Kind, Primary}`; `Status.State map[string]any`, `Counters map[string]int`, `Decisions []Decision` (bounded, newest 200), `Reporter Actor` |
| `Task` | `Metadata{ID, WorkItem, Provenance{Event, RuleSet, Rule, CatalogSHA}}`; `Spec` (resolved TaskSpec digest, Queue, Pool, Provider, Target, Inputs, Harness{Request, Resolved}, Skills (with commit), Prompt{Ref, Digest}, Context, Credentials{Git{Provider, Identity, Repo, ReadRepos, Permissions, TTL}, LLM:"agent-local"}, Effects, Limits, Output); `Status{Phase, Agent, Reason, Attempts, LeaseExpiresAt}` |
| `Run` | `Task`, `Attempt`, `Agent`, `Owner`, `StartedAt`, `EndedAt`, `Progress []ProgressEvent` (ring buffer), `Result *Result`, `TranscriptRef`, `Usage` |
| `Agent` | `Spec{Pool, Owner, Capabilities{Harnesses []HarnessOffer{Name, Version, Auth, Models, ReasoningEffort}, Labels}, Lending{Unlimited, Queues, RuleSets, Harnesses, MaxConcurrent, MaxRunsPerDay, MaxTokensPerDay, Hours}}`; `Status{Phase, ThrottledUntil, LastHeartbeat, Running []TaskID, Today{Runs, Tokens}}` |
| `Effect` | `Task`, `Op`, `Target ObjectRef`, `IdempotencyKey`, `Identity`, `Request`, `Result{URL, Error}`, `At` |
| `BudgetCounter` | name `budget.<scope>.<name>.<YYYY-MM-DD>`; `Runs`, `Tokens`, `USD` (CAS-updated by the scheduler) |
| `Approval` | type only in PoC (Q18) |

### Result

Exactly the JSON in [03 §Result](../../../03-domain-model.md#result): `Task`, `Status` (`done|failed|needs_input|noop`), `Outcome`, `Summary`, `Verdict map[string]any`, `Artifacts []Artifact`, `Suggest []Suggestion`, `Question *string`, `Harness{Name, Version, Auth, EffectiveConfig, Usage{InputTokens, OutputTokens, Turns, WallClock, CostUSD *float64}}`, plus spec-specific fields under `Data map[string]any` validated by the TaskSpec's output schema.

### Capability descriptors

```go
type AdapterCapabilities struct {          // published by each harness adapter (static per adapter version)
    Harness          string
    EffortMapping    map[string]string      // normalised effort → native value
    Features         []string               // "structured-output", "mcp", "plugins", "skills-dir"
}
```

`factory validate --capabilities caps.json` validates patterns against them; the agent's `Probe` produces per-machine offers (`HarnessOffer`).

## Event vocabulary

Constants in `v1alpha1/events.go`, with the emitting provider types (validation uses this map):

| Type | Provider types |
|---|---|
| `issue.opened` `issue.commented` `issue.labeled` `issue.closed` | github |
| `pr.opened` `pr.updated` `pr.ready_for_review` `pr.labeled` `pr.review.submitted` `pr.checks.completed` `pr.merged` `pr.closed` | github |
| `release.published` | github |
| `deploy.succeeded` `deploy.failed` | github |
| `message.posted` `message.mention` | slack (future) |
| `factory.run.completed` `factory.run.failed` `factory.approval.granted` `factory.approval.denied` `factory.approval.expired` `factory.schedule.tick` `factory.workitem.resynced` `factory.command.pause` `factory.command.resume` `factory.command.redispatch` `factory.command.cancel` | **factory only** |

## Effect ops

Canonical names (as used in the PoC catalog). `Side` says who executes it.

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

Codegen: `oapi-codegen` strict-server for `internal/apiserver/gen`, client for `internal/apiclient`; web uses `openapi-typescript`. `api/examples/*.json` hold one example per operation response; `factory-mock` and MSW serve them.

**Contract versioning:** additive changes only during the PoC; a breaking change bumps the path to `/api/v2` — not expected in the PoC.

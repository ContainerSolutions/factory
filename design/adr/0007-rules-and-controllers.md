# ADR-0007: Behaviour is rules over provider-neutral events; the dispatcher is a set of controllers over a resource store

- **Status:** proposed
- **Date:** 2026-10-15

## Context
The first draft modelled behaviour as per-work-item state machines (flows) plus role-specific settings in repo bindings (`autoStart`, `reviewHumanPRs`, `maxFixIterations`, merge method). That made policy read as hard-coded: the engine would need to know what "triage", "fix" or "merge" mean. We want to write behaviour as declarations like "on a new issue from a non-member, dispatch triage to queue X with harness Y". Capacity (spawning agents vs. registered agents) should be defined separately, and each rule should name the provider instance(s) it applies to (public GitHub, GitHub Enterprise, Slack).

## Decision
- **Config kinds**, all in git (ADR-0006):
  - `Provider`: how to connect to one provider instance (endpoint, identities, trust tiers, scope; secrets by reference).
  - `Rule`: **reusable and scope-less**. `providerSelector` (type compatibility, for validation), `on`, `where` (CEL), declared `params`, and `actions` (`dispatch`, `set` (values prefixed `=` are CEL), `notify`, `requestApproval`, `cancel`, `link`, `effect`), plus `priority`/`final`/`debounce`/`schedule`.
  - `RuleSet`: **mandatory**. It binds provider instances (`providerSelector`), sets `scope`, selects rules (`refs` ∪ `selector`, minus `exclude`), and overrides params. Rules fire only through RuleSets.
  - `TaskSpec`: what and how for a role. No triggers or transitions. Harness constraints may use patterns.
  - `Queue` → `Pool`: concurrency, priority, budget. Pools are registered (lease) or ephemeral (scaling).
  - `Group` (who) and `Template` (message bodies per provider type).
- **A built-in, reserved `factory` provider** is the only source of `factory.*` events: `factory.run.completed/failed`, `factory.approval.granted/denied/expired`, `factory.schedule.tick`, `factory.workitem.resynced`, `factory.command.*`. Provider types declare the neutral event vocabularies they emit (`issue.*`, `pr.*`, `release.*`, `message.*`).
- **Cascades happen through events only.** Run outcomes, approvals and provider echoes (e.g. `pr.opened` by the factory) trigger the next rules. There are no built-in flows.
- **Human gates are `Approval` resources** (approvers Group, quorum, channels: UI, Slack, GitHub comment or label, expiry). They resolve into provider-neutral approval events.
- **Harness config fields accept patterns** (`*`, `?`, `{a,b}`, opt-in `re:`). They're resolved against agent offers at scheduling time, and the effective values are reported back.
- **Engine invariants are limited to safety rails:** kill switches, budget ceilings, per-item run cap, single active run per (item, task spec), dedupe per (event, rule), credential scoping, effects allow-list.
- **The dispatcher** is one binary built like a small Kubernetes control plane:
  - an API server;
  - a watchable resource store (in-memory first, ADR-0001) with queues;
  - controllers: catalog-sync, provider, rule, work-item, approval, scheduler, pool and run;
  - the UI.

## Consequences
- Every behaviour change is a catalog PR, testable by replaying recorded events (`factory rules test`) and explainable per decision in the UI.
- Rule authors can write loops or conflicting rules. The per-item run cap, `final`/`priority`, CI decision diffs and the UI's "why" view are the mitigations.
- The CEL environment (`event`, `actor`, `item`, `run`, `provider`) becomes a public API and must be versioned.
- The resources are Kubernetes-shaped, so moving config kinds to CRDs on a real cluster later is possible but not required.

## Alternatives considered
- **State machines per flow** (first draft): explicit, but policy leaks into the engine and repo bindings, and the main building block is heavy for simple triggers.
- **A general workflow engine** (Temporal, Argo, agent-orca `AgentWorkflow`): sequencing in code or DAGs, with state hidden from the factory's own model; see ADR-0001.
- **Hard-coded handlers per role:** fastest to build, but every behaviour change becomes a code change.

# ADR-0001: The factory owns work state (in-memory first); providers hold a rebuildable projection

- **Status:** proposed
- **Date:** 2026-10-15

## Context
Work items live for hours or days and can span providers: a Slack thread, then a GitHub issue, then a PR. Humans interleave at any point. Webhooks get lost or duplicated, and the factory's own writes echo back as events. Slack has no labels, and GitLab (not planned, not ruled out) has different primitives. We want to start simple, with no database to operate.

## Decision
- Work items, links to provider objects, state keys (set by rules, ADR-0007), run counters, tasks and runs live in the **factory's state store**, using a provider-neutral model behind a `Store` interface.
- The first implementation is **in-memory**. A single dispatcher replica holds it.
- The dispatcher writes a **projection** of each work item to its provider objects:
  - GitHub: `factory:*` labels plus a hidden marker comment;
  - Slack: message metadata.
- The projection must carry everything rules need to continue: id, state keys, run counters and links.
- On startup, and on resync, the dispatcher **rebuilds and reconciles** by listing the bound scopes and reading the projections plus the current provider state. Human actions (relabel, approve, merge by hand, reply) are observed and merged in. This is level-triggered reconciliation.

## Consequences
- No database is needed for the PoC, and state is never "lost" in a way that stalls work.
- History (past runs, transcripts, cost) is lost on restart until a durable store (Postgres) replaces the in-memory one. The UI shows only what has happened since the last start.
- The dispatcher is single-replica while the store is in-memory.
- The projection format is an API. Version it (a `v=` field in the marker) and keep it backward-compatible.
- Every provider adapter must implement `List` and `Observe` well enough to rebuild from scratch.

## Alternatives considered
- **GitHub labels as the only state** (the first draft of this ADR): no own store, but it locks the factory to GitHub, doesn't fit Slack, and has no home for cross-provider links or run history.
- **Durable DB from day one:** more ops before we know the model. The `Store` interface keeps it a drop-in later.
- **Workflow engine as the source of truth** (Temporal, Argo, agent-orca `AgentWorkflow`): hidden state that drifts from what humans see, and poor at modelling humans acting out of band.

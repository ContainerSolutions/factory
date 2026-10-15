# ADR-0002: The dispatcher emits immutable Task manifests consumed by pluggable runtime backends

- **Status:** proposed
- **Date:** 2026-10-15

## Context
The runtime is undecided: GitHub Actions, Kubernetes, hosted sandboxes, agent-orca, kagent or Managed Agents. The brief asks for both "dispatch to a registered agent" and "create a new agent just for this task".

## Decision
- The dispatcher never runs an LLM. Its output is a **Task**: an immutable, self-contained manifest holding the task spec digest, the rule and catalog SHA that produced it, its queue and pool, the harness and model, pinned skills, the pre-rendered prompt reference, context references, the credential scope, the effects allow-list, limits and the output schema.
- Tasks wait in a **Queue** and are drained by the Queue's **Pool** (ADR-0007). Pool backends materialise Tasks. *Ephemeral* backends do it by push (`Launch(Task)`); *registered agents* do it by pull (`Register` / `Lease` / `Heartbeat` / `Complete`).
- The same `factory-agent` runner executes a Task on every backend.

## Consequences
- The runtime choice is reversible. New backends are adapters (an Actions `workflow_dispatch`, a k8s `SandboxClaim`, an orca `AgentRun`, a Managed Agents session).
- Tasks are replayable for debugging and evals.
- Large context must travel by reference, so the runner fetches it from the Factory API (or a blob store).

## Alternatives considered
- Dispatcher calls harnesses directly: couples routing to the runtime and loses replayability.
- Adopt one platform's manifest (e.g. orca `AgentRun`) as our Task: binds us to that platform's agent contract, which doesn't fit Claude Code or Codex today (see 06).

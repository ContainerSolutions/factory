# ADR-0005: The PoC is one plain Go binary (not Kubernetes) with registered agents only

- **Status:** proposed
- **Date:** 2026-10-15

## Context
The PoC targets `docs`. It should run on registered agents on maintainers' machines: **Codex on the owners' ChatGPT plans** for default traffic, and **Claude Code on org API keys** mostly for validation. So every agent is a registered agent outside any cluster. State is in-memory and the config is central (ADR-0001, ADR-0006), so a long-running dispatcher is required. No existing tool covers the rule layer plus capability-matched pools of harness agents ([10](../10-poc.md#is-there-tooling-that-already-matches)).

## Decision
- **Dispatcher:** one Go binary, one replica, **not on Kubernetes**. It contains the API, the in-memory store and queues, the controllers and the UI. Ingress is the GitHub App webhook (through a tunnel for the PoC), plus optional Slack Socket Mode.
- **Pools:** one registered pool, `maintainer-agents`. Agents use the lease protocol in [10](../10-poc.md#agent-lifecycle-and-lease-protocol) and run the harnesses under their owners' own logins (ADR-0008).
- Keep the Kubernetes *shape* (resources, selectors, controllers, `Store` interface), so config kinds can become CRDs later if ephemeral pod pools or GitOps call for it.
- agent-orca, kagent, Claude Managed Agents, and GitHub self-hosted runners as the pool are **not adopted**.

## Consequences
- No cluster and no database. The PoC runs from a laptop or a small VM.
- Throughput depends on owners' machines being online and on their plan limits. That's visible in the UI as agent state and throttling.
- Capacity that doesn't depend on people (ephemeral pools with API keys) is future work.
- Codex (owners' plans) is the default harness, and Claude Code (API keys) is mostly for validation (ADR-0008). The Codex plan-terms check (Q15) should be done before real traffic.

## Alternatives considered
- **Kubernetes operator from day one:** the main runtime value (pods) goes unused with registered agents, our own API is needed anyway, and runtime kinds churn too much for etcd.
- **Actions runtime (ephemeral) for the PoC:** needs API keys, and doesn't exercise the registered-agent model we want to validate.
- **GitHub self-hosted runners as the registered pool:** free registration and logs, but no capability or pattern matching, consent or throttling. It runs workflow code on personal machines and ties pools to GitHub.

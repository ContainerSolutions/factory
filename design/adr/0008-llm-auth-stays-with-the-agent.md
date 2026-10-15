# ADR-0008: LLM authentication stays with the agent; the factory never handles LLM credentials

- **Status:** proposed
- **Date:** 2026-10-15

## Context
Registered agents run on maintainers' machines. **Codex** is the default harness and uses the owner's **ChatGPT plan** login. **Claude Code** reviews and validates; in the PoC it uses its owner's subscription, and beyond the PoC **Anthropic API keys**, the documented path for products and services. Anthropic's terms forbid developers from collecting, storing or intermediating subscription credentials. OpenAI's Codex docs recommend API keys for programmatic and CI workflows, and Enterprise access tokens for automation needing workspace access. Its Terms forbid sharing credentials or letting others use an account. Running the official CLI under your own login on your own machine isn't prohibited by anything we found (Q15).

## Decision
- Tasks carry `credentials.llm: agent-local` for registered agents. The dispatcher **never** stores, forwards, mints or proxies LLM credentials or session tokens.
- **Codex:** agents run the **unmodified** `codex` binary, signed in by their **owner** through OpenAI's own flow, in a dedicated `CODEX_HOME`.
- **PoC (confirmed):** both Codex and Claude Code run locally on their owners' subscriptions. The PoC is the factory's developers running and validating their own system on their own machines.
- **Claude Code (beyond the PoC):** agents use an **org-provisioned Anthropic API key**, one per agent or owner, held only in the worker's environment. Claude agents accept the `validation` queue only.
- Agents are owned by named people in the pool's `owners` Group. Owners declare **consent** (`lending`: queues, RuleSets, harnesses, concurrency, hours, runs and tokens per day, or unlimited) and can pause at any time.
- Ephemeral pools (later) use org API keys (`Pool.llmAuth.apiKeyRef`), injected only into the agent process.
- Results record `auth: subscription | apiKey | cloud`, the agent and the owner for audit. Subscription runs count runs and tokens against budgets; API-key runs also count USD.

## Consequences
- Clean separation: the factory is an orchestrator, not an LLM access broker.
- The Claude side follows the documented API-key path.
- Codex in the PoC runs on owners' own logins, on their own machines, with device-code sign-in and credentials never copied. Beyond the PoC, Codex moves to API keys or Enterprise access tokens (`auth: apiKey | accessToken`), with no design change.
- Codex capacity is per-owner (plan windows), and Claude cost is per key. Both are surfaced in the UI.

## Alternatives considered
- **A shared subscription login on a shared box:** that's credential intermediation and outside the subscription terms.
- **API keys for both harnesses:** simplest compliance story, but it gives up running default traffic on Codex plans. It stays available as `auth: apiKey` Codex agents.

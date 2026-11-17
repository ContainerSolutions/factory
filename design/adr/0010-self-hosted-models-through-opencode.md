# ADR-0010: Self-hosted models through OpenCode and a gateway model-endpoint route

- **Status:** proposed
- **Date:** 2026-11-17

## Context
The company bought an NVIDIA DGX Spark to serve its own models. vLLM serves them there, on an
OpenAI-compatible `/v1` API that takes an API key (`--api-key`). The agents run in Brazil and
the Spark is in the UK, joined by a tunnel (Tailscale is recommended; a WireGuard network or an
SSH tunnel works too).

Factory runs two harnesses today, and each is tied to its vendor's login: `codex` to a ChatGPT
login and `claude-code` to an Anthropic token (ADR-0003, ADR-0008). Each runs in a VM workspace
whose gateway injects the credential, so the VM never holds one
([gateway.md](../workstreams/agent/LLD/gateway.md)).

The Spark's address is private: Tailscale's `100.64.0.0/10`, a WireGuard or LAN range, or
`127.0.0.1` for an SSH tunnel. The gateway's egress check denies every one of those, and its
only exception list (`Egress.OpenAddrs`) is a test hook, set nowhere in production.

## Decision
- **OpenCode is the third harness**, `opencode`, with the new auth `selfHosted`. It is
  MIT-licensed and works with any provider. It talks plain OpenAI chat completions
  (`@ai-sdk/openai-compatible`), so any model vLLM serves works. It has a headless mode
  (`opencode run --format json`), MCP and `SKILL.md` skills. NVIDIA's Spark playbook for CLI
  coding agents lists it.
- **The Spark is reached only through a new gateway route, `model.factory.internal`.** It is
  built like the dispatcher route:
  - the VM sends plain HTTP to the synthetic name through `HTTP_PROXY`, so the CA doesn't
    change;
  - the upstream is fixed by the supervisor's config, not chosen by the VM;
  - the route has its own transport, which dials only that URL and skips the egress check;
  - the gateway strips every credential carrier and injects `Authorization: Bearer <key>`;
  - only `POST /v1/chat/completions` and `GET /v1/models[/<id>]` pass.
- **The key is held by the agent host.** `factory-agent login opencode` stores it in the
  agent's state dir (0600). Only an opencode task's gateway gets it. It never reaches the VM,
  the run bundle, a file in the workspace or a log. The VM's config carries
  `harness.Placeholder`.
- **This is consistent with ADR-0008:** the dispatcher never sees, stores or forwards the key.
  Results record `auth: selfHosted`.
- **opencode's headless interface is unofficial.** Every name the adapter relies on (flags,
  environment variables, config keys, stream events) is a constant in the adapter's `flags.go`,
  marked UNVERIFIED. The owner's first run on a VM workspace verifies them.
- Design: [harness-adapters.md §OpenCode](../workstreams/agent/LLD/harness-adapters.md#opencode),
  [gateway.md §The model endpoint](../workstreams/agent/LLD/gateway.md#the-model-endpoint) and
  [workspace-run.md §opencode](../workstreams/agent/LLD/workspace-run.md#opencode).

## Consequences
- **The same Task can run on the Spark.** A command override picks opencode, so a role can be
  compared across harnesses (ADR-0003's A/B); the TaskSpecs' defaults stay on codex and
  claude-code until a run has shown opencode works.
- **No USD cost.** Self-hosted runs count runs and tokens against budgets, never USD (only
  `apiKey` runs do).
- **One Spark's throughput is shared by every agent.** vLLM batches concurrent streams, and
  each one slows down as more join. The agents that offer opencode start with a low
  concurrency (about 2).
- **A Spark or tunnel that is down throttles the task instead of failing it.** The gateway's
  `upstream` and `timeout` answers put the task back in the queue.
- **The key crosses the tunnel.** With an `http` endpoint URL it is sent in clear on the hop from
  the agent host to the Spark. So that URL is for a tunnel that encrypts (Tailscale, WireGuard,
  SSH); anything else uses https.
- **Open models must write a schema-valid Result** to `$FACTORY_RESULT`. The pipeline validates
  it and repairs. If repairs keep failing, the fallback is a `result` tool on the Effector's MCP
  server, which validates inside the loop.
- **opencode's names can change between releases.** The version is pinned in the image, and a
  fix is confined to `flags.go`.

## Alternatives considered
- **codex with a custom `model_providers` block:** codex talks only to vLLM's Responses API,
  whose gaps need a compatibility proxy, and its prompts are tuned for OpenAI's models.
- **claude-code with `ANTHROPIC_BASE_URL` at vLLM's Anthropic endpoint:** claude-code 2.1.154
  and later sends message roles that vLLM rejects, and its prompts are tuned for Claude.
- **Opening the Spark's address through `Egress.OpenAddrs`:** anything in the VM could then reach
  the private address, not only the model client, and the gateway would have no point at which
  to inject the key, so the key would have to be in the VM.

# LLD — Agent worker (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Task: AGT-S1-T01 · Packages: `internal/agent/worker`, `internal/agent/config`, `cmd/factory-agent`
> Protocol: [scheduler-and-pools.md](../../dispatcher-runtime/LLD/scheduler-and-pools.md), [10 §Registered agents](../../../10-poc.md#registered-agents-codex-on-subscriptions-claude-on-api-keys)

## CLI

```text
factory-agent register [--config …]                                   # device-flow owner login → agent registration
factory-agent worker  [--config ~/.config/factory-agent/agent.yaml]   # (registers if needed) + lease loop
factory-agent run     --task <id>                                      # one task, then exit (ephemeral pools, later)
factory-agent probe   [--json]                                         # print capabilities (no network to dispatcher)
factory-agent fx <op> [flags]                                          # Effector CLI (also installed as `factory` on PATH in runs)
factory-agent mcp                                                      # Effector MCP server (stdio)
factory-agent stack up|wait|down                                       # local stack helper
factory-agent version
```

## Local config (owner-controlled)

```yaml
# ~/.config/factory-agent/agent.yaml — never committed; permissions 0600
dispatcher: https://factory.example.dev
pool: local
joinTokenFile: ~/.config/factory-agent/join-token          # given by the pool owner out of band
labels: [has-local-stack, gif-tooling]
harnesses:
  codex:
    home: ~/.factory-agent/codex-home        # dedicated CODEX_HOME, signed in once with `codex login --device-auth`
    models: [gpt-5-codex]                    # what this owner's plan allows
    efforts: [low, medium, high]
    default: gpt-5-codex
  claude-code:
    configDir: ~/.factory-agent/claude       # dedicated CLAUDE_CONFIG_DIR (subscription login, PoC)
    models: [claude-sonnet-5-5, claude-opus-5-5]
    efforts: [low, medium, high, max]
    default: claude-sonnet-5-5
localDev: ~/repos/cs/ccf/local-dev           # required for has-local-stack
workRoot: ~/.factory-agent/work              # scratch workspaces
lending:
  unlimited: true                            # or: queues, ruleSets, harnesses, maxConcurrent, maxRunsPerDay, maxTokensPerDay, hours
```

`lending` changes take effect on `SIGHUP` (re-sent via `PATCH /agents/{id}`).

## Startup

1. Load + validate config (unknown keys are errors).
2. `probe`: for each configured harness run the adapter's `Probe` (version, auth status, configured models/efforts); verify labels with the **full probe lists in [local-stack.md §Labels and probes](local-stack.md#labels-and-probes)** (`has-local-stack`: Docker, compose v2, `localDev` layout, memory; `gif-tooling`: Playwright **with installed browsers**, `gifski`, `vhs`, **`ffmpeg`**). A label is advertised only when every prerequisite passes; otherwise it's **dropped** with a warning. **Base tooling** (required, or the worker refuses to start): `git`, `gh` with the `gh-pr-review` and `gh-stack` extensions, `jq`, `python3` (used by `lisa` and `ccf-review`'s `ccfr`).
3. Owner identity: `factory-agent register` runs a **GitHub OAuth device flow** against the dispatcher's OAuth client (`GET /api/v1/auth/device-config` gives the client ID). The owner approves in their browser; the resulting short-lived user token is sent once in `POST /agents` and dropped from memory. **No `gh` login is needed or used on the worker's OS user**, so no personal GitHub credential sits where a task's harness could read it.
4. Register → `agentToken` kept in memory only (re-register on restart).

## Lease loop

```text
for !draining:
    if running < lending.maxConcurrent (default 1):
        resp := GET /agents/{id}/lease?wait=30s
        if 204: continue
        go execute(resp)                      // run-pipeline.md
    else: wait for a slot
execute(lease):
    hb := ticker(lease.heartbeatEvery)        // POST /leases/{id}/heartbeat with progress batch + token deltas
    on hb response cancel=true → cancel run ctx
    on hb response refreshedGitCredential → swap credential in the helper
    result := pipeline.Run(ctx, task, cred)
    switch:
      throttled  → POST abandon{reason: throttled, until}; PATCH agent {phase: Throttled, until}
      error      → POST complete{status: failed, reason}
      ok         → POST complete{result, transcriptRef}
```

- **Draining:** `SIGTERM`/`SIGINT` → `PATCH {phase: Draining}`, stop leasing, wait for running tasks up to their remaining wall-clock (second signal = abandon all with `shutdown`).
- **Network loss:** exponential backoff on dispatcher calls; a lease may expire server-side — the agent detects `404/409` on heartbeat and kills the run.

## Tests

- Against `factory-mock` (FND-S1-T02): register → lease canned task → heartbeats → complete.
- Config validation table; probe dropping a failed label.
- Drain behaviour with a fake pipeline; heartbeat `cancel` → run context cancelled.

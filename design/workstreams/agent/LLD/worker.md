# LLD — Agent worker (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Task: AGT-S1-T01 · Packages: `internal/agent/worker`, `internal/agent/config`, `cmd/factory-agent`
> Protocol: [scheduler-and-pools.md](../../dispatcher-runtime/LLD/scheduler-and-pools.md), [10 §Registered agents](../../../10-poc.md#registered-agents-codex-on-subscriptions-claude-on-api-keys)

## CLI

```text
factory-agent register [--config …]                                   # device-flow owner login → agent registration
factory-agent worker  [--config ~/.config/factory-agent/agent.yaml]   # (registers if needed) + lease loop
factory-agent run     --proxy <gateway> [--bundle /var/lib/factory/run] …      # the in-VM runtime: one task from a run bundle (workspace-run.md)
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

**`factory-agent run` is the in-VM runtime (P1 phase 3),** not the one-task ephemeral mode first sketched here. It runs inside a VM workspace, reads the run bundle the supervisor wrote to `/var/lib/factory/run`, runs the pipeline with no token of any kind (the workspace's gateway adds every credential), streams JSONL events on stdout and writes `out/outcome.json` for the supervisor. It never talks to the dispatcher's agent routes, so it needs no config, registration or lease. See [workspace-run.md](workspace-run.md#factory-agent-run).

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
    on hb response refreshedGitCredential (+ refreshedReadCredentials) → swap credentials in the helper
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

## Decisions taken in AGT-S1-T01

- **Packages.** `internal/agent/config` (the local config), `internal/agent/owner` (the owner login, `owner.TokenSource`), `internal/agent/probe` (capabilities), `internal/agent/runner` (the `Runner` seam, `Job`, `Outcome`, `Sink`, the `Stub`) and `internal/agent/worker` (registration, lease loop, heartbeats, reports, drain). `internal/agent/internal/fakegh` is a test-only fake GitHub (device flow and `GET /user`). `cmd/factory-agent` has `version`, `worker` and `probe`, one file each and one line each in `main.go`; no arguments still prints the version.
- **Owner token: the device flow, as this LLD says** (the task text's `gh auth token` would put a personal GitHub credential on the worker's OS user, which step 3 rules out). `owner.DeviceFlow` reads `{clientId, scopes}` from `GET /auth/device-config`, runs GitHub's device flow (`/login/device/code`, then polls `/login/oauth/access_token`, honouring `slow_down`, `expired_token` and `access_denied`), and prints the verification URL and user code on stderr, never through the logger. The token goes once in `X-Factory-Owner-Token` (with `X-Factory-Join-Token`) and is dropped; transient registration retries reuse it.
- **No `register` subcommand.** The agent token is in memory only (step 4), so a separate `register` would have nothing to hand to `worker`; `worker` registers itself. A restart therefore means a new device login. Any agent call answered `401` registers again once (a new device login) and retries; a refused registration (no join token, a join token readable by others, the owner denied the login, `400`/`401`/`403`) is fatal: `Run` returns the error and later calls don't try again.
- **Deregister on exit** (the LLD is silent): after a drain or an abort the worker calls `DELETE /agents/{id}`. The token can never be used again anyway, so revoking it costs nothing, and the agent shows Offline at once instead of Draining for 24 h.
- **Heartbeats.** The first one goes at once (the task turns Running and a lost lease shows up immediately), then every `lease.heartbeatEvery` on the injected clock. Progress and usage deltas are batched between beats (at most 200 events; the oldest are dropped with a note); a failed heartbeat keeps its batch for the next. Before reporting, a last heartbeat flushes what the run sent since the previous one. `refreshedGitCredential` is swapped into `Job.Git`; `cancel: true` cancels the run, which is then **abandoned with `agent-error`** ("cancelled by the dispatcher"): the run controller takes an abandon of a Cancelling task as the acknowledgement (Cancelled). `404`, `409` or `403` on a heartbeat means the lease is gone: the run is cancelled and nothing is reported.
- **Reports.** Throttled → `abandon {throttled, until}` and `PATCH {Throttled, until}` (the scheduler already throttles on the abandon; the PATCH is what this LLD lists), `until` defaulting to now + 15 min (harness-adapters.md), and the worker doesn't poll again before `until`. A runner error or an Outcome without a Result → `complete` with `Result{status: failed, summary: "<reason>[: <error>]"}` (≤ 500 characters): the Result has no reason field. Otherwise `complete {result, transcriptRef}`, with `result.task` filled in. `complete` and `abandon` retry transient failures 5 times; registration and the long poll back off exponentially (1 s doubling to 1 min, no jitter: a handful of agents per pool).
- **Drain.** The first SIGTERM/SIGINT stops leasing at once: the parked long poll is cancelled (a lease that still arrives is abandoned with `shutdown`), then `PATCH {Draining}`. Each running task may finish until its start + `limits.maxWallClock` (1 h when the TaskSpec sets none); then it is cancelled and abandoned with `shutdown`. The second signal cancels every run at once (abandon `shutdown`). A run that produced its Result just as the shutdown hit is still completed. SIGHUP re-reads the config and re-sends only `lending` (and the local concurrency); other changes need a restart.
- **The `Runner` seam** (`runner.Runner.Run(ctx, *Job, Sink) (Outcome, error)`) is what AGT-S1-T02's pipeline implements, so `Pipeline.Run(ctx, task, cred)` becomes a thin adapter. `Job.Lease` is the lease response passed through unchanged (the `gh` stack's `readCredentials` will simply appear there), `Job.Git` the current git credential (redacted in `String`, `Format`, `LogValue` and JSON), `Job.API` the dispatcher client already authenticated as the agent, `Job.AgentToken` the token for `FACTORY_TOKEN`, and `Sink` takes progress and usage deltas. `Outcome` is run-pipeline.md's. Until the pipeline exists `factory-agent worker` refuses to start unless `--stub-result <result.json>` (development) answers every task with that Result; AGT-S1-T02 changes `newRunner` in `cmd/factory-agent/worker.go`.
- **Config.** Path `~/.config/factory-agent/agent.yaml` (this LLD), `--config` to override. Decoded as YAML 1.2 strictly, like v1alpha1 (YAML node → JSON → `DisallowUnknownFields`; v1alpha1's converter is unexported, so the package has its own copy). Two keys beyond the example: `host` (default: the machine's short host name, lower-cased and reduced to the dispatcher's `^[a-z0-9][a-z0-9-]*$`) and `githubURL` (the device flow's GitHub, default `https://github.com`; GHE and tests). Validation: `dispatcher`/`githubURL` absolute http(s) without credentials, query or fragment, plain http only on loopback (the agent token travels on every call); `pool` and an absolute `joinTokenFile` required; harnesses only `codex` (needs `home`) and `claude-code` (needs `configDir`), at least one model, efforts from `minimal, low, medium, high, max`, `default` among the models; `has-local-stack` needs `localDev`; `lending` must lend something (`unlimited: true` or `queues`), and `unlimited` with lists or daily caps is refused rather than silently ignored (`maxConcurrent` still applies; default 1). `lending.hours` is left to the dispatcher's validation. Paths take `~/`. **Permissions:** the config file must not be writable by group or others (whoever can write it can send the owner's login to another dispatcher; readable is only warned about), and the join-token file must not be accessible to group or others at all (0600 or stricter), checked at each registration. Errors never carry a token.
- **Probe.** `probe.Prober` has the adapters' `Probe(ctx, config.Harness) (HarnessOffer, error)` signature; until AGT-S1-T03/T04 register theirs, `probe.Configured` offers the config's models, efforts and default with `auth: subscription` (the PoC's mode). A harness whose probe fails is dropped with a warning. Labels go through `probe.LabelChecker`s; a label without a checker is advertised as configured until local-stack.md's probes land. Base tooling is checked (`git`, `gh`, `jq`, `python3` on PATH, and `gh-pr-review`, `gh-stack` in `gh extension list`, which needs no gh login): without it, or without a harness, `worker` refuses to start and `probe` exits 1. `factory-agent probe [--config] [--json]` prints the report and never calls the dispatcher.
- **Tests.** The done criterion runs against `factory-mock` built by the test (`go build ./cmd/factory-mock`, skipped with `-short`): device-config from the mock, register → lease the canned task → heartbeat → complete, drain, deregister. The same flow runs against the real dispatcher: the test builds `cmd/factory` and runs `factory serve --dev-fake-providers` as a subprocess (test-only secrets in the environment, free loopback ports, `--github-url`/`--github-api-url` at the fake GitHub, which verifies the owner), waits for `/readyz`, posts the recorded webhook (copied to `testdata/`) to `/hooks/github-public`, and checks through the API with the CLI token that the plan-epic task succeeded with the stub's progress on its Run and that the agent ended Offline; serve then stops on SIGTERM. The catalog is the PoC's copied with one change: `maintainers` lists the owner by login, since the subprocess's fake provider can't be seeded with the CCF org admins. Skipped with `-short`. Throttle, drain deadlines, heartbeat timing and backoff run on `clock.Fake`; every test that logs greps the logs for the join, owner, agent and git tokens.

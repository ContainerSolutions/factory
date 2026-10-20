# LLD — Local CCF stack and media tooling (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Task: AGT-S1-T06 · Package: `internal/agent/stack`
> Used by: `release-docs` (depth `full`) and `docs-review` with `verifyAgainst`; skills `docs-gif`, `docs-verify`, `ccf-test`.

## Labels and probes

| Label | Probe (all must pass) |
|---|---|
| `has-local-stack` | `docker info` ok; `docker compose version` ≥ v2; `localDev` path has `Makefile` and `compose/`; ≥ 8 GB free memory reported by Docker |
| `gif-tooling` | `npx playwright --version` (browsers installed: `npx playwright install --dry-run` clean), `gifski --version`, `vhs --version`, `ffmpeg -version` |

## Versions to pin

```go
type StackVersions struct { API, UI, Agent string; Plugins map[string]string }
func FromTrain(t TrainFacts) StackVersions      // from the train state JSON (fetched via the task context)
func FromRelease(repo, tag string) StackVersions // single-release variant
```

Mapping to `local-dev` overrides (env passed to `make`):

| Component | Env var (local-dev) |
|---|---|
| api | `CCF_API_IMAGE=ghcr.io/compliance-framework/api:<v>` |
| ui | `CCF_UI_IMAGE=ghcr.io/compliance-framework/ui:<v>` (confirmed in `compose/ui.yml`) |
| agent | `CCF_AGENT_VERSION=<v>` |
| plugins | none: local-dev pins plugins in `demo-agents/versions/*/config.yml`, so they stay at its pins (verified in AGT-S1-T06, see below) |

Components not in the train use their latest final release.

## Commands

```text
factory-agent stack up   --versions versions.json | --train <ref> | --release <repo>@<tag>   [--profile showcase|free]
factory-agent stack wait [--timeout 10m]      # polls API health + UI 200
factory-agent stack down                       # compose down -v (volumes removed)
factory-agent stack env                        # prints FACTORY_STACK_URL=http://localhost:8000 FACTORY_STACK_API=http://localhost:8080 …
```

- `up` runs `make demo-up-showcase` (profile `showcase`, default for GIFs: realistic data) or `make demo-up-free` with the env overrides, from `localDev`, with a **compose project name** `factory-<taskID>` to isolate volumes.
- A file lock (`~/.factory-agent/stack.lock`) enforces **one stack per machine**; the scheduler already avoids two `has-local-stack` leases, this is defence in depth.
- `down` is always called by the pipeline's cleanup, even on failure.
- **Skills get `env` and `wait` only.** The stack of a run is the worker's: the pipeline brings it up before the harness starts and down in cleanup. Inside a run (`FACTORY_HEADLESS=1` or `FACTORY_TASK_ID` set) `stack up` and `stack down` refuse with exit 2. `up` and `down` are the owner's, outside runs (manual verification, a stack left behind).

## Media constraints (from `docs` STYLE.md, enforced by skills)

GIF ≤ 4 MB, ≤ 20 s, width 1280 px; recordings use the seeded showcase data only (no real customer data); files under `static/img/<instance>/<version-line>/`.

## Tests

- `--dry-run` prints the exact `make` invocation and env for a sample train (golden).
- Probe tables with fake command runner.
- Lock contention test.

## Decisions taken in AGT-S1-T06

- **Package layout.** `internal/agent/stack`: versions (`StackVersions`, `TrainFacts`, `FromTrain`, `FromRelease`, `Resolve`), the `Manager` (lock, plan, up, wait, down), `ForPipeline` (the pipeline's `Stack`), and a read-only `GitHub` reader. The label checkers are `probe.LocalStack` and `probe.GIFTooling` (`probe.Labels`), wired into `factory-agent probe` and `worker`. The worker gives the pipeline a `ForPipeline` only when the config has `localDev` and the `has-local-stack` label. Every command goes through a `stack.Runner` (`probe.Commands` for the probes), which every test fakes.
- **How versions reach the task.** `inputs.train` is the train's tracking issue, `owner/repo#n` (`docs-on-release-train` passes `event.object.ref`). `inputs.verifyAgainst` is `{train: <same ref>}` or `{repo, tag}`. `train` wins when both are set. The stack reads the issue body anonymously from GitHub's REST API, since `workflows` is public and the pipeline's stack step runs before the task token is in any environment. It then parses the `<!-- train-state` JSON, the same block the GitHub provider's `ccf-train` extractor reads. That parser is duplicated (about 20 lines) because AGT can't import `internal/provider`. "Fetched via the task context" is not used: no context item carries the train, and the stack step comes before the prompt step.
- **`FromTrain`/`FromRelease` return `(StackVersions, error)`.** A version must be an image tag (`[0-9A-Za-z][0-9A-Za-z._-]*`, leading `v` stripped). It goes on make's command line, where make would expand a `$(…)`. A repo pins its `"."` package (or its only package). `api`, `ui` and `agent` pin their images. `plugin-*` repos (not `*-policies`) are recorded in `Plugins`. Others (`helm-charts`, `docs`) pin nothing. A **dry-run train pins nothing**: its versions are planned, not released, so no image exists for them. A dry run (the dev RuleSet's mock trains) brings up the latest releases.
- **Latest final release** = GitHub's `releases/latest` (newest non-draft, non-prerelease) of `compliance-framework/{api,ui,agent}`. local-dev's own defaults (`api:0.21.0`, `ui:2.12.1`, agent `0.9.0`) are pins, not "latest", so they are never relied on.
- **local-dev's variables (verified read-only in local-dev@main).** `CCF_API_IMAGE` (`compose/api.yml`), `CCF_UI_IMAGE` (`compose/ui.yml`) and `CCF_AGENT_VERSION` (the agent image tag in `demo-agents-*.yml`, without `v`) exist as the table says. **No per-plugin version variable exists.** The demo agents pin plugins in `demo-agents/versions/*/config.yml` (`source: ghcr.io/…/plugin-x:vN`). So `Plugins` is carried in `FACTORY_STACK_VERSIONS` and printed by `--dry-run` as not pinned, and the stack runs local-dev's plugin pins. Pinning them would need a local-dev change (environment interpolation in those configs). `CCF_SERVER_AGENT_VERSION` (the free profile's ubuntu server agent) is left alone.
- **Overrides go on make's command line** (`make demo-up-showcase VAR=value …`), and also into make's environment. local-dev's Makefile does `include .env` + `export`, and a makefile assignment beats the environment: an owner's `.env` that sets `CCF_API_IMAGE` would otherwise win. Command-line variables win, and make exports them to the recipes. Other overrides: `COMPOSE_COMMAND=docker compose`, because the Makefile defaults to `podman-compose`. `COMPOSE_PROJECT_NAME=factory-<task>`, which takes precedence over `compose/common.yml`'s `name: ccf`. For showcase only, `KIND_HOST_ALIAS=<project>-api-1`, because the kind cluster's agent reaches the API by container name, `ccf-api-1` under local-dev's project. make gets the owner's whole environment, because it is the owner's tool on the owner's machine. The harness still gets only the allow-list plus `FACTORY_STACK_*`.
- **Project name.** `factory-` plus the task id lower-cased, with everything outside `[a-z0-9-]` turned into `-` (`tsk_01JA.x` becomes `factory-tsk-01ja-x`). Underscores are allowed by compose, but are avoided because the API container's name is used as a DNS name. **Known gap in local-dev:** `scripts/lib/compose-scope.sh` hard-codes the project `ccf` when it detects running agents. Under `factory-<task>`, `register-todo-agent` registers the agent in the API but doesn't recreate the container with its credentials, so the demo agents run anonymously (remote config off). The seeded showcase data comes from API scripts and is unaffected. The fix belongs in local-dev (`CCF_COMPOSE_PROJECT="${COMPOSE_PROJECT_NAME:-ccf}"`).
- **One stack per machine.** The lock is `~/.factory-agent/stack.lock`, created with `O_EXCL` and holding the running stack's `State` (project, task, profile, localDev, versions, source, start). It is a file and not an `flock`, because the stack outlives the `stack up` process: `wait`, `env` and `down` run later, as other processes. An `up` for the same project replaces the state and runs make again (local-dev's targets are idempotent): the owner can switch a manual stack's versions. Another project gets a `BusyError` (CLI exit 3). An unreadable lock (mid-write, or damaged) counts as busy. A failed `up` takes its stack down and releases the lock, because the pipeline only calls `Down` after a successful `Up`. A failed `down` **keeps** the lock, because the stack may still be running and the next `has-local-stack` task must not start on top of it. The owner then runs `factory-agent stack down`. A stack the owner starts by hand under local-dev's own `ccf` project doesn't take the lock. Fixed host ports (8000, 8080, 5432) and container names (`postgres-db`) make a factory `up` fail on it rather than share it.
- **`down`** runs `docker compose --project-name factory-<task> down --volumes --remove-orphans --timeout 10` in localDev. It runs without `-f`, since Compose v2 finds the project's containers, networks and volumes by label. `--timeout 10` because local-dev's workers ask for a 40 s stop grace and the volumes are removed anyway. The showcase profile adds `make showcase-kind-destroy` (the `ccf-showcase` kind cluster `demo-up-showcase` creates). local-dev's `demo-destroy-*` is not used: it also deletes the checkout's generated agent env files and doesn't bound the stop time.
- **Cleanup timeout.** `cleanupTimeout` (30 s) is not enough. Compose stopping about ten containers, with removal of volumes and networks, takes 15–40 s, and deleting a 3-node kind cluster takes 10–30 s more. The pipeline now gives `Stack.Down` its own `pipeline.StackDownTimeout` of 3 min. The 30 s remains for the transcript upload, and `TestRunWithStack` checks the budget. A `Down` that still fails is logged, and the lock stays (above).
- **`ForPipeline.Up`** resolves the versions, runs `up` with the showcase profile, then `Wait`s (10 min). The harness therefore starts on a ready stack, and a stack that never gets ready is taken down. It returns `FACTORY_STACK_URL`, `FACTORY_STACK_API`, `FACTORY_STACK_PROJECT`, `FACTORY_STACK_VERSIONS` (JSON) and `FACTORY_STACK_DIR`. The last is the lock's directory: inside a run `HOME` is a scratch directory, so `factory-agent stack env|wait` find the worker's stack through it. make's output is captured (last 64 KiB). A failed `up` or `Wait` writes its last 400 lines to `<state dir>/logs/stack-<project>-<UTC timestamp>.log` (0600, the newest 20 kept), logs a WARN with only make's last line, redacted, because local-dev's output is the owner's, not the task's, and can echo credentials, and the error, and so the Result's summary, names that path on the agent and none of the output.
- **`stack wait`** polls `GET <api>/api/health` (the API's liveness route) and `GET <ui>/` every 2 s on the injected clock until both answer 200.
- **CLI.** The ops are `factory-agent stack up|wait|down|env`. `up` takes at most one of `--versions file`, `--train ref` or `--release repo@tag`. With none, every component runs its latest release. `--task` defaults to `manual`. `--local-dev` defaults to the running stack's checkout, else the config's `localDev`. `--dry-run` resolves the versions and prints the project, versions, directory, overrides, the exact `make` line and the down commands, and runs nothing (golden: `cmd/factory-agent/testdata/stack-up-dry-run.golden`, from a real train body). `up` and `env` print `KEY=VALUE` lines quoted for `eval`. The versions file is `{"api","ui","agent","plugins"}` JSON with unknown keys refused. The CLI reads GitHub with `$GH_TOKEN` when set, else anonymously.
- **Probes.** `has-local-stack` checks four things. First, `localDev` has `Makefile`, `compose/` and `.env`: local-dev's Makefile includes `.env` and fails without it, and the probe only stats it, never reads it. Second, `docker info --format '{{json .MemTotal}}'` succeeds and reports at least 8 GB. Docker reports total, not free, memory, and the threshold is decimal 8 GB so that a Docker Desktop VM given 8 GiB passes. Third, `docker compose version --short` is v2 or later. `gif-tooling` runs `npx --no playwright --version`, where `--no` means npx never downloads Playwright during a probe. It then runs `npx --no playwright install --dry-run chromium` and checks that every `Install location:` it lists exists, which is what "browsers installed" means here: only Chromium is needed. Last, `gifski --version`, `vhs --version` and `ffmpeg -version`.
- **Pipeline-managed only (ccf-review #47).** Only the worker brings a run's stack up and down; `stack up` and `stack down` refuse inside a run (exit 2), and `env` and `wait` stay available to skills. A skill can't run docker from inside Claude Code's OS sandbox anyway, and exempting docker (or make) from the sandbox would be a full escape: whoever runs docker owns the machine. Changing versions inside a run would need a worker-side op outside the sandbox (an Effector-like request to the worker), which isn't built.
- **The harness reaches the stack.** When a stack is up, the pipeline adds the hosts of `FACTORY_STACK_URL` and `FACTORY_STACK_API` (localhost) to `Workspace.Hosts`, so Claude Code's sandbox allow-list carries `WebFetch(domain:localhost)`. Whether a domain rule is enough for local ports is in the claudecode UNVERIFIED list (harness-adapters.md), to be confirmed by `make smoke-claude` with a stack up.

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
| ui | `CCF_UI_IMAGE=ghcr.io/compliance-framework/ui:<v>` (**verify** var name in `compose/ui.yml`; add it in local-dev if missing — follow-up PR there) |
| agent | `CCF_AGENT_VERSION=<v>` |
| plugins | per-plugin version vars used by the demo agents (**verify**; fallback: latest) |

Components not in the train use their latest final release.

## Commands (exposed to skills)

```text
factory-agent stack up   --versions versions.json | --train <ref> | --release <repo>@<tag>   [--profile showcase|free]
factory-agent stack wait [--timeout 10m]      # polls API health + UI 200
factory-agent stack down                       # compose down -v (volumes removed)
factory-agent stack env                        # prints FACTORY_STACK_URL=http://localhost:8000 FACTORY_STACK_API=http://localhost:8080 …
```

- `up` runs `make demo-up-showcase` (profile `showcase`, default for GIFs: realistic data) or `make demo-up-free` with the env overrides, from `localDev`, with a **compose project name** `factory-<taskID>` to isolate volumes.
- A file lock (`~/.factory-agent/stack.lock`) enforces **one stack per machine**; the scheduler already avoids two `has-local-stack` leases, this is defence in depth.
- `down` is always called by the pipeline's cleanup, even on failure.

## Media constraints (from `docs` STYLE.md, enforced by skills)

GIF ≤ 4 MB, ≤ 20 s, width 1280 px; recordings use the seeded showcase data only (no real customer data); files under `static/img/<instance>/<version-line>/`.

## Tests

- `--dry-run` prints the exact `make` invocation and env for a sample train (golden).
- Probe tables with fake command runner.
- Lock contention test.

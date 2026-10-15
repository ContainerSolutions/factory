# LLD — Repo layout, build and CI (FND)

> Workstream: [FND](../../definitions.md#fnd-foundation) · Tasks: FND-S0-T01, FND-S0-T02

## Module and toolchain

| Item | Choice | Why |
|---|---|---|
| Module | `github.com/compliance-framework/factory` | — |
| Go | 1.26 (same as `api`) | One toolchain across CCF |
| Node | 20 (`web/.nvmrc`), npm (same as `ui`) | Same as `ui` and its CI |
| Licence | AGPL-3.0, `LICENSE` at root | Decision Q14 |

## Tree

```text
factory/
├── cmd/
│   ├── factory/              # dispatcher: serve | validate | rules test | dispatch | version
│   ├── factory-agent/        # agent: worker | run | probe | fx | mcp | stack | version
│   └── factory-mock/         # contract mock server (dev only)
├── internal/
│   ├── apis/v1alpha1/        # Go types for every kind (FND)
│   ├── catalog/              # load + validate catalogs (CORE)
│   ├── cel/                  # CEL env + "=" expressions (CORE)
│   ├── patterns/             # harness config patterns (CORE)
│   ├── rules/                # rule engine, pure (CORE)
│   ├── store/                # Store interface + memory impl (RT)
│   ├── controllers/          # controller runtime + controllers (RT)
│   ├── scheduler/            # queues, pools, leases (RT)
│   ├── server/               # HTTP handlers implementing apiserver/gen (RT)
│   ├── auth/                 # agent tokens, UI sessions (RT)
│   ├── apiserver/gen/        # oapi-codegen server interface (generated, FND)
│   ├── apiclient/            # oapi-codegen client (generated, FND)
│   ├── provider/             # Adapter/Conn interfaces + fake (FND); github/ (GH); tokens/ (GH)
│   ├── agent/                # worker, pipeline, harness/{codex,claudecode,fake}, effector, mcp, stack (AGT)
│   └── webui/                # go:embed of the built UI (FND placeholder, WEB build output)
├── api/
│   ├── openapi.yaml          # THE HTTP contract (FND)
│   └── examples/             # example payloads used by factory-mock and MSW
├── schemas/                  # generated JSON Schemas for catalog kinds (FND)
├── catalog/                  # PoC catalog (CAT)
├── web/                      # Vue app (WEB) — own package.json, never imported by Go
├── deploy/poc/               # run kit (RUN)
├── design/                   # this design
├── Makefile
├── .golangci.yml
├── AGENTS.md  CLAUDE.md  README.md  LICENSE
└── .github/workflows/ci.yml, notify-failure.yml
```

**Ownership rule:** each workstream writes only under its directories (see *Owns* lines in [definitions](../../definitions.md)). Cross-cutting changes (contracts) go through FND-style small PRs first.

## Embedding the UI with `go:embed`

```go
// internal/webui/embed_placeholder.go
//go:build !webui
package webui
import "embed"
//go:embed placeholder
var assets embed.FS
const root = "placeholder"

// internal/webui/embed_dist.go
//go:build webui
package webui
import "embed"
//go:embed all:dist
var assets embed.FS
const root = "dist"

// internal/webui/handler.go
func Handler() http.Handler // serves assets under root; SPA fallback to index.html for non-/api paths;
                            // immutable cache headers for hashed assets, no-cache for index.html
```

- `internal/webui/placeholder/index.html` is committed ("UI not built — run `make web`").
- `internal/webui/dist/` is **gitignored**; produced by `make web` (`vite build --outDir ../internal/webui/dist --emptyOutDir`).
- Backend devs and backend CI never need Node; release builds use `make build-release` (= `make web` + `go build -tags webui`).

## Makefile targets

| Target | Does |
|---|---|
| `build` | `go build ./cmd/...` (placeholder UI) |
| `test` | `go test -race -count=1 ./...` |
| `lint` | `golangci-lint run` (same linters as `api`, `new-from-merge-base` in CI) |
| `generate` | `oapi-codegen` (server + client), `go generate ./...`, `make schemas` |
| `schemas` | generate `schemas/*.schema.json` from `internal/apis/v1alpha1` |
| `check-generated` | `make generate` then `git diff --exit-code` |
| `catalog` | `go run ./cmd/factory validate --dir catalog && go run ./cmd/factory rules test --catalog catalog --fixtures catalog/testdata` |
| `reviewable` | `check-generated lint test catalog` — what CI runs for Go |
| `web` | `cd web && npm ci && npm run build` |
| `build-release` | `web` + `go build -tags webui -trimpath -ldflags "-X …/version.Version=$(VERSION)" ./cmd/...` |
| `smoke-codex`, `smoke-claude` | manual harness smoke tests (AGT), skipped in CI |
| `poc-up` | RUN kit |

## CI (`.github/workflows/ci.yml`)

```yaml
name: ci
on:
  pull_request: { types: [opened, edited, synchronize, reopened, closed] }
  push: { branches: [main] }
permissions: { contents: read }
jobs:
  changes:      # dorny/paths-filter pinned by SHA → outputs go, web
  go:           # needs: changes; if go == 'true'; runs `make reviewable`
                # (or calls workflows' ci-go-service.yml pinned @sha # vX.Y.Z if it fits a mixed repo)
  web:          # needs: changes; if web == 'true' && hashFiles('web/package.json') != ''
                # working-directory: web; npm ci; npm run reviewable
  required:     # needs: [go, web]; if: always(); fails if any needed job failed (skipped = ok)
```

- Path filters: `go` ← `cmd/**`, `internal/**`, `api/**`, `schemas/**`, `catalog/**`, `go.*`, `Makefile`; `web` ← `web/**`, `api/openapi.yaml`, `api/examples/**`.
- `CORE-CI-001`: every action pinned by SHA, `persist-credentials: false`, top-level `permissions: contents: read`.
- `notify-failure.yml` caller like other repos.

## Coding conventions (AGENTS.md content)

- **Errors:** wrap with `%w`; typed errors for validation (`catalog.Error{Code, File, Line}`); never `panic` outside `main`/init.
- **Context:** every I/O function takes `ctx`; controllers honour cancellation.
- **Logging:** `log/slog`, JSON in serve mode; never log tokens, prompts with secrets, or untrusted bodies at info level.
- **Time:** inject a `clock.Clock` (tests use a fake clock); no `time.Now()` in logic packages.
- **Concurrency:** no shared mutable state without a lock; `-race` in CI.
- **Packages:** `internal/rules`, `internal/cel`, `internal/patterns`, `internal/catalog` are **pure** (no network, no store) — enforced by a `depguard` rule.
- **Tests:** table-driven; golden files under `testdata/` with `-update` flag; fakes live next to interfaces (`provider/fake`, `store/memory`, `agent/harness/fake`).
- **Generated code:** never edited by hand; CI checks freshness.
- **Commits/PRs:** conventional commits; one task per PR (see definitions).

## Dependencies (allow-list for the PoC)

| Purpose | Library |
|---|---|
| CEL | `github.com/google/cel-go` |
| YAML | `sigs.k8s.io/yaml` (strict via `yaml.UnmarshalStrict`) |
| JSON Schema gen / validate | `github.com/invopop/jsonschema` / `github.com/santhosh-tekuri/jsonschema/v6` |
| OpenAPI codegen | `github.com/oapi-codegen/oapi-codegen/v2` (strict server, std `net/http`) |
| GitHub REST/GraphQL | `github.com/google/go-github/v7x`, `github.com/shurcooL/githubv4` |
| App auth | `github.com/bradleyfalzon/ghinstallation/v2` |
| Globs | `github.com/gobwas/glob` |
| MCP | `github.com/modelcontextprotocol/go-sdk` |
| Semver | `golang.org/x/mod/semver` |

Anything else needs a line in the PR body justifying it.

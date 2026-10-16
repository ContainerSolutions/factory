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
│   ├── examples/             # example payloads used by factory-mock and MSW
│   ├── vacuum.yaml           # OpenAPI lint ruleset (make openapi-lint)
│   ├── componentsgen/        # writes the v1alpha1 components into openapi.yaml (make generate)
│   └── *.go                  # package api: embeds the spec and examples; spec_test.go checks them
├── schemas/                  # generated JSON Schemas for catalog kinds (FND)
├── catalog/                  # PoC catalog (CAT)
├── web/                      # Vue app (WEB) — own package.json, never imported by Go
├── deploy/poc/               # run kit (RUN)
├── design/                   # this design
├── Makefile                  # lint uses the shared CCF golangci-lint config (no .golangci.yml)
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
- `.gitignore` covers at least: `deploy/poc/.env`, `.env*` (but not `*.env.example`), `internal/webui/dist/`, `web/node_modules/`, `web/dist/`, `bin/`, `coverage*`.
- `internal/webui/dist/` is **gitignored**; produced by `make web` (`vite build --outDir ../internal/webui/dist --emptyOutDir`).
- Backend devs and backend CI never need Node; release builds use `make build-release` (= `make web` + `go build -tags webui`).

## Makefile targets

| Target | Does |
|---|---|
| `build` | `go build ./cmd/...` (placeholder UI; `factory`, `factory-agent`, `factory-mock`) |
| `test` | `go test -race -count=1 ./...` |
| `lint` | `golangci-lint run` with the shared CCF config at the workflows SHA `ci.yml` pins (as `api`; `new-from-merge-base` in CI) |
| `generate` | `make schemas`, `make openapi-components` (the v1alpha1 components in `api/openapi.yaml`), then `go generate ./...` (runs `go tool oapi-codegen` for the server and the client) |
| `schemas` | generate `schemas/*.schema.json` from `internal/apis/v1alpha1` |
| `check-generated` | `make generate` then `git diff --exit-code` |
| `catalog` | `go run ./cmd/factory validate --dir catalog && go run ./cmd/factory rules test --catalog catalog --fixtures catalog/testdata` |
| `openapi-lint` | vacuum (`go run …/vacuum@<pinned>`) on `api/openapi.yaml` with `api/vacuum.yaml`; warnings fail |
| `reviewable` | `check-generated lint test catalog openapi-lint` — what CI runs for Go |
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
  required:     # needs: [changes, go, web]; if: always(); fails only when a needed job's result is failure or cancelled (skipped = ok)
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
- **Packages:** `internal/rules`, `internal/cel`, `internal/patterns`, `internal/catalog` are **pure** (no network, no store); kept so in review.
- **Tests:** table-driven; golden files under `testdata/` with `-update` flag; fakes live next to interfaces (`provider/fake`, `store/memory`, `agent/harness/fake`).
- **Generated code:** never edited by hand; CI checks freshness.
- **Commits/PRs:** conventional commits; one task per PR (see definitions).

## Dependencies (allow-list for the PoC)

| Purpose | Library |
|---|---|
| CEL | `github.com/google/cel-go` |
| YAML | `go.yaml.in/yaml/v3` (YAML 1.2; strict decoding goes YAML → JSON → `encoding/json` with `DisallowUnknownFields`) |
| JSON Schema gen / validate | `github.com/invopop/jsonschema` / `github.com/santhosh-tekuri/jsonschema/v6` |
| HTTP server (real dispatcher) | `github.com/labstack/echo/v4` (v4.13.x, the same line as `api`); `factory-mock` uses plain `net/http` |
| OpenAPI codegen | `github.com/oapi-codegen/oapi-codegen/v2` **v2.7.x** (Echo strict server + client; a `go.mod` `tool`). Not v2.8+: its 3.1 support needs runtime ≥ v1.4 and so Echo ≥ v4.15.1, so the spec stays OpenAPI 3.0.3 (see [contracts.md §HTTP API](contracts.md#http-api)) |
| OpenAPI codegen runtime | `github.com/oapi-codegen/runtime` **v1.3.x**, required by the generated server and client; v1.3.1 is the newest that works with Echo v4.13 |
| OpenAPI parsing | `github.com/getkin/kin-openapi` (already pulled in by oapi-codegen), in package `api` only: spec parsing for the contract tests and `factory-mock`; not linked into `factory` or `factory-agent` |
| OpenAPI lint | `github.com/daveshanley/vacuum`, run with `go run …@<version>` from `make openapi-lint`; not in `go.mod` (its dependency tree would move shared dependencies) |
| GitHub REST/GraphQL | `github.com/google/go-github/v7x`, `github.com/shurcooL/githubv4` |
| App auth | `github.com/bradleyfalzon/ghinstallation/v2` |
| MCP | `github.com/modelcontextprotocol/go-sdk` |
| Semver | `golang.org/x/mod/semver` |

Anything else needs a line in the PR body justifying it.

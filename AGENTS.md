# AGENTS.md: compliance-framework/factory

Guidance for coding agents (and humans) working in this repo. The design lives in `design/`; read the LLD for your workstream in `design/workstreams/<name>/LLD/` before changing code.

## Commands

```sh
make reviewable                 # generated-code check + lint + tests + catalog checks: what CI runs for Go
make test                       # go test -race -count=1 ./...
make lint                       # golangci-lint v2 with the shared CCF config (workflows/.golangci.yml at the SHA ci.yml pins)
make generate                   # schemas + the spec's v1alpha1 components + go generate (oapi-codegen server and client)
make openapi-lint               # vacuum on api/openapi.yaml (ruleset api/vacuum.yaml)
go test ./internal/webui -run TestHandler   # one test
go run ./cmd/factory rules test --catalog catalog [--update | --base-catalog <dir>] [--coverage]   # replay catalog/testdata against the rules
make build-release              # web/ build + go build -tags webui
make e2e-run-container          # opt-in, needs docker: factory-agent run (linux) in a plain container as root, fake harnesses, no network
make kata-context KATA_CONTEXT_DIR=dir && docker build dir   # the kata image's build context (provision/kata + base.sh), what CI's kata job builds
cd web && npm ci && npm run gen:api   # after changing api/openapi.yaml or api/examples: regenerates web's committed client (CI's web job checks it)
bin/factory serve --dev-fake-providers --catalog-dir internal/catalog/testdata/poc   # local dispatcher, fake GitHub (needs FACTORY_OAUTH_CLIENT_ID/_SECRET, FACTORY_SESSION_KEY, FACTORY_CLI_TOKEN in env)
bin/factory dispatch --event event.json --server http://127.0.0.1:8800             # inject an event (FACTORY_CLI_TOKEN in env)
pbpaste | bin/factory-agent login claude; bin/factory-agent login codex   # the model logins a VM workspace's gateway adds (state dir, 0600); copy the token from `claude setup-token` first (Linux: run login claude, paste, Enter, Ctrl-D); `codex login` on this machine first
bin/factory-agent supervise [--keep-workspace] [--driver tart|kata]              # the agent: registers, leases tasks and runs each in its own VM (host check must pass; make tart-up or kata-up)
```

Backend work never needs Node: without `-tags webui` the binary embeds `internal/webui/placeholder/`.

## Layout and ownership

- `cmd/factory`, `cmd/factory-agent`: one file per subcommand; register it with one line in `main.go` (keeps merge conflicts to a single line).
- `internal/apis/v1alpha1`, `api/openapi.yaml`, `schemas/`: **contracts**. Change them in their own PR, merged before the code that uses them. A change to `api/openapi.yaml` or `api/examples/` also regenerates `web/`'s client in that PR (`npm run gen:api`).
- `internal/{rules,cel,patterns,catalog}`: **pure** packages. No network, store, provider or processes.
- `web/`: the Vue app, with its own toolchain. Never imported by Go; never changed in the same PR as Go code (except contract regeneration).
- `catalog/`: factory configuration. Changes need `factory rules test` fixtures.
- Each workstream writes only under the directories its *Owns* line lists in `design/workstreams/definitions.md`.

## Conventions

- **Errors:** wrap with `%w`; typed errors for validation; never `panic` outside `main`/`init`.
- **Context:** every I/O function takes a `context.Context`; honour cancellation.
- **Logging:** `log/slog`. Never log tokens, credentials, prompts or untrusted provider text at info level.
- **Time:** inject a clock in logic packages; no direct `time.Now()` there.
- **Concurrency:** no shared mutable state without a lock; tests run with `-race`.
- **Tests:** table-driven; golden files under `testdata/` with an `-update` flag; fakes live next to their interfaces.
- **Generated code:** never edit it by hand; CI fails when it's stale.
- **Dependencies:** only those in the repo-layout LLD's allow-list; anything else needs a justification in the PR body.

## Never

- Read, print or commit `deploy/poc/.env` or any other credential (App keys, tokens, `auth.json`). The `.gitignore` covers them; keep it that way.
- Write to GitHub outside the provider adapter / Effector paths.
- Commit `go.work`, `web/node_modules`, `internal/webui/dist` or other build outputs.

## PRs

Conventional-commit titles (`feat(rules): …`), branch `fx/<task-id>-<slug>`, one task per PR, the task ID and verification steps in the body.

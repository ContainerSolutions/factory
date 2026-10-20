# CCF Factory. `make reviewable` is what CI runs for Go.
VERSION ?= dev
LDFLAGS := -X github.com/compliance-framework/factory/internal/version.Version=$(VERSION)

# The repo has no golangci-lint config of its own: like CI, lint with the shared one at the
# compliance-framework/workflows commit that ci.yml pins.
WORKFLOWS_SHA := $(shell sed -n 's|.*compliance-framework/workflows/.github/workflows/ci-go-service.yml@\([0-9a-f]\{40\}\).*|\1|p' .github/workflows/ci.yml)
LINT_CONFIG := bin/golangci-$(WORKFLOWS_SHA).yml

# The OpenAPI linter, run with go run at a pinned version (no Node). Not a go.mod tool: its
# dependency tree is large and would move shared dependencies of the module.
VACUUM_VERSION := v0.30.6

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build bin/factory, bin/factory-agent and bin/factory-mock with the placeholder UI (no Node needed)
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

.PHONY: test
test: ## Run Go tests with the race detector
	go test -race -count=1 ./...

.PHONY: smoke-codex
smoke-codex: ## Maintainer machine only, never CI: a trivial task end to end through the real codex. FACTORY_SMOKE_CODEX_MODEL is required: a gpt-5* or gpt-6* model your codex sign-in offers (see /model in codex). Optional: FACTORY_SMOKE_CODEX_HOME, _EFFORT, _BIN
	FACTORY_SMOKE_CODEX=1 go test -count=1 -v -run '^TestSmokeCodex$$' ./internal/agent/harness/codex/

.PHONY: smoke-claude
smoke-claude: ## Maintainer machine only, never CI: a trivial task end to end through the real claude (FACTORY_SMOKE_CLAUDE_CONFIG_DIR, _MODEL, _EFFORT, _BIN)
	@if [ -n "$$CI" ]; then echo "smoke-claude runs a real claude on a maintainer machine, never in CI"; exit 1; fi
	FACTORY_SMOKE_CLAUDE=1 go test -count=1 -v -run '^TestSmokeClaude$$' ./internal/agent/harness/claudecode/

# POC_ADDR is the PoC dispatcher's public listener (what ngrok forwards to). Not 8080 or 8000:
# local-dev's CCF stack (the agent's has-local-stack runs) binds those.
POC_ADDR ?= 127.0.0.1:8800

.PHONY: poc-up
poc-up: ## Start the PoC dispatcher on POC_ADDR (default 127.0.0.1:8800): deploy/poc/run.sh (loads deploy/poc/.env, make build-release unless SKIP_BUILD=1, factory serve)
	FACTORY_ADDR=$(POC_ADDR) deploy/poc/run.sh

.PHONY: lint
lint: $(LINT_CONFIG) ## Run golangci-lint with the shared CCF config, as CI does
	golangci-lint run --config=$(LINT_CONFIG)

$(LINT_CONFIG):
	@mkdir -p bin
	curl -fsSL -o $@.tmp https://raw.githubusercontent.com/compliance-framework/workflows/$(WORKFLOWS_SHA)/.golangci.yml
	mv $@.tmp $@

.PHONY: generate
generate: schemas openapi-components ## Regenerate code: schemas, the spec's v1alpha1 components, then go generate ./... (oapi-codegen)
	go generate ./...

.PHONY: openapi-components
openapi-components: ## Write the v1alpha1 components into api/openapi.yaml (between its GENERATED markers)
	go run ./api/componentsgen -spec api/openapi.yaml

.PHONY: schemas
schemas: ## Generate schemas/*.schema.json from internal/apis/v1alpha1 (once the types exist)
	@if [ -d internal/apis/v1alpha1 ]; then go run ./internal/apis/v1alpha1/schemagen -out schemas; fi

.PHONY: check-generated
check-generated: generate ## Fail if generated files are stale
	@out=$$(git status --porcelain -- schemas api/openapi.yaml internal/apiserver/gen internal/apiclient); \
	if [ -n "$$out" ]; then echo "$$out"; echo "generated files are stale or uncommitted: run make generate and commit"; exit 1; fi

.PHONY: openapi-lint
openapi-lint: ## Lint api/openapi.yaml with vacuum (ruleset api/vacuum.yaml); infos fail too
	go run github.com/daveshanley/vacuum@$(VACUUM_VERSION) lint --no-update-check --ext-refs \
		--ruleset api/vacuum.yaml --fail-severity info --no-banner --no-style --details api/openapi.yaml

.PHONY: catalog
catalog: ## Validate the catalog, run its rule tests and fail on a selected rule no fixture fires (once catalog/ exists)
	@if [ -d catalog ]; then \
		go run ./cmd/factory validate --dir catalog && \
		go run ./cmd/factory rules test --catalog catalog --coverage; \
	fi

.PHONY: reviewable
reviewable: check-generated lint test catalog openapi-lint ## Everything CI checks for Go

.PHONY: web
web: ## Build the web UI into internal/webui/dist
	@if [ ! -f web/package.json ]; then echo "web/ isn't scaffolded yet (WEB-S0-T01)"; exit 1; fi
	cd web && npm ci && npm run build

.PHONY: build-release
build-release: web ## Build the binaries with the real UI embedded
	go build -tags webui -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

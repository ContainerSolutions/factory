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
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build bin/factory, bin/factory-agent and bin/factory-mock with the placeholder UI (no Node needed)
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

# HOST_INIT_FLAGS are extra flags for tart-up's and kata-up's host init, e.g. --dry-run or
# --only basevm,warmbase (kata: --only kataimage,katawarm).
HOST_INIT_FLAGS ?=

.PHONY: tart-up
tart-up: ## Set up this Mac as a tart agent host: build bin/factory-agent (darwin/arm64) and bin/factory-agent-linux-arm64, then run host init --driver tart (HOST_INIT_FLAGS=--dry-run to only print the plan)
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/factory-agent ./cmd/factory-agent
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/factory-agent-linux-arm64 ./cmd/factory-agent
	bin/factory-agent host init --driver tart --agent-linux bin/factory-agent-linux-arm64 $(HOST_INIT_FLAGS)

.PHONY: kata-up
kata-up: ## Set up this Linux machine as a kata agent host: build bin/factory-agent (linux/amd64, static: the kata helper and every workspace get a copy), then run host init --driver kata (HOST_INIT_FLAGS=--dry-run to only print the plan)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/factory-agent ./cmd/factory-agent
	bin/factory-agent host init --driver kata $(HOST_INIT_FLAGS)

# KATA_CONTEXT_DIR is where kata-context writes the kata image's build context.
KATA_CONTEXT_DIR ?= bin/kata-context

.PHONY: kata-context
kata-context: ## Write the kata image's build context (provision/kata and base.sh) to KATA_CONTEXT_DIR (default bin/kata-context) and print its sha256; then docker build KATA_CONTEXT_DIR
	go run ./internal/agent/host/provision/katacontext -out $(KATA_CONTEXT_DIR)

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

# The container layer of factory-agent run's tests (workspace-run.md §Tests, layer 5). The
# image stands in for the workspace base (ghcr.io/cirruslabs/ubuntu): Ubuntu 24.04 with bash,
# git and the CA roots, run as root, as linux/$(E2E_GOARCH) (emulated when the runtime can).
# It is pinned by its multi-arch index digest, as a tag can be repointed. To refresh it, read
# the tag's index digest from the registry without pulling (or `docker buildx imagetools inspect`):
#   tok=$(curl -fsS 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/buildpack-deps:pull' | jq -r .token)
#   url=https://registry-1.docker.io/v2/library/buildpack-deps/manifests/noble-scm
#   curl -fsSI -H "Authorization: Bearer $tok" -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' "$url" | grep -i docker-content-digest
E2E_CONTAINER_CLI ?= docker
E2E_CONTAINER_IMAGE ?= docker.io/library/buildpack-deps:noble-scm@sha256:59d4f4d8581d741bb2c31f4bfd00a557ca94d182d082c81bd56d32afbad8dfc9
E2E_GOARCH ?= $(shell go env GOARCH)
E2E_DIR := bin/e2e-container
E2E_LOG := $(E2E_DIR).log

# The test's name and its opt-in variables (FACTORY_E2E_CONTAINER, FACTORY_E2E_AGENT_BIN) are
# TestRunInContainer, envContainer and envContainerAgent in internal/agent/vmrun/container_test.go:
# change them together. A -test.run that matches nothing, or a skip, exits 0, so the target
# fails unless the test binary exited 0 and its output (E2E_LOG) has TestRunInContainer's
# top-level PASS and no SKIP. The exit status goes through a file: make's sh may lack pipefail.
.PHONY: e2e-run-container
e2e-run-container: ## Opt-in, needs docker (E2E_CONTAINER_CLI) and pulls E2E_CONTAINER_IMAGE: factory-agent run, the linux binary, in a plain container as root with fake harnesses and no network
	@mkdir -p $(E2E_DIR)
	GOOS=linux GOARCH=$(E2E_GOARCH) CGO_ENABLED=0 go build -o $(E2E_DIR)/factory-agent ./cmd/factory-agent
	GOOS=linux GOARCH=$(E2E_GOARCH) CGO_ENABLED=0 go test -c -o $(E2E_DIR)/vmrun.test ./internal/agent/vmrun
	@rm -f $(E2E_LOG) $(E2E_LOG).status
	{ $(E2E_CONTAINER_CLI) run --rm --platform linux/$(E2E_GOARCH) --network none --user 0:0 \
		--mount type=bind,src=$(CURDIR)/$(E2E_DIR),dst=/e2e,readonly \
		-e FACTORY_E2E_CONTAINER=1 -e FACTORY_E2E_AGENT_BIN=/e2e/factory-agent \
		$(E2E_CONTAINER_IMAGE) /e2e/vmrun.test -test.run '^TestRunInContainer$$' -test.count=1 -test.v 2>&1; \
		echo $$? > $(E2E_LOG).status; } | tee $(E2E_LOG)
	@status=$$(cat $(E2E_LOG).status 2>/dev/null); \
	case "$$status" in \
	0) ;; \
	''|*[!0-9]*) echo "e2e-run-container: no exit status for the run"; exit 1 ;; \
	*) echo "e2e-run-container: the run exited $$status"; exit "$$status" ;; \
	esac; \
	if grep -q -e '--- SKIP' $(E2E_LOG); then \
		echo "e2e-run-container: a test was skipped (see $(E2E_LOG)): is the opt-in variable still FACTORY_E2E_CONTAINER?"; exit 1; \
	fi; \
	if ! grep -q '^--- PASS: TestRunInContainer ' $(E2E_LOG); then \
		echo "e2e-run-container: TestRunInContainer didn't run and pass (see $(E2E_LOG)): does -test.run still match its name?"; exit 1; \
	fi

# POC_ADDR is the PoC dispatcher's public listener (what ngrok forwards to). Not 8080 or 8000:
# local-dev's CCF stack binds those.
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

# CCF Factory. `make reviewable` is what CI runs for Go.
VERSION ?= dev
LDFLAGS := -X github.com/compliance-framework/factory/internal/version.Version=$(VERSION)

# The repo has no golangci-lint config of its own: like CI, lint with the shared one at the
# compliance-framework/workflows commit that ci.yml pins.
WORKFLOWS_SHA := $(shell sed -n 's|.*compliance-framework/workflows/.github/workflows/ci-go-service.yml@\([0-9a-f]\{40\}\).*|\1|p' .github/workflows/ci.yml)
LINT_CONFIG := bin/golangci-$(WORKFLOWS_SHA).yml

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build bin/factory and bin/factory-agent with the placeholder UI (no Node needed)
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

.PHONY: test
test: ## Run Go tests with the race detector
	go test -race -count=1 ./...

.PHONY: lint
lint: $(LINT_CONFIG) ## Run golangci-lint with the shared CCF config, as CI does
	golangci-lint run --config=$(LINT_CONFIG)

$(LINT_CONFIG):
	@mkdir -p bin
	curl -fsSL -o $@.tmp https://raw.githubusercontent.com/compliance-framework/workflows/$(WORKFLOWS_SHA)/.golangci.yml
	mv $@.tmp $@

.PHONY: generate
generate: schemas ## Regenerate code: go generate ./... (incl. oapi-codegen directives once api/openapi.yaml exists) and schemas
	go generate ./...

.PHONY: schemas
schemas: ## Generate schemas/*.schema.json from internal/apis/v1alpha1 (once the types exist)
	@if [ -d internal/apis/v1alpha1 ]; then go run ./internal/apis/v1alpha1/schemagen -out schemas; fi

.PHONY: check-generated
check-generated: generate ## Fail if generated files are stale
	@out=$$(git status --porcelain -- schemas internal/apiserver/gen internal/apiclient); \
	if [ -n "$$out" ]; then echo "$$out"; echo "generated files are stale or uncommitted: run make generate and commit"; exit 1; fi

.PHONY: catalog
catalog: ## Validate the catalog and run its rule tests (once catalog/ exists)
	@if [ -d catalog ]; then \
		go run ./cmd/factory validate --dir catalog && \
		go run ./cmd/factory rules test --catalog catalog --fixtures catalog/testdata; \
	fi

.PHONY: reviewable
reviewable: check-generated lint test catalog ## Everything CI checks for Go

.PHONY: web
web: ## Build the web UI into internal/webui/dist
	@if [ ! -f web/package.json ]; then echo "web/ isn't scaffolded yet (WEB-S0-T01)"; exit 1; fi
	cd web && npm ci && npm run build

.PHONY: build-release
build-release: web ## Build the binaries with the real UI embedded
	go build -tags webui -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

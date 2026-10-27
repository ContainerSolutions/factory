# CCF Factory. `make reviewable` is what CI runs for Go.
VERSION ?= dev
LDFLAGS := -X github.com/ContainerSolutions/factory/internal/version.Version=$(VERSION)

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

# The object store's emulator tests (P3 §Tests, "Real buckets"): the bucket conformance, a race
# of conditional creates, the SDKs' retry of a create that landed, and storetest.Run through the
# object store, on gs:// against fake-gcs-server and on s3:// against MinIO, each in a container
# on a loopback port the runtime picks. The images are pinned by their multi-arch index digests
# (refresh them as for E2E_CONTAINER_IMAGE). minio/minio's images are no longer published:
# pgsty/minio is the community build of the same server. The MinIO user is a test value, not a
# credential. The opt-in variables are envObjectStore & co in
# internal/store/objectstore/emulator_test.go: change them together. As for e2e-run-container,
# the target fails unless TestEmulators ran and passed with nothing skipped.
E2E_GCS_IMAGE ?= docker.io/fsouza/fake-gcs-server:1.56.1@sha256:797ce226d62f947c009dc40246b30cfb456b8473d8241407f9d6f2c04e4d69ef
E2E_MINIO_IMAGE ?= docker.io/pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372
E2E_MINIO_USER := factorytest
E2E_MINIO_PASSWORD := factorytest-not-a-secret
E2E_OBJECTSTORE_LOG := bin/e2e-objectstore.log

.PHONY: e2e-objectstore
e2e-objectstore: ## Opt-in, needs docker (E2E_CONTAINER_CLI) and pulls E2E_GCS_IMAGE and E2E_MINIO_IMAGE: the gs:// and s3:// buckets and the object store against fake-gcs-server and MinIO
	@mkdir -p bin
	@rm -f $(E2E_OBJECTSTORE_LOG) $(E2E_OBJECTSTORE_LOG).status
	@cli='$(E2E_CONTAINER_CLI)'; gcs=factory-e2e-gcs-$$$$; s3=factory-e2e-minio-$$$$; \
	trap '$$cli rm -f $$gcs $$s3 >/dev/null 2>&1' EXIT; \
	$$cli run -d --rm --name $$gcs -p 127.0.0.1::4443 $(E2E_GCS_IMAGE) -scheme http -port 4443 >/dev/null || exit 1; \
	$$cli run -d --rm --name $$s3 -p 127.0.0.1::9000 -e MINIO_ROOT_USER=$(E2E_MINIO_USER) -e MINIO_ROOT_PASSWORD=$(E2E_MINIO_PASSWORD) \
		$(E2E_MINIO_IMAGE) server /data >/dev/null || exit 1; \
	gcs_addr=$$($$cli port $$gcs 4443/tcp | head -n1); s3_addr=$$($$cli port $$s3 9000/tcp | head -n1); \
	echo "fake-gcs-server on $$gcs_addr, MinIO on $$s3_addr"; \
	up=""; for i in $$(seq 1 60); do \
		if curl -fs -o /dev/null "http://$$gcs_addr/storage/v1/b?project=factory-e2e" && curl -fs -o /dev/null "http://$$s3_addr/minio/health/ready"; then up=1; break; fi; \
		sleep 1; \
	done; \
	if [ -z "$$up" ]; then echo "e2e-objectstore: the emulators didn't come up in 60s"; $$cli logs $$gcs; $$cli logs $$s3; exit 1; fi; \
	{ FACTORY_E2E_OBJECTSTORE=1 STORAGE_EMULATOR_HOST="$$gcs_addr" FACTORY_S3_ENDPOINT="http://$$s3_addr" \
		AWS_ACCESS_KEY_ID=$(E2E_MINIO_USER) AWS_SECRET_ACCESS_KEY=$(E2E_MINIO_PASSWORD) AWS_SESSION_TOKEN= AWS_PROFILE= AWS_REGION=us-east-1 \
		AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_EC2_METADATA_DISABLED=true AWS_ENDPOINT_URL= AWS_ENDPOINT_URL_S3= \
		go test -race -count=1 -run '^TestEmulators$$' -v ./internal/store/objectstore 2>&1; \
		echo $$? > $(E2E_OBJECTSTORE_LOG).status; } | tee $(E2E_OBJECTSTORE_LOG) | grep -E '^(=== RUN|--- |\s+--- (FAIL|SKIP)|ok|FAIL|PASS|panic)' | grep -v '^=== RUN.*/store/'; \
	status=$$(cat $(E2E_OBJECTSTORE_LOG).status 2>/dev/null); \
	case "$$status" in \
	0) ;; \
	''|*[!0-9]*) echo "e2e-objectstore: no exit status for the run"; exit 1 ;; \
	*) echo "e2e-objectstore: the run exited $$status (see $(E2E_OBJECTSTORE_LOG))"; exit "$$status" ;; \
	esac; \
	if grep -q -e '--- SKIP' $(E2E_OBJECTSTORE_LOG); then \
		echo "e2e-objectstore: a test was skipped (see $(E2E_OBJECTSTORE_LOG)): is the opt-in variable still FACTORY_E2E_OBJECTSTORE?"; exit 1; \
	fi; \
	if ! grep -q '^--- PASS: TestEmulators ' $(E2E_OBJECTSTORE_LOG); then \
		echo "e2e-objectstore: TestEmulators didn't run and pass (see $(E2E_OBJECTSTORE_LOG)): does -run still match its name?"; exit 1; \
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

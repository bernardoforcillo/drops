SHELL := /usr/bin/env bash -euo pipefail
GO    := go
PKG   := ./...

# Colour helpers — skip if the terminal doesn't support it.
RESET  := $(shell tput sgr0 2>/dev/null || true)
BOLD   := $(shell tput bold  2>/dev/null || true)
GREEN  := $(shell tput setaf 2 2>/dev/null || true)
YELLOW := $(shell tput setaf 3 2>/dev/null || true)

.DEFAULT_GOAL := help

# ── help ──────────────────────────────────────────────────────────────
.PHONY: help
help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*## "}; {printf "  $(BOLD)%-18s$(RESET) %s\n", $$1, $$2}'

# ── build ─────────────────────────────────────────────────────────────
.PHONY: build
build: ## Compile all packages
	$(GO) build $(PKG)

.PHONY: cli
cli: ## Build the drops binary (cmd/drops is its own module — see docs/cli.md)
	$(GO) build -C cmd/drops -o drops .

# ── test ──────────────────────────────────────────────────────────────
.PHONY: test
test: ## Run the full test suite
	$(GO) test $(PKG) -count=1

.PHONY: race
race: ## Run tests with the race detector
	$(GO) test $(PKG) -race -count=1

.PHONY: cover
cover: ## Generate an HTML coverage report (opens in browser)
	$(GO) test $(PKG) -count=1 -coverprofile=coverage.out
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "$(GREEN)coverage report → coverage.html$(RESET)"
	@open coverage.html 2>/dev/null || xdg-open coverage.html 2>/dev/null || true

.PHONY: test-cli
test-cli: ## Run the CLI module's own tests
	$(GO) test -C cmd/drops $(PKG) -count=1

.PHONY: test-short
test-short: ## Run only short tests (skips integration tests)
	$(GO) test $(PKG) -count=1 -short

# ── integration ───────────────────────────────────────────────────────
# integration/ and cmd/drops are separate modules: their drivers must
# not reach a user's build, so neither is covered by $(PKG) and each
# needs its own targets.

.PHONY: integration
integration: ## Run the integration suite (SQLite always; others need DSNs — see docs/testing.md)
	$(GO) test -C integration ./... -count=1

.PHONY: integration-up
integration-up: ## Start the servers the integration suite talks to
	docker compose -f integration/docker-compose.yml up -d --wait

.PHONY: integration-down
integration-down: ## Stop them and discard their data
	docker compose -f integration/docker-compose.yml down -v

.PHONY: servers-up
servers-up: ## Start Postgres and MySQL from distribution packages, no Docker
	./scripts/local-servers.sh

.PHONY: servers-down
servers-down: ## Stop the packaged servers
	./scripts/local-servers.sh stop

.PHONY: integration-all
integration-all: ## Run every backend against the compose servers
	DROPS_PG_DSN='postgres://drops:drops@localhost:5433/drops?sslmode=disable' \
	DROPS_MYSQL_DSN='drops:drops@tcp(localhost:3307)/drops?parseTime=true' \
	DROPS_CLICKHOUSE_DSN='clickhouse://localhost:9001/default' \
	DROPS_QDRANT_URL='http://localhost:6334' \
	DROPS_REQUIRE_ALL=1 \
	$(GO) test -C integration ./... -count=1 -v

# ── lint ──────────────────────────────────────────────────────────────
.PHONY: vet
vet: ## Run go vet in every module
	$(GO) vet $(PKG)
	$(GO) vet -C cmd/drops $(PKG)
	$(GO) vet -C integration $(PKG)

.PHONY: staticcheck
staticcheck: ## Run staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck $(PKG)
	cd cmd/drops && staticcheck $(PKG)
	cd integration && staticcheck $(PKG)

.PHONY: govulncheck
govulncheck: ## Check for known vulnerabilities (install: go install golang.org/x/vuln/cmd/govulncheck@latest)
	govulncheck $(PKG)
	cd cmd/drops && govulncheck $(PKG)
	cd integration && govulncheck $(PKG)

.PHONY: golangci-lint
golangci-lint: ## Run golangci-lint (install: brew install golangci-lint)
	golangci-lint run

.PHONY: query-lint
query-lint: cli ## Run "drops lint" over every module (the tree should be clean)
	./cmd/drops/drops lint ./...
	cd cmd/drops && ./drops lint ./...
	cd integration && ../cmd/drops/drops lint ./...

.PHONY: lint
lint: vet staticcheck golangci-lint query-lint ## Run all linters (vet + staticcheck + golangci-lint + drops lint)

# ── module hygiene ────────────────────────────────────────────────────
.PHONY: tidy
tidy: ## Run go mod tidy in all three modules
	$(GO) mod tidy
	$(GO) mod tidy -C cmd/drops
	$(GO) mod tidy -C integration

# The diff is whole-tree rather than naming go.mod and go.sum: the root
# module has no dependencies, so it has no go.sum, and naming a file
# that does not exist is itself an error.
.PHONY: tidy-check
tidy-check: ## Verify every module's go.mod / go.sum is up to date
	$(GO) mod tidy
	$(GO) mod tidy -C cmd/drops
	$(GO) mod tidy -C integration
	git diff --exit-code

# ── full CI equivalent ────────────────────────────────────────────────
.PHONY: check
check: tidy-check build cli test-cli vet test race integration staticcheck govulncheck ## Run everything CI runs (requires tools)
	@echo "$(GREEN)$(BOLD)All checks passed.$(RESET)"

# ── examples ──────────────────────────────────────────────────────────
.PHONY: examples
examples: ## Build (but don't run) all examples under examples/
	@for dir in examples/*/; do \
	  echo "$(YELLOW)building $$dir$(RESET)"; \
	  $(GO) build ./$$dir; \
	done

# ── clean ─────────────────────────────────────────────────────────────
.PHONY: clean
clean: ## Remove generated artefacts (coverage files, example binaries)
	rm -f coverage.out coverage.html cmd/drops/drops
	find examples _examples -maxdepth 2 -type f -name main -delete 2>/dev/null || true

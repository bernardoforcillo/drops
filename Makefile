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

.PHONY: test-short
test-short: ## Run only short tests (skips integration tests)
	$(GO) test $(PKG) -count=1 -short

# ── integration ───────────────────────────────────────────────────────
# integration/ is a separate module: its drivers must not reach a user's
# build, so it is not covered by $(PKG) and needs its own targets.

.PHONY: integration
integration: ## Run the integration suite (SQLite always; others need DSNs — see docs/testing.md)
	$(GO) test -C integration ./... -count=1

.PHONY: integration-up
integration-up: ## Start the servers the integration suite talks to
	docker compose -f integration/docker-compose.yml up -d --wait

.PHONY: integration-down
integration-down: ## Stop them and discard their data
	docker compose -f integration/docker-compose.yml down -v

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
vet: ## Run go vet
	$(GO) vet $(PKG)

.PHONY: staticcheck
staticcheck: ## Run staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck $(PKG)

.PHONY: govulncheck
govulncheck: ## Check for known vulnerabilities (install: go install golang.org/x/vuln/cmd/govulncheck@latest)
	govulncheck $(PKG)

.PHONY: golangci-lint
golangci-lint: ## Run golangci-lint (install: brew install golangci-lint)
	golangci-lint run

.PHONY: lint
lint: vet staticcheck golangci-lint ## Run all linters (vet + staticcheck + golangci-lint)

# ── module hygiene ────────────────────────────────────────────────────
.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

.PHONY: tidy-check
tidy-check: ## Verify go.mod / go.sum are up to date (fails if tidy would change anything)
	$(GO) mod tidy
	git diff --exit-code go.mod go.sum

# ── full CI equivalent ────────────────────────────────────────────────
.PHONY: check
check: tidy-check build vet test race integration staticcheck govulncheck ## Run everything CI runs (requires tools)
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
	rm -f coverage.out coverage.html
	find examples _examples -maxdepth 2 -type f -name main -delete 2>/dev/null || true

.PHONY: build build-go dev setup serve test lint lint-complexity complexity-ratchet complexity-baseline deadcode-ratchet deadcode-baseline lint-all clean release sync-contracts check-contracts check-cross-repo-contracts check-selfhost-backup check-selfhost-restore-live

# Defaults
SITE ?= airtime.local
CONFIG ?= config/v0/airtime/
PORT ?= 8000
DB_USER ?= root
DB_PASS ?=
ADMIN_EMAIL ?= admin@airtime.local
ADMIN_PASS ?=
TAG ?= v0.1.0

## Build
build: build-go                    ## Build the Go binary (the UI is the separate kora-ui repo)

VERSION ?= $(shell cat VERSION 2>/dev/null || echo "dev")
LDFLAGS += -X github.com/asenawritescode/kora/internal/cli.Version=$(VERSION)

build-go:                          ## Build Go binary
	go build -ldflags="-s -w $(LDFLAGS)" -o kora ./cmd/kora

## Development
dev:                               ## Full dev setup: MySQL + build + setup + serve
	@echo "Starting MySQL..."
	docker compose up -d mysql
	@sleep 3
	@echo "Building..."
	$(MAKE) build
	@echo "Setting up site $(SITE)..."
	./kora setup --site $(SITE) --path $(CONFIG) --db-user $(DB_USER) --db-pass $(DB_PASS) --admin-email $(ADMIN_EMAIL) --admin-password $(ADMIN_PASS)
	@echo "Starting server on :$(PORT)..."
	./kora serve --port $(PORT)

setup: build                       ## Build + setup a site (SITE=airtime.local CONFIG=config/v0/airtime/)
	./kora setup --site $(SITE) --path $(CONFIG) --db-user $(DB_USER) --db-pass $(DB_PASS) --admin-email $(ADMIN_EMAIL) --admin-password $(ADMIN_PASS)

serve: build                       ## Build + start the server
	./kora serve --port $(PORT)

restart:                           ## Rebuild Go, kill old server, start fresh
	@fuser -k $(PORT)/tcp 2>/dev/null || true
	$(MAKE) build
	./kora serve --port $(PORT)

## Quality
check-contracts:                      ## Verify generated schemas, fixture copies, and runtime validators
	go run ./contract/gen -check
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest scripts/test_sync_contract_consumers.py
	python3 scripts/check-contract-fixture-manifest.py --local-only

sync-contracts:                         ## Regenerate and copy Engine contracts to first-party consumers
	go run ./contract/gen -write
	python3 scripts/sync-contract-consumers.py

check-cross-repo-contracts:            ## Verify consumer contracts, route inventory, and complete Go import graph
	python3 scripts/sync-contract-consumers.py --check
	python3 scripts/check-contract-fixture-manifest.py
	go run ./cmd/route-inventory -check
	python3 scripts/architecture-import-graph.py --check
	python3 scripts/check-cloud-engine-boundary.py

check-selfhost-backup:                  ## Test self-host backup and restore helpers
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest scripts/test_selfhost_source_manifest.py scripts/test_selfhost_restore_drill.py
	PYTHONPYCACHEPREFIX=/dev/shm/kora-pycache python3 -m py_compile scripts/selfhost-source-manifest.py scripts/test_selfhost_source_manifest.py scripts/selfhost-restore-drill.py scripts/test_selfhost_restore_drill.py

check-selfhost-restore-live:            ## Run the opt-in disposable MySQL backup/restore drill
	python3 scripts/selfhost-restore-drill.py --run

test:                              ## Run Go and SDK tests
	go test ./... -v -count=1
	cd sdk && go test ./... -count=1

lint:                              ## Static analysis (govet+shadow, errcheck, staticcheck, gosec, errorlint, revive, ...)
	golangci-lint run

lint-complexity:                   ## Report every complexity/duplication hotspot (informational)
	golangci-lint run --config .golangci-complexity.yml

complexity-ratchet:                ## Fail if a function became newly over the limits or got worse
	python3 scripts/complexity-ratchet.py

complexity-baseline:               ## Lower the complexity baseline after refactoring (never raises it)
	python3 scripts/complexity-ratchet.py --update

deadcode-ratchet:                  ## Fail if a function became unreachable from the binary
	python3 scripts/deadcode-ratchet.py

deadcode-baseline:                 ## Shrink the dead-code baseline after deleting code (never adds)
	python3 scripts/deadcode-ratchet.py --update

lint-all: lint complexity-ratchet deadcode-ratchet  ## Everything CI's Lint job runs

fmt:                               ## Format code
	go fmt ./...

## Release
release:                           ## Full release: test → lint → changelog → bump → tag → push (TAG=v0.2.0)
	@test -n "$(TAG)" || (echo "Usage: make release TAG=v0.2.0" && exit 1)
	./scripts/release.sh $(TAG)

## Cleanup
clean:                             ## Remove build artifacts
	rm -rf kora

## Help
help:                              ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help

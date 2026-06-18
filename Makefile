.PHONY: build build-ui build-go dev setup serve test lint clean release

# Defaults
SITE ?= airtime.local
CONFIG ?= config/airtime/
PORT ?= 8000
DB_USER ?= root
DB_PASS ?=
ADMIN_EMAIL ?= admin@airtime.local
ADMIN_PASS ?=
TAG ?= v0.1.0

## Build
build: build-ui build-go          ## Build UI + Go binary

build-ui:                          ## Build React SPA
	cd ui && bun install --frozen-lockfile
	cd ui && bun run build
	rm -rf workspace/dist
	cp -r ui/dist workspace/dist

VERSION ?= $(shell cat VERSION 2>/dev/null || echo "dev")
LDFLAGS += -X github.com/asenawritescode/kora/cli.Version=$(VERSION)

build-go:                          ## Build Go binary
	go build -ldflags="-s -w $(LDFLAGS)" -o kora .

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

setup: build                       ## Build + setup a site (SITE=airtime.local CONFIG=config/airtime/)
	./kora setup --site $(SITE) --path $(CONFIG) --db-user $(DB_USER) --db-pass $(DB_PASS) --admin-email $(ADMIN_EMAIL) --admin-password $(ADMIN_PASS)

serve: build                       ## Build + start the server
	./kora serve --port $(PORT)

restart:                           ## Rebuild UI + Go, kill old server, start fresh
	@fuser -k $(PORT)/tcp 2>/dev/null || true
	$(MAKE) build
	./kora serve --port $(PORT)

## Quality
test:                              ## Run Go tests
	go test ./... -v -count=1

lint:                              ## Run linters
	golangci-lint run --timeout=3m
	cd ui && bunx tsc -b --noEmit

fmt:                               ## Format code
	go fmt ./...
	cd ui && bunx prettier --write 'src/**/*.{ts,tsx}'

## Release
release:                           ## Full release: test → lint → changelog → bump → tag → push (TAG=v0.2.0)
	@test -n "$(TAG)" || (echo "Usage: make release TAG=v0.2.0" && exit 1)
	./scripts/release.sh $(TAG)

## Cleanup
clean:                             ## Remove build artifacts
	rm -rf kora workspace/dist ui/dist ui/node_modules

## Help
help:                              ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help

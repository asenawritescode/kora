# Contributing to Kigae

Kigae is a config-driven application engine — define your data model in YAML and get a database and a REST API. The admin UI (Studio) lives in a separate repository. We welcome contributions of all kinds.

## What You Need Installed

| Dependency | Version | Why | Install |
|-----------|---------|-----|---------|
| **Go** | 1.25+ | Engine server, CLI, ORM, schema migration | [go.dev/dl](https://go.dev/dl/) |
| **Docker** | 27+ | Runs MySQL for development | [docker.com](https://docker.com) |
| **MySQL** | 8.0 | Database engine (runs in Docker for dev) | Comes with Docker |

That's it for this repository. Docker gives you MySQL without installing it locally. The UI is a separate repository and has its own toolchain (see below).

### Verify Your Setup

```bash
go version      # go1.25.0 or later
docker ps       # Docker running
```

## How Everything Works Together

### The Engine and Its Clients

This repository is the **Engine**: a single, API-only Go binary. It serves no UI files. The user interface (Studio), the cloud control plane, the mobile app and the website are separate repositories that talk to the Engine over HTTP.

```
┌──────────────────────────────────────────────┐
│                 kora binary                   │
│  REST API · Auth · Kernel · Workflow · AI/MCP │
│  ORM · Schema migration · Scheduler · CLI     │
│                  (port :8000)                 │
└───────────────────────┬──────────────────────┘
                        │
              ┌─────────┴─────────┐
              │  MySQL / LibSQL   │
              └───────────────────┘

 Clients (separate repositories, same /api contract):
   kora-ui (Studio)  ·  kora-cloud  ·  kora-mobile-app  ·  kora-website  ·  kora-ts-extension-sdk
```

| Repository | What it is |
|---|---|
| `kora` (this repo) | Engine: Go module and nested `sdk/` module, `config/` packs, `contract/` wire-contract source of truth |
| `kora-ui` | Studio, the React workspace and admin UI (Bun + Vite). Proxies `/api` to the Engine |
| `kora-cloud` | Control plane: marketplace, signup, provisioning, customer portal |
| `kora-mobile-app` | Android client (Kotlin, Compose), offline-first |
| `kora-website` | Public marketing site |
| `kora-ts-extension-sdk` | TypeScript SDK and extension tooling |

The wire contract is generated here with `go run ./cmd/contractgen -write` and copied to the clients with `scripts/sync-contract-consumers.py` (set `KORA_PROJECTS_ROOT` to a scratch directory unless you intend to update your real sibling clones).

### Request Flow

```
Client → :8000 → Gin router
  ├── /api/*           → SiteGuard (auth) → Permission check → Kernel/ORM → database
  ├── /api/auth/*      → Public (login, logout)
  ├── /console/*       → SystemGuard (platform console API)
  └── /s/:site/api/*   → NoRoute handler picks the site, then re-dispatches to /api/*
```

### Key Concept: Config-Driven

Kigae has **no per-entity code**. There's no `invoice.go`. Everything is generic:

1. You define a DocType in YAML (or through Studio's admin screens)
2. Kigae creates the table and builds the REST API
3. Clients render list, create and edit screens from the DocType field definitions the API returns

```
YAML / Studio  →  _kora_doctype table  →  Registry (in-memory)
                                         →  Schema migration (CREATE TABLE)
                                         →  API endpoints (generic CRUD)
                                         →  Schema served to clients
```

## Development Workflow

### Quick Start (One Command)

```bash
make dev DB_PASS=kora123 ADMIN_EMAIL=admin@airtime.local ADMIN_PASS=kora123
```

This starts MySQL, builds the Go binary, sets up the `airtime.local` site with sample config, and starts the server on `:8000`.

### The Dev Loop

```bash
go build -o kora .        # Rebuild the Go binary
./kora serve --port 8000  # Restart the server
make restart              # Same, and kills the old server first
make test                 # Go and SDK tests
```

To see changes in a browser, run Studio from the kora-ui repository against this Engine (`bun install && bun run dev`, which proxies `/api` to `:8000`).

### Project Structure

```
kora/
├── api/            # REST handlers: CRUD, system endpoints, AI chat, OpenAPI
├── auth/           # Session auth, CSRF tokens, SiteGuard, SystemGuard
├── cli/            # Cobra CLI commands (serve, setup, migrate, config, mcp, secret)
├── cmd/            # Developer tools (contractgen, routeinventory)
├── config/         # Config packs (YAML DocTypes, views, workflows) for POS, ERP, CRM...
├── configstore/    # Persist config to _kora_* tables, versioning, roles/permissions
├── contract/       # Wire-contract source of truth; generated/ and schema/ are build outputs
├── contract-fixtures/ # Shared JSON fixtures decoded by every client
├── db/             # SQL dialect abstraction (MySQL, LibSQL, PostgreSQL)
├── doctype/        # Core types: DocType, Field, Registry, permissions, expression engine
├── internal/       # Engine composition (internal/engine) and application services
├── kernel/         # Command pipeline: authorization, validation, idempotency, audit, outbox
├── mcp/            # Model Context Protocol server
├── net/            # SiteRouter (multi-site, path-based API routing), TLS, CORS, rate limits
├── orm/            # Generic CRUD on map[string]any documents
├── schema/         # Registry vs live schema diff → DDL, migration safety tiers
├── scheduler/      # Cron-style background jobs
├── script/         # Sandboxed JS runtime for extensions
├── sdk/            # Go SDK (separate Go module)
├── site/           # Site registry, provisioning, bootstrap, config import
├── workflow/       # Workflow runtime
├── docs/           # Architecture and operations documentation
├── scripts/        # Release, contract-sync and verification scripts
└── skills/         # Claude Code skill definitions
```

(There are more packages than listed, for example `analytics/`, `storage/`, `webhook/`, `outbox/`, `payment/`, `query/`, `print/`. See `docs/architecture/` for the full inventory.)

### Where to Make Changes

| You want to... | Change this |
|---------------|-------------|
| Add a new API endpoint | `api/system.go` — add handler, register in `RegisterSystemRoutes` (in `api/router.go`) |
| Add a new field type | `doctype/parser.go` — `validateFieldType`; `db/dialect_*.go` — `ColumnType`; `api/schema.go` — `FieldToJSONSchema`; then the renderer in kora-ui |
| Add a new field property | `doctype/doctype.go` — add to `Field` struct (yaml/json tags make it a known key automatically); `configstore/store.go` — add to `_kora_field` INSERT/SELECT + `db/dialect_*.go` ALTER column; regenerate the contract if it is part of the wire types |
| Change how data is stored | `configstore/store.go` — DB schema and queries; `doctype/` — struct definitions |
| Add a new constraint type | `doctype/validation.go` — add validation logic; then the constraint editor in kora-ui |
| Change the wire contract | `contract/*.go`, then `go run ./cmd/contractgen -write` and `scripts/sync-contract-consumers.py` |
| Change anything visual | The kora-ui repository (Studio), not this one |

## Testing

```bash
go test ./...                       # All Go tests
make test                           # Go tests plus the nested sdk/ module
make check-contracts                # Generated contract and fixture checks
```

Set `KORA_UI_ROOT` to a kora-ui checkout to also run the Engine/Studio parity tests in `doctype/reference_validation_test.go`; they are skipped otherwise.

## Pull Requests

1. Keep PRs focused — one feature or fix per PR
2. Update docs if your change adds or changes behavior
3. Run `go build ./... && go vet ./... && go test ./...` before submitting
4. Describe what you changed and why in the PR description
5. All commits to `main` go through PRs — never push directly

## Common Issues

**"Something went wrong" on save** — Check the response in the client's Network tab. Usually a validation error from the Engine with a useful message. Also check Go server logs.

**Port 8000 already in use** — Run `fuser -k 8000/tcp` or use `make restart` which handles this automatically.

**MySQL connection refused** — Run `docker compose up -d mysql` and wait ~5s for it to be ready. Password is `kora123` for root.

**No UI at `/workspace`** — Expected: the Engine no longer serves a UI. Run Studio from the kora-ui repository.

## Go Code Review Checklist

Use this checklist when reviewing Go code in pull requests.

### Interfaces

- [ ] Prefer 1-3 method interfaces (small, focused)
- [ ] Define interfaces at the consumption point, not the production point
- [ ] No fat interfaces — split into separate interfaces when a type has unrelated methods

### Error Handling

- [ ] Wrap errors with `%w` for unwrappable error chains
- [ ] Use sentinel errors (`var ErrNotFound = errors.New("...")`) for known error conditions
- [ ] No panic-driven control flow — use errors for expected failures

### Context & Concurrency

- [ ] `context.Context` is the first parameter in function signatures that need cancellation or deadlines
- [ ] Goroutines have a clearly defined lifecycle (known start + known stop)
- [ ] Threaded I/O uses appropriate synchronization (mutex, channel, or atomic)

### Testing

- [ ] Tests are table-driven where multiple cases share the same test logic
- [ ] Coverage is meaningful — test the error paths, not just the happy path
- [ ] `go test ./...` passes before requesting review

### Architecture

- [ ] No raw SQL in HTTP handlers — all DB access goes through the ORM or configstore
- [ ] Packages are organized by domain capability, not by layer
- [ ] No packages named `models`, `utils`, `helpers`, or `common`

## License

Kigae is licensed under the GNU Affero General Public License v3.0. By contributing, you agree that your contributions will be licensed under the same terms.

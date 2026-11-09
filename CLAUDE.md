# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Agent Rules (read first)

Standing rules for every agent ticket (source: the M1 tracker, #123). They override convenience.

**Scope**
- One ticket = one PR, branched from `main` only after its "Depends on" items are merged. No stacked PRs.
- ≤300 changed lines including tests. Overages are allowed only for tests and must be noted in the PR. Tests prove every "Done when" point.
- Stay inside the ticket. Out-of-scope bugs or ideas become a new issue (labels `m1-next,blocked`, plus a "Depends on" list), never a side fix.
- Spec gap or unclear ticket: comment the question on the ticket, label it `blocked`, stop. Never invent design.
- No hard-coding: verticals (POS, inventory) are config packages on generic core features. No vertical names in Engine or client code.

**Code and verification**
- All SQL goes through the dialect (`internal/db/`). Changes must pass Integration (MySQL) and Integration (Postgres).
- Integration CI is temporarily disabled (`if: ${{ false }}` in `.github/workflows/integration.yml`). Until it is re-enabled, verify every DB-touching change locally against disposable `mysql:8.0` and `postgres:16` containers (DSN env vars as in the workflow), running the affected integration tests or `scripts/ci-integration.sh mysql|postgres`. State the result in the PR.
- Do not trust unit tests with fake tables for SQL changes. They once hid a real table-prefix bug; run against a real database.
- This repository contains no UI code. UI changes belong in the standalone UI repository (kora-ui, local clone `~/projects/kora-studio-mvp`); embed screenshots in those PRs via its `pr-assets` branch.

**Git and CI**
- Work in a worktree off a fresh `git fetch`; never edit the main checkout. The user's own clones (`~/projects/kora`, `~/projects/kora-studio-mvp`, etc.) hold uncommitted work: use worktrees only.
- Push only to the `kora-upstream` remote. In the local clone, `origin` points at a different repo (kora-ui); never push there.
- Never force-push. Never delete branches you did not create.
- No attribution: never add `Co-authored-by` trailers, "Generated with" lines or any tool/agent credit to commits, PR or issue text, code comments or docs. Commits carry the repository owner's own identity only.
- Never merge on a red or pending Go check, even when the failure looks unrelated.
- kora-ui merges also require the `contracts` check (lint ratchet: no new `any`, `react-refresh/only-export-components`, set-state-in-effect).
- Never run the contract sync script with default paths; it writes into the user's real sibling clones. Point `KORA_PROJECTS_ROOT` at a scratch directory. After 3 failed fix rounds, comment the blocker on the ticket, label it `blocked`, close the PR.
- PR title `<ticket-id>: <title>`, body ≤10 lines with `Closes #N`.

**Shared machine state**
- Do not run `go clean`, delete module/build caches, prune Docker, or otherwise remove shared state other agents depend on. Use disposable, uniquely named containers and remove only what you created.

**Tickets** use `.github/ISSUE_TEMPLATE/agent-task.md` (Goal, Done when, Depends on, Rules).

## Build & Run

```bash
make dev                # MySQL + build + setup + serve (one command)
make build              # Build the Go binary
make serve              # Start server on :8000
make restart            # Kill old server + rebuild all + start fresh
make setup              # Setup a site (SITE=airtime.local CONFIG=config/v0/airtime/)
make test               # Run Go tests (18 packages)
make lint               # Run linters (golangci-lint)
make fmt                # Format code
make release TAG=v0.2.0 # Tag and push a release
make help               # Show all commands
```

### Manual Commands

```bash
# Backend (Go)
go build -o kora ./cmd/kora                  # Build binary
go run ./cmd/kora serve --port 8000          # Dev run
go run ./cmd/kora migrate --all              # Apply all pending migrations
go run ./cmd/kora config import --site X --path Y  # Re-import YAML config to DB

# AI & Secrets (also configurable in Studio: Settings → Secrets)
./kora secret set --site X --key deepseek_api_key --value sk-...   # Set AI provider key (CLI)
./kora mcp --site X                           # Start MCP server (stdio) for Claude Desktop

# Frontend: not in this repository. Studio (the React UI) lives in kora-ui; see "Repository Layout".
# To run it against a local Engine: cd ~/projects/kora-studio-mvp && bun install && bun run dev  (proxies /api → :8000)

# Docker
docker compose up -d mysql                   # MySQL 8.0 (root:kora123)
```

## Architecture

For restructuring work, follow [the target-state specification](docs/architecture/target-state-spec.md). Its G0–G10 groups define migration order and contract compatibility. The descriptions below refer to the current implementation; do not treat existing startup ownership as the target.

Kigae is currently a **DocType-centric config-driven application engine**. The codebase has kernel, contract, workflow, AI/MCP, and reconciliation capabilities, while the generic resource/runtime architecture is still evolving.

### Startup Flow (`internal/cli/serve.go`)

1. Load config from environment variables (`CommonConfigFromEnv` — no YAML files)
2. Connect to platform DB (MySQL or remote LibSQL via `DB_DSN`)
3. Discover sites from the canonical `_kora_site_registry` only; missing registry is a startup error
4. Per site: reconstruct config from persisted site registry data + platform defaults → connect → bootstrap `_kora_*` tables → load config from DB → build Registry → run schema migration
5. Build `SiteRouter` (domain → site map)
6. Wire middleware: Recovery → RequestID → SecurityHeaders → CORS → SiteRouter → RateLimiter
7. Register auth routes (public), API routes (/api — SiteGuard), path-based tenant routes (/s/:site/api — NoRoute re-dispatch), console API (/console — SystemGuard). The Engine serves no UI files.
8. Start scheduler, listen, graceful shutdown on SIGTERM

**Site config is reconstructed from env vars + the durable site registry** — no `site_config.yaml` files. Sites survive container redeploys because metadata persists in `_kora_site_registry`, while tenant config history remains in `_kora_config_version`. Application behavior itself is still DocType/config-pack driven, not a fully generic resource model, and Cloud does not own the tenant business truth stored there.

### Middleware Chain

```
Request  → Recovery → RequestID → SecurityHeaders → CORS → SiteRouter → RateLimiter
         → API routes: SiteGuard (Auth + CSRF) → Permission check → Validation → ORM
         → /s/:site/api/...: NoRoute handler injects the site and re-dispatches to the API routes
         → /console: SystemGuard (system_credentials.yaml, separate from site auth)
         → /api/auth: public (no guard)
```

### Multi-Site Routing

Three methods coexist:
- **Host-based**: `Host` header → site (production, needs DNS)
- **Path-based**: `/s/:site/api/...` → site (dev, no config needed; the standalone UI uses this form)
- **Default**: localhost/IP → first loaded site

The `SiteRouter` middleware sets `site_name`, `site_db`, `site_registry` in Gin context. **All auth is site-scoped** — login, session creation, session validation, and logout all read `site_db` from context. A session from one site doesn't work on another.

Path-based routing (`net.RegisterPathSiteRoutes`) resolves the site from the `/s/:site` prefix, adds a `kora_site` cookie to the re-dispatched request, and serves nothing but API routes. Path-based routing skips host validation entirely — the site is identified by the URL path prefix, not the Host header.

### API Envelope

All responses: `{"data": ..., "meta": {"doctype": "...", "total": N, "config_version": N}}`  
Errors: `{"error": "plain message"}` or `{"error": {"type": "UniqueConstraint", "message": "...", "field": "fieldname"}}`  
Multiple: `{"error": {"errors": [{"type": "...", "message": "...", "field": "..."}]}}`

**YAML validation errors** (from `POST /api/system/doctype/validate`):
```json
{"valid": false, "syntax": [{"line": 4, "column": 1, "key": "is_submittible", "context": "doctype", "detail": "did you mean \"is_submittable\"?"}]}
```
Unknown YAML keys are rejected with line numbers and Levenshtein-based suggestions. Fields inside `fields[]`, `constraints[]`, and `doc_constraints[]` are checked recursively.

### DocType & Field Config (`config/{app}/doctypes/*.yaml`)

Fields map to DB columns. Key field types: Data, Int, Float, Currency, Select, Link (autocomplete to target doctype), Table (child table — separate DB table with parent/parentfield/parenttype columns), Attach (file upload), Attach Image (image with thumbnail preview), Attach Audio (audio with player), Section Break, Column Break.

**New config-driven properties:**
- `computed: "quantity * unit_price"` — expression auto-calculated when dependencies change. Supports `+`, `-`, `*`, `/`, `SUM(table.field)`, `ROUND(expr, N)`
- `linked_field: "product.selling_price"` — auto-populates from linked document when Link field changes
- `unique: true` — DB UNIQUE index enforced at database level (MySQL error 1062 → field-level ValidationError)
- `renamed_from: "old_fieldname"` — non-breaking column rename via `ALTER TABLE RENAME COLUMN` during migration
- `constraints` — field constraints (min, max, regex, one_of, etc.) editable via visual form builder or YAML

### Repository Layout

This repository is the **Engine only**: a single Go binary built from `cmd/kora` (API, kernel, workflow, AI/MCP, scheduler, CLI) plus the config packs under `config/`. It no longer contains or embeds any UI, and there is no Bun/Node toolchain here.

| Repository | Local clone | What it is |
|---|---|---|
| `kora` (this repo) | `~/projects/kora` | Engine: Go module `github.com/asenawritescode/kora`, nested Go module `sdk/`, `config/` packs, `contract/` source of truth for the wire contract |
| `kora-ui` | `~/projects/kora-studio-mvp` | Studio, the React 19 workspace and admin UI (TanStack Router/Query/Table/Form, Tailwind v4, Zustand, Vite + Bun). Serves itself and proxies `/api` to the Engine |
| `kora-cloud` | `~/projects/kora-cloud` | Control plane: marketplace, signup, provisioning, customer portal (`web/`) |
| `kora-mobile-app` | `~/projects/kora-mobile` | Android client (Kotlin, Jetpack Compose), offline-first |
| `kora-website` | `~/projects/kora-website` | Public marketing site |
| `kora-ts-extension-sdk` | `~/projects/kora-ts-sdk` | TypeScript SDK and extension tooling |

The Engine's wire contract is generated here (`go run ./contract/gen -write`, output in `contract/generated/` and `contract/schema/`) and copied into the consumer repositories with `scripts/sync-contract-consumers.py`. Fixtures are in `contract-fixtures/` and catalogued in `docs/architecture/contract-fixture-manifest.json`.

Studio-specific behaviour (routes, admin pages, chat widget, component library) is documented in the kora-ui repository, not here. The Engine-side surfaces Studio calls are the REST API (`/api/v1`), the v2 request/module/offline routes (`/api/v2`), and the OpenAPI document at `/api/swagger-ui`.

### Config Versioning

Config versions use a status workflow: **Draft → Active → Superseded**. The `_kora_config_version` table has a `status` column (replaced `is_active`). Versions store a full config snapshot + diff changelog. Only one version is Active at a time. Draft versions are not applied to the live schema — they must be Activated. Versions can be rolled back. This is a current implementation detail, not yet the RFC's full immutable application-definition and reconciliation model.

### Schema Migration Safety Tiers

Every doctype change is classified on activation:

| Tier | Changes | Action |
|------|---------|--------|
| Safe | Add nullable field, new doctype, add index, rename via `renamed_from` | Auto-apply |
| Warning | Add required field no default, orphan field | Show impact, require confirm |
| Blocked | Change field type, rename without `renamed_from` | Require explicit fix |

The `schema.AnalyzeImpact()` function compares old vs new doctype, counts affected rows, and classifies each change.

### Key Packages

| Package | Purpose |
|---|---|
| `internal/doctype/` | DocType, Field, Constraint, Document, Registry, PermissionMatrix, Workflow, expression engine. Current resource model is DocType-first. |
| `internal/orm/` | Generic CRUD (Insert, Save, GetDoc, GetList, Delete), filter parsing, DB-level unique enforcement, batched child INSERTs, diff-based Save, ULID name generation, NOT NULL error parsing |
| `internal/db/` | SQL Dialect abstraction — MySQL/LibSQL DDL, DML, error parsing, schema introspection |
| `internal/schema/` | Diff registry vs live schema → DDL via dialect; column rename via `renamed_from` |
| `internal/api/` | REST handlers, envelope, CRUD, unified automations and status transitions, system endpoints, YAML validation, AI Chat, User management (`users.go`), Secrets management (`secrets.go`), OpenAPI spec generation. Kernel entrypoints exist but are not the only RFC target surface. |
| `internal/auth/` | Session auth (bcrypt), in-memory session cache (30s TTL), CSRF (double-submit cookie), SystemGuard, SiteGuard |
| `internal/net/` | SiteRouter with host validation, security headers, CORS, rate limiter, TLS (autocert), ULID request IDs |
| `internal/cli/` | Cobra CLI: serve, setup, migrate, config (import/export/versions/diff/rollback), new-site, mcp, secret |
| `internal/configstore/` | Read/write config to/from DB (_kora_doctype, _kora_field, etc.) |
| `internal/scheduler/` | Cron-style background jobs |
| `internal/secret/` | Encrypted API key storage (AES-256-GCM) for AI provider keys (settable in Studio: Settings → Secrets) |
| `internal/storage/` | Pluggable attachment blob storage — local filesystem + S3-compatible (MinIO/AWS/R2/Garage) backends with AWS SigV4 signing, presigned URLs, range reads |
| `internal/analytics/` | EventBus + Worker + Query Engine — real-time CDC, daily/monthly rollup tables, time-series/funnel/duration queries, auto-generated metrics |
| `internal/mcp/` | Model Context Protocol server — auto-generates tools from doctype registry for Claude Desktop |
| `internal/api/ai/` | AI Chat handlers, AI config, tool execution loop, token accounting |
| `internal/script/` | JS script runtime — sandboxed execution engine for extensions |
| `internal/webhook/` | Webhook dispatcher — outgoing and incoming webhook management |
| `sdk/` | Go SDK client library for external integrations |

### RFC Alignment Notes

- Kernel and contract surfaces are real and should be treated as the current base layer.
- Workflow runtime and reconciliation primitives exist, but they are smaller than the RFC target.
- The frontend is the standalone React-based Studio (kora-ui), not part of this repository.
- Docs should distinguish implemented behavior from aspirational RFC sections.
- New PostgreSQL/MySQL feature-parity work is intentionally excluded from follow-up implementation tickets unless explicitly reintroduced later. This does not waive verification: changes to existing dual-dialect code must still pass on both MySQL and Postgres (see Agent Rules).

### File Attachments & Storage

Attachments are stored as blobs in a pluggable storage backend, with only a site-scoped file reference stored in the doctype field. Field types:

- **`Attach`** — any file; renders as a filename link (PDF gets an inline preview).
- **`Attach Image`** — images only; renders an inline thumbnail.
- **`Attach Audio`** — audio only; renders an inline `<audio controls>` player with seeking.

Endpoints (all behind SiteGuard):
- `POST /api/v1/upload` — multipart upload; returns `{path, filename, key, mime_type, size, checksum}`.
- `GET /api/v1/files/*path` — serves a stored blob with HTTP Range support (audio/video seeking).
- `DELETE /api/v1/files/*path` — deletes a stored blob.

Each attach field can restrict accepted formats with an optional `accept` property (comma/newline-separated extensions `.pdf` or MIME types `image/*`). When set, it takes precedence over the field-type defaults and is enforced both client-side and at upload time.

The backend is resolved per site from the `file_storage` column in `_kora_site_registry` (overriding `KORA_STORAGE_BACKEND`), with S3 credentials/endpoint from env. Local backend persists a `.meta.json` sidecar (excluded from serving); the S3 backend stores metadata in object headers and supports SigV4 presigned GET URLs.

**Multiple attachments** use the existing child-table pattern: create an `Attachment` child doctype with an `Attach`/`Attach Image`/`Attach Audio` field, then add a `Table` field pointing at it. Each child row renders the same upload/preview control.

### AI Chat System

The canonical chat flow is `GET/POST /api/v1/ai/chat`. GET restores the durable transcript for the signed-in user and current page context; POST accepts only the new turn and context. The backend owns conversation history, creates a tracked run for each turn, and records messages, steps, tool execution, usage, and audit evidence. The shared UI API client must be used so path-routed `/s/:site` workspaces send chat to the active tenant.

Run cancellation, resume, and approval records live under `/api/v1/ai/runs` and `/api/v1/ai/approvals`. The old `/api/chat` and `/api/v1/chat` endpoints are retired. Site provider keys are configured in Studio (Settings → Secrets); shared provider keys can be enabled by the system administrator. `ai.chat` remains experimental while authorization and durable-effect risks are still open.

#### Tool Catalog (`internal/api/ai/tools.go`)

`BuildToolCatalog()` is the canonical source for chat tools. Each non-child DocType contributes typed `_find`, `_list`, `_get`, `_create`, and `_update` tools. The system catalog also includes analytics, DocType draft, script, and view tools. Create/update and administrative mutations require confirmation and recent authentication where configured. Record mutations use the kernel command path; reads and domain-specific system tools retain their owning service contracts.

#### Multi-Round Tool Execution Loop

The execution loop is `finish_reason`-driven (industry standard — same pattern as Anthropic SDK, OpenAI SDK, Vercel AI SDK, LangChain):

```
while round < MaxRounds:
    call AI (WITH tools always)
    switch finish_reason:
        case "stop"        → return final response
        case "tool_calls"  → execute tools, feed results back, loop
        case "length"      → return truncated response
        case "content_filter" → return policy message
```

**Safety nets** (all thresholds configurable via `AIConfig`, per-model defaults + site overrides):
- Max rounds (fallback cap, not primary termination)
- Stall detection (3× identical tool call → specific nudge injected)
- Tool error circuit breaker (5 errors → force model to respond)
- Context compaction at 80% token budget (preserves system prompt + first user message)
- Tool result size cap (4KB per result, head+tail preservation)
- HTTP timeout + exponential backoff retry on 429/503
- Textified tool call detection (scans for `<｜｜DSML｜｜tool_calls>` in content — retries with `tool_choice: "required"`)
- Narrate-then-act detection (GPT-4o false finish — re-prompts with tool_choice escalation)

#### AI Configuration (`internal/api/ai_config.go`)

`AIConfig` struct with per-model defaults + site-level overrides from `_kora_secret` (keys prefixed `ai.`):

```go
type AIConfig struct {
    MaxRounds, TokenBudget, MaxToolResultChars, StallThreshold, MaxToolErrors int
    CompactionThreshold float64  // 0.0-1.0
    MaxTokensPerCall, HTTPTimeoutSec, MaxRetries, RetryBackoffMs, HistoryLimit int
}
```

Model-specific defaults: GPT-4o (120K budget), Claude Sonnet (190K), DeepSeek V4 (120K).

#### AI Audit Trail

AI-created records use `owner = <authenticated_user>` (for permissions) and `modified_by = "ai-assistant"` (for audit). Query `WHERE modified_by = 'ai-assistant'` to find all AI-created records.

#### Safe Access Layer

All AI response parsing uses safe helpers (`safeGetString`, `safeGetMap`, `safeGetSlice`) — no bare type assertions. Prevents panics from unexpected provider response shapes (nil content, empty choices, missing finish_reason, DeepSeek `reasoning_content`).

#### Tool Name Parsing

Uses suffix matching against known operations (`_find`, `_list`, `_get`, `_create`, `_update`, `_delete`) instead of `strings.SplitN(name, "_", 2)`. This correctly handles multi-word doctype names like "Work Order" → `work_order_create`.

#### ORM Error Handling

Database errors are parsed via `db.Dialect.ParseError()` — dialect-neutral:
- MySQL: error 1062 (Duplicate) / 1364, 1048 (NOT NULL)
- LibSQL/SQLite: constraint violation messages (UNIQUE/NOT NULL constraint failed)
- Converted to `doctype.ValidationError` with user-friendly field labels

#### Doctype Creation Safety

- `create_doctype_draft` ALWAYS sets `status: "Draft"` — no migration runs, no table created
- Human must review and activate via Versions admin panel
- Config version stores: `status: Draft`, `is_active: 0`, `label: "Created X via AI (Draft)"`
- Confirmation UX: AI validates → summarizes in 2-3 lines → asks "Create as draft?" → waits for user confirmation → creates

#### Frontend Chat Widget

Implemented in Studio (kora-ui): `src/components/chat/ChatWidget.tsx` (floating panel) and `useChat.ts`, which restores the context-scoped transcript and sends new turns through `POST /api/v1/ai/chat` using the shared API client. The Engine contract is the chat endpoint above.

#### MCP Server (`internal/mcp/server.go`)

Separate from chat API. Auto-generates MCP tools from the doctype registry for use with Claude Desktop, Cursor, etc. over stdio transport. The server is experimental and, for tenant tool projection, currently behaves as a validation/execution bridge rather than a standalone agent runtime.

### Extensibility System

Kigae provides a comprehensive extensibility system for customizing application behavior without modifying core code.

#### Script Engine (`internal/script/`)

JavaScript runtime based on Go's JavaScript engine. Scripts run in a sandboxed environment with configurable RAM limits (`KORA_SCRIPTS_MAX_RAM`) and HTTP/domain allowlists. Disabled by default — enable with `KORA_SCRIPTS_ENABLED=true`.

- **Script types**: Event hooks, custom API methods, scheduled scripts, and computed fields. Automations are defined and run by the Engine v2 workflow system; scripts are not workflow steps.
- **Sandboxing**: Memory limits, CPU limits, timeout enforcement, no filesystem or network access
- **Admin UI**: the script editor, test runner and console log viewer are in Studio (Settings → Scripts)

#### Event Hooks

Scripts can hook into document lifecycle events:

| Hook | Trigger |
|------|---------|
| `before_insert` | Before a document is created |
| `after_insert` | After a document is created |
| `before_save` | Before a document is saved (create or update) |
| `after_save` | After a document is saved |
| `before_delete` | Before a document is deleted |
| `after_delete` | After a document is deleted |
| `on_submit` | When a document is submitted via workflow |
| `on_cancel` | When a document is cancelled via workflow |

Hooks receive the document as context and can modify it, validate it, or perform side effects.

#### Custom API Methods

Register custom REST endpoints implemented in JavaScript. Accessed at `/api/v1/x/:site/:method_name`. Useful for custom business logic, integrations, and specialized queries.

#### Automations

Record-triggered automations are configured in the Automations workspace. Their effects use registered Engine actions and are validated, previewed, and published through the v2 workflow API. Do not add a second script-based workflow executor or load the retired document workflow configuration.

#### Scheduled Scripts

Cron-style scheduled jobs written in JavaScript. Configured via the Extensions admin panel with standard cron expressions. Each scheduled script has its own sandboxed runtime.

#### Webhook Extensions (`internal/webhook/`)

- **Outgoing webhooks**: Trigger HTTP calls to external services on document events (create, update, delete, submit, cancel). Configure payload templates, headers, and retry policies.
- **Incoming webhooks**: External services can push data to Kigae via signed webhook endpoints. Each endpoint validates a secret token before processing.

#### Extension Auth

Extensions authenticate via:
- **API keys** — generated in the Extensions admin panel, scoped to specific doctypes and operations
- **Webhook secrets** — shared secrets for incoming webhook verification (HMAC-SHA256)
- **Session tokens** — existing user sessions for UI-triggered extensions

This surface is permission-gated and auditable, but it is not a general-purpose plugin runtime or unchecked code execution environment.

### API Versioning

The REST API is versioned via URL prefix:

| Prefix | Status |
|--------|--------|
| `/api/v1/` | Stable — backward compatible |
| `/api/` | Legacy — redirects to `/api/v1/` |

Versioning ensures that integrations built against `/api/v1/` continue to work across core updates. New features are added to the stable API without breaking changes.

All endpoints documented in the auto-generated OpenAPI 3.0 spec at `/api/swagger-ui` (uses `/api/v1/` base path).

### SDKs

Kigae provides two official SDKs for building integrations:

- **Go SDK** (`sdk/`): `import "github.com/asenawritescode/kora/sdk"` — full CRUD client, filter building, connection pooling. Use for Go services that need to interact with Kigae programmatically.
- **TypeScript SDK** (`@kora/sdk`): Available on npm, integrates with any JS/TS runtime (Node, Bun, Deno, browser). Full type safety with generated TypeScript types from doctype definitions.

### Code Quality

Go code follows these quality standards:

- **Lint gate (CI `Lint` job, run locally with `make lint-all`)** has three parts:
  1. `.golangci.yml` — govet (with `shadow`), errcheck, staticcheck, gosimple, unused, ineffassign, wastedassign, unconvert, unparam, nilerr, errorlint, durationcheck, copyloopvar, exhaustive, gocritic, bodyclose, noctx, contextcheck, rowserrcheck, sqlclosecheck, gosec, revive (exported-symbol docs, error and context conventions), misspell, with `-tags integration`. It fails a PR on any finding on a line the PR changes (`--new-from-rev`), so new code is clean while the backlog is cleared package by package. `make lint`.
  2. **Function complexity ratchet** — `.golangci-complexity.yml` sets the limits (cognitive and cyclomatic 15, 80 lines, 40 statements). `.lint-baseline/complexity.json` lists every function already over them. A function may not be newly over a limit, and a listed function may not get worse; extract named, tested steps instead of growing a god function. After refactoring run `make complexity-baseline`, which only lowers the baseline. `make complexity-ratchet`.
  3. **Dead code ratchet** — `deadcode ./...` lists functions unreachable from the binary and `cmd/` tools; `.lint-baseline/deadcode.txt` lists the existing ones. A newly unreachable function fails the build: delete it or call it. Deliberately kept code (API for another repository, helpers shared by integration tests) is added with `python3 scripts/deadcode-ratchet.py --update --allow-new` and justified in the PR. `make deadcode-ratchet`, `make deadcode-baseline`.
  `make lint-complexity` reports every hotspot, including nesting, duplication and repeated constants, which are informational.
- **Errors** — handle every error or discard it with `_ =` and a reason. Never shadow `err` inside an `if` that the code after it depends on. Use `errors.Is`/`As`, not string matching. Context keys use a private type, never a bare string.
- **No ignored parameters** — a function must not accept an identity (actor, owner, roles) that it then overwrites from the verified execution; remove the parameter. Check with `unparam`.
- **Dead code** — `deadcode ./...` (golang.org/x/tools/cmd/deadcode) lists functions unreachable from the binary. Check `//go:build integration` tests and Cloud before deleting.
- **Table-driven tests** — new code uses table-driven test patterns
- **Interface hygiene** — small interfaces (1-3 methods), defined at consumption point
- **No `utils`, `helpers`, `common` packages** — packages are organized by domain capability
- **Module layout** follows https://go.dev/doc/modules/layout for a server project: logic in `internal/`, commands in `cmd/`, and as few exported packages as possible. Exported packages are the contract with other repositories (`contract`, the nested `sdk` module); anything else Cloud or tooling needs should be exposed through them rather than by importing implementation packages. The layout is: `cmd/kora` (the server and CLI binary), `contract/gen` and `cmd/route-inventory` (developer tools), `internal/` (all implementation packages), `contract/` (the public wire contract) and `sdk/` (the public Go client module). Other repositories must not import `internal/...` (Go enforces this); anything they need is added to `contract` or `sdk`. Cloud tests that used to import `site`, `db`, `auth`, `api`, `doctype` or `management` need to run the Engine binary or use the SDK instead.
- **Error wrapping** — all errors use `%w` for proper error chains

### ORM Document Model

Documents are `map[string]any`. Parent document names are auto-generated: `PREFIX-NNNN` via `SELECT COUNT(*)` (prefix = first 4 chars of single-word names, first-letter-of-each-word for multi-word). Child row names use ULID: `PREFIX-<ulid>` (26-char sortable unique ID, no DB query needed). System columns on every table: `name`, `owner`, `creation`, `modified`, `modified_by`, `doc_status`, `idx`. Child tables add: `parent`, `parentfield`, `parenttype`. Table names are backtick-quoted for SQL safety (spaces in names like "Work Order").

`Insert(dt, doc, owner, modifiedBy)` — `modified_by` tracks the actor. REST API passes `owner` (user creating directly); AI Chat passes `"ai-assistant"` for audit trail.

### Multi-Tenancy

**MySQL**: Each site = separate database. Complete isolation.  
**LibSQL**: Every LibSQL site resolves to the shared `DB_DSN` database, and business tables are not tenant-prefixed, so a LibSQL deployment serves exactly one tenant. Activation claims the database in `_kora_database_owner` and refuses any second tenant (`site.ClaimDatabaseOwner`).

System console API at `/console` sees all sites (SystemGuard, env var credentials). Workspace API access is per-site (SiteGuard, per-site `_kora_user` table). Console auth uses Bearer tokens, separate from workspace session auth.

### SQL Dialect (`internal/db/` package)

All SQL must go through the Dialect; never hardcode database-specific syntax.

The `db.Dialect` interface abstracts all DB-specific SQL generation for MySQL and LibSQL:
- DDL: `CreateTable`, `AddColumn`, `ColumnType`, schema introspection (`LoadSchema` via INFORMATION_SCHEMA vs PRAGMA)
- DML: `UpsertClause` (ON DUPLICATE KEY UPDATE vs ON CONFLICT DO UPDATE), `InsertOrIgnorePrefix`
- Error parsing: `ParseError` (MySQL error codes vs SQLite constraint messages)
- `SystemTableSQL()` returns all 11 `_kora_*` CREATE TABLE statements per dialect

Dialect is resolved once at startup via `db.Resolve(common.DBType)` and threaded through all packages.

### Config is DB-Sourced

No YAML files for site config. `CommonConfigFromEnv()` builds all config from `KORA_*` environment variables. Sites are discovered from `_kora_site_registry` (see Startup Flow). YAML doctype files are used only for bulk import/export (`internal/cli/config_impl.go`).

Key environment variables for extensibility and analytics:
- `KORA_SCRIPTS_ENABLED` — Enable the JS script engine (default: `false`)
- `KORA_SCRIPTS_MAX_RAM` — Max RAM per script in MB (default: `64`)
- `KORA_ANALYTICS` — Enable analytics event bus and rollup tables (default: `false`)
- `KORA_NATS_SUBJECT_PREFIX` — Event subject tree `<prefix>.>` captured by the JetStream event stream (default: `kora`)
- `KORA_NATS_DEAD_LETTER` — Literal dead-letter subject, stored by the event stream but outside every consumer filter (default: `<prefix>_deadletter`; was `kora.deadletter`). Startup rejects a value the consumer filter (default `<prefix>.>`) matches, which would re-consume dead letters in a loop
- `KORA_STORAGE_BACKEND` — Attachment storage backend: `local` or `s3` (default: `local`)
- `KORA_STORAGE_LOCAL_PATH` — Local storage root dir (default: `.`)
- `KORA_STORAGE_S3_ENDPOINT` / `_REGION` / `_BUCKET` / `_ACCESS_KEY` / `_SECRET_KEY` — S3-compatible backend config
- `KORA_STORAGE_S3_USE_SSL` — TLS to S3 endpoint (default: `true`)
- `KORA_STORAGE_S3_PUBLIC_URL` — Optional public base URL (else presigned URLs are used)
- On startup, S3-compatible backends are checked and the bucket is created idempotently when missing.
- `KORA_ENGINE_CLOUD_IDENTITY_URL` / `KORA_ENGINE_CLOUD_IDENTITY_TOKEN` — Cloud sign-in handoff redemption endpoint and service token; set both or neither (neither: startup WARN and `POST /api/auth/cloud/handoff` answers 503 `auth.cloud_handoff_unconfigured`; one: startup fails). Local dev: `http://127.0.0.1:8081/internal/identity/handoff/redeem`
- `KORA_MAX_UPLOAD_MB` — Per-file upload cap (default: `50`)

## Release Workflow

### CI/CD (GitHub Actions)

On every PR and push to `main`:
- **Go**: `golangci-lint run ./...` → `go test ./...` → `go build`

On tag push (`v*`): builds binaries for linux/darwin (amd64/arm64), categorizes commits into Features/Fixes/Security/Improvements/Docs, creates a GitHub Release with download links and SHA256 checksums.

### Creating a Release

```bash
# Full release: validates, generates changelog, bumps version, tags, pushes
./scripts/release.sh v0.2.0

# Or via Make:
make release TAG=v0.2.0
```

The release script (`scripts/release.sh`) runs these steps:

| Step | What it does |
|------|-------------|
| 1. **Validate** | `go test ./...` + `go build` — blocks release if checks fail |
| 2. **Changelog** | Categorizes commits into Features/Fixes/Security/Improvements/Docs, prepends to `CHANGELOG.md` |
| 3. **Version bump** | Writes version number to `VERSION` file |
| 4. **Tag & push** | Commits changelog + version, creates annotated tag, pushes |

After push, GitHub Actions:
- Builds `kora-linux-amd64`, `kora-darwin-amd64`, `kora-darwin-arm64` with SHA256 checksums
- Creates a GitHub Release with categorized release notes and download links

### Version File

`VERSION` at the repo root holds the current version (e.g., `0.2.0`). The release script bumps it automatically. The `go.mod` uses `go 1.25.0` (gin v1.12.0 requires it). CI uses `golangci-lint run ./...` for linting across all packages.

### Branch Rules (set in GitHub Settings → Rules → Rulesets)

- Require PR before merging to `main`
- Require status checks (`Go`) to pass
- Block force pushes
- Require linear history (rebase/squash, no merge commits)

### Docker Release (Manual — not in CI yet)

Images are built and pushed manually to Docker Hub. GitHub Actions CI (above) handles Go checks only — Docker publishing is done locally for now.

```bash
# 1. Build with version injected via ldflags
VSN=$(cat VERSION)
docker build --build-arg VERSION=$VSN -t kora:$VSN .

# 2. Test locally
docker run -d --name kora-test -p 8001:8000 \
  -e KORA_DB_TYPE=mysql \
  -e CONSOLE_EMAIL=admin@kora.local -e CONSOLE_PASSWORD=kora123 \
  kora:$VSN
curl -s http://localhost:8001/api/v1/ping
# {"message":"pong","version":"0.5.0-alpha.3"}
docker stop kora-test && docker rm kora-test

# 3. Tag for Docker Hub
docker tag kora:$VSN smitdockerhub/kora:$VSN
docker tag kora:$VSN smitdockerhub/kora:latest

# 4. Push
docker login -u smitdockerhub
docker push smitdockerhub/kora:$VSN
docker push smitdockerhub/kora:latest
```

**Image:** `smitdockerhub/kora` — supports both MySQL and LibSQL, pure Go (no CGO), ~30MB, version injected at build time.

**Dockerfile** (`/Dockerfile`): Multi-stage — Go build (CGO_ENABLED=0, ldflags for version), then Alpine runtime. The image contains the Engine only; the UI is deployed separately. `build-arg VERSION` sets the version string returned by `/api/v1/ping`.

## Contributing

See `CONTRIBUTING.md` for full guidelines. PRs must pass CI before merging. All changes to `main` go through pull requests — never push directly to `main`.

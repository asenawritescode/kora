# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
make dev                # MySQL + build + setup + serve (one command)
make build              # Build UI + Go binary
make serve              # Start server on :8000
make restart            # Kill old server + rebuild all + start fresh
make setup              # Setup a site (SITE=airtime.local CONFIG=config/airtime/)
make test               # Run Go tests
make lint               # Run linters (Go + TypeScript)
make fmt                # Format code
make release TAG=v0.2.0 # Tag and push a release
make help               # Show all commands
```

### Manual Commands

```bash
# Backend (Go)
go build -o kora .                           # Build binary
go run . serve --port 8000                   # Dev run
go run . migrate --all                       # Apply all pending migrations
go run . config import --site X --path Y     # Re-import YAML config to DB

# AI & Secrets (also configurable via UI at /workspace/admin/secrets)
./kora secret set --site X --key deepseek_api_key --value sk-...   # Set AI provider key (CLI)
./kora mcp --site X                           # Start MCP server (stdio) for Claude Desktop

# Frontend (React SPA in ui/)
cd ui && bun install                         # Install deps
cd ui && bun run build                       # Build SPA → dist/
cd ui && bun run dev                         # Dev server (proxies /api → :8000)

# Docker
docker compose up -d mysql                   # MySQL 8.0 (root:kora123)
```

## Architecture

Kora is a **config-driven application engine**. Applications are YAML configs — the engine is generic and permanent. No code generation, no per-entity Go/React code.

### Startup Flow (`cli/serve.go`)

1. Load config from environment variables (`CommonConfigFromEnv` — no YAML files)
2. Connect to platform DB (MySQL or remote LibSQL via `DB_DSN`)
3. Discover sites from DB: `SELECT DISTINCT site FROM _kora_config_version`
4. Per site: reconstruct config from platform defaults + persisted domains → connect → bootstrap `_kora_*` tables → load config from DB → build Registry → run schema migration
5. Build `SiteRouter` (domain → site map)
6. Wire middleware: Recovery → RequestID → SecurityHeaders → CORS → SiteRouter → RateLimiter
7. Register auth routes (public), API routes (/api — SiteGuard), SPA (/workspace — NoRoute), console (/console — SystemGuard)
8. Start scheduler, listen, graceful shutdown on SIGTERM

**Site config is reconstructed from env vars + DB** — no `site_config.yaml` files. Sites survive container redeploys because metadata persists in `_kora_config_version.config` (domains, etc.).

### Middleware Chain

```
Request  → Recovery → RequestID → SecurityHeaders → CORS → SiteRouter → RateLimiter
         → API routes: SiteGuard (Auth + CSRF) → Permission check → Validation → ORM
         → /workspace: NoRoute handler serves SPA directly
         → /console: SystemGuard (system_credentials.yaml, separate from site auth)
         → /api/auth: public (no guard)
```

### Multi-Site Routing

Three methods coexist:
- **Host-based**: `Host` header → site (production, needs DNS)
- **Path-based**: `/s/:site/workspace` → site (dev, no config needed)
- **Default**: localhost/IP → first loaded site

The `SiteRouter` middleware sets `site_name`, `site_db`, `site_registry` in Gin context. **All auth is site-scoped** — login, session creation, session validation, and logout all read `site_db` from context. A session from one site doesn't work on another.

The `kora_site` cookie (set by path-based routing at `/s/:site/workspace`) resolves the site for subsequent API calls. Path-based routing skips host validation entirely — the site is identified by the URL path prefix, not the Host header.

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

Fields map to DB columns. Key field types: Data, Int, Float, Currency, Select, Link (autocomplete to target doctype), Table (child table — separate DB table with parent/parentfield/parenttype columns), Section Break, Column Break.

**New config-driven properties:**
- `computed: "quantity * unit_price"` — expression auto-calculated when dependencies change. Supports `+`, `-`, `*`, `/`, `SUM(table.field)`, `ROUND(expr, N)`
- `linked_field: "product.selling_price"` — auto-populates from linked document when Link field changes
- `unique: true` — DB UNIQUE index enforced at database level (MySQL error 1062 → field-level ValidationError)
- `renamed_from: "old_fieldname"` — non-breaking column rename via `ALTER TABLE RENAME COLUMN` during migration
- `constraints` — field constraints (min, max, regex, one_of, etc.) editable via visual form builder or YAML

### Frontend (`ui/`)

React 19 + TanStack Router/Query/Table/Form + shadcn/ui + Tailwind CSS v4 + Zustand. All views are **config-driven** — the SPA reads `/api/system/doctype/:name` and renders forms, lists, and workflow generically. No per-doctype components.

Key patterns:
- `router.tsx`: Auto-detects basepath for multi-site (`/s/:site` prefix). **Admin routes must be registered before `$doctype`** to avoid the catch-all route capturing literal paths. Admin pages are under `ui/src/routes/workspace/admin/`.
- `lib/basepath.ts`: `sitePath()` helper — all navigation uses this to preserve site prefix
- `lib/computed-fields.ts`: Expression evaluator for `computed` fields
- `lib/expression-eval.ts`: Parses `SUM()`, `ROUND()`, arithmetic
- `components/tables/DataTable.tsx`: Shared table component — desktop table + mobile stacked cards via `hidden md:` / `md:hidden`
- Forms served via `NoRoute` handler in `workspace/spa.go` (not middleware — Gin's radix tree conflicts)
- **Mobile**: Tables use stacked card layout. Permissions uses role drill-down accordion. Workflow editor uses collapsible card sections. No horizontal scroll anywhere.

### Administrator Tab (SPA)

The workspace sidebar has an Administrator section with seven views, all config-driven:

| Page | Route | Purpose |
|------|-------|---------|
| DocTypes | `/workspace/admin/doctypes` | CRUD doctypes via visual form builder + live YAML panel |
| Permissions | `/workspace/admin/permissions` | Role × DocType permission matrix, inline editing |
| Workflows | `/workspace/admin/workflows` | Define state machines for submittable doctypes |
| Versions | `/workspace/admin/versions` | Config version history with Activate/Discard/Rollback |
| Users | `/workspace/admin/users` | User CRUD, role assignment, enable/disable, password reset |
| Secrets | `/workspace/admin/secrets` | AI provider keys (OpenAI, DeepSeek, Anthropic) via dropdown UI |
| API Docs | `/api/swagger-ui` | Auto-generated OpenAPI 3.0 spec with interactive Swagger UI |

The doctype editor uses a split-pane layout: visual form builder on the left, live YAML preview on the right (with syntax highlighting). YAML is editable and can be applied back to the form via `js-yaml` client-side parsing.

### Config Versioning

Config versions use a status workflow: **Draft → Active → Superseded**. The `_kora_config_version` table has a `status` column (replaced `is_active`). Versions store a full config snapshot + diff changelog. Only one version is Active at a time. Draft versions are not applied to the live schema — they must be Activated. Versions can be rolled back.

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
| `doctype/` | DocType, Field, Constraint, Document, Registry, PermissionMatrix, Workflow, expression engine |
| `orm/` | Generic CRUD (Insert, Save, GetDoc, GetList, Delete), filter parsing, DB-level unique enforcement, batched child INSERTs, diff-based Save, ULID name generation, NOT NULL error parsing |
| `db/` | SQL Dialect abstraction — MySQL/LibSQL DDL, DML, error parsing, schema introspection |
| `schema/` | Diff registry vs live schema → DDL via dialect; column rename via `renamed_from` |
| `api/` | REST handlers, envelope, CRUD, workflow actions, system endpoints, YAML validation, AI Chat, User management (`users.go`), Secrets management (`secrets.go`), OpenAPI spec generation |
| `auth/` | Session auth (bcrypt), in-memory session cache (30s TTL), CSRF (double-submit cookie), SystemGuard, SiteGuard |
| `net/` | SiteRouter with host validation, security headers, CORS, rate limiter, TLS (autocert), ULID request IDs |
| `cli/` | Cobra CLI: serve, setup, migrate, config (import/export/versions/diff/rollback), new-site, mcp, secret |
| `configstore/` | Read/write config to/from DB (_kora_doctype, _kora_field, etc.) |
| `workspace/` | SPA serving (go:embed dist/*), NoRoute handler, static file server |
| `console/` | System console — React SPA for site creation + system admin (SystemGuard auth) |
| `scheduler/` | Cron-style background jobs |
| `secret/` | Encrypted API key storage (AES-256-GCM) for AI provider keys (settable via UI at `/workspace/admin/secrets`) |
| `analytics/` | EventBus + Worker + Query Engine — real-time CDC, daily/monthly rollup tables, time-series/funnel/duration queries, auto-generated metrics |
| `mcp/` | Model Context Protocol server — auto-generates tools from doctype registry for Claude Desktop |
| `ui/` | React SPA (Vite + TanStack + shadcn) with floating AI Chat Widget |

### AI Chat System

The AI chat at `POST /api/chat` auto-generates OpenAI-compatible function definitions from the doctype registry and executes tools via the ORM. Supports OpenAI, DeepSeek, and Anthropic providers (all via `/chat/completions` endpoint). API keys are configured via the Secrets admin page at `/workspace/admin/secrets` — no CLI required.

#### Tool Generation (`api/chat.go`)

For each non-child-table DocType, `buildFunctions()` generates tools:

| Tool pattern | Purpose |
|-------------|---------|
| `<doctype>_find` | Search by field values (duplicate check before create) |
| `<doctype>_list` | List documents (paginated, markdown table format) |
| `<doctype>_get` | Get single document by name |
| `<doctype>_create` | Create a new document |

Plus system-level tools from `buildSystemFunctions()`:

| Tool | Purpose |
|------|---------|
| `list_doctypes` | List all doctypes with field counts |
| `validate_doctype_yaml` | Validate YAML without saving, returns line-numbered errors |
| `create_doctype_draft` | Create new DocType as **Draft only** — never activates, never runs migrations |

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

#### AI Configuration (`api/ai_config.go`)

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

- `ui/src/components/chat/ChatWidget.tsx` — floating Intercom-style panel (bottom-right)
- `ui/src/components/chat/useChat.ts` — React hook, sends `POST /api/chat` with CSRF token
- Embedded in `RootLayout.tsx` — available on every page

#### MCP Server (`mcp/server.go`)

Separate from chat API. Auto-generates MCP tools (5 per doctype: list, create, get, update, delete) for use with Claude Desktop, Cursor, etc. over stdio transport.

### ORM Document Model

Documents are `map[string]any`. Parent document names are auto-generated: `PREFIX-NNNN` via `SELECT COUNT(*)` (prefix = first 4 chars of single-word names, first-letter-of-each-word for multi-word). Child row names use ULID: `PREFIX-<ulid>` (26-char sortable unique ID, no DB query needed). System columns on every table: `name`, `owner`, `creation`, `modified`, `modified_by`, `doc_status`, `idx`. Child tables add: `parent`, `parentfield`, `parenttype`. Table names are backtick-quoted for SQL safety (spaces in names like "Work Order").

`Insert(dt, doc, owner, modifiedBy)` — `modified_by` tracks the actor. REST API passes `owner` (user creating directly); AI Chat passes `"ai-assistant"` for audit trail.

### Multi-Tenancy

**MySQL**: Each site = separate database. Complete isolation.  
**LibSQL**: All sites share the same remote database. Isolation via site-specific table naming and `_kora_config_version.site` column.

System console at `/console` sees all sites (SystemGuard, env var credentials). Workspace at `/workspace` is per-site (SiteGuard, per-site `_kora_user` table). Console auth uses Bearer tokens (`kora_console_token` in localStorage), separate from workspace session auth.

### SQL Dialect (`db/` package)

> **Skill**: `.claude/skills/db-compat.md` — invoke when writing SQL, reviewing DB code, or debugging MySQL/LibSQL errors. All SQL must go through the Dialect; never hardcode database-specific syntax.

The `db.Dialect` interface abstracts all DB-specific SQL generation for MySQL and LibSQL:
- DDL: `CreateTable`, `AddColumn`, `ColumnType`, schema introspection (`LoadSchema` via INFORMATION_SCHEMA vs PRAGMA)
- DML: `UpsertClause` (ON DUPLICATE KEY UPDATE vs ON CONFLICT DO UPDATE), `InsertOrIgnorePrefix`
- Error parsing: `ParseError` (MySQL error codes vs SQLite constraint messages)
- `SystemTableSQL()` returns all 11 `_kora_*` CREATE TABLE statements per dialect

Dialect is resolved once at startup via `db.Resolve(common.DBType)` and threaded through all packages.

### Config is DB-Sourced

No YAML files for site config. `CommonConfigFromEnv()` builds all config from `KORA_*` environment variables. Sites are discovered from `_kora_config_version`. YAML doctype files are used only for bulk import/export (`cli/config_impl.go`).

## Release Workflow

### CI/CD (GitHub Actions)

On every PR and push to `main`:
- **Go**: `go vet ./...` → `go test ./...` → `go build`
- **UI**: `bun install` → `tsc --noEmit` → `bun run build`

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

`VERSION` at the repo root holds the current version (e.g., `0.2.0`). The release script bumps it automatically. The `go.mod` uses `go 1.25.0` (gin v1.12.0 requires it). CI uses `go vet` instead of `golangci-lint` because golangci-lint v1.64.8 doesn't support Go 1.25 analysis yet.

### Branch Rules (set in GitHub Settings → Rules → Rulesets)

- Require PR before merging to `main`
- Require status checks (`Go`, `UI`) to pass
- Block force pushes
- Require linear history (rebase/squash, no merge commits)

### Docker Release (Manual — not in CI yet)

Images are built and pushed manually to Docker Hub. GitHub Actions CI (above) handles Go/UI checks only — Docker publishing is done locally for now.

```bash
# 1. Build with version injected via ldflags
VSN=$(cat VERSION)
docker build --build-arg VERSION=$VSN -t kora:$VSN .

# 2. Test locally
docker run -d --name kora-test -p 8001:8000 \
  -e KORA_DB_TYPE=mysql \
  -e CONSOLE_EMAIL=admin@kora.local -e CONSOLE_PASSWORD=kora123 \
  kora:$VSN
curl -s http://localhost:8001/api/ping
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

**Dockerfile** (`/Dockerfile`): Multi-stage — Bun for UI, Go for binary (CGO_ENABLED=0, ldflags for version), Alpine runtime. `build-arg VERSION` sets the version string returned by `/api/ping`.

## Contributing

See `CONTRIBUTING.md` for full guidelines. PRs must pass CI before merging. All changes to `main` go through pull requests — never push directly to `main`.

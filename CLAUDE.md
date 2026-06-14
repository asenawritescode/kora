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

# AI & Secrets
./kora secret set --site X --key deepseek_api_key --value sk-...   # Set AI provider key
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

1. Load `common_site_config.yaml`
2. Discover sites in `sites/` (subdirs with `site_config.yaml`)
3. Per site: connect DB → bootstrap `_kora_*` tables → load config from DB → build Registry → run schema migration
4. Build `SiteRouter` (domain → site map)
5. Wire middleware: Recovery → RequestID → SecurityHeaders → CORS → SiteRouter → RateLimiter
6. Register auth routes (public), API routes (/api — SiteGuard), SPA (/workspace — NoRoute), console (/console — SystemGuard)
7. Start scheduler, listen, graceful shutdown on SIGTERM

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

The `kora_site` cookie (set by path-based routing) is validated against the request Host header via `isHostAllowedForSite()` — only `localhost`, loopback IPs, or the site's configured domains are allowed. Unknown hosts get 403.

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

The workspace sidebar has an Administrator section with four views, all config-driven:

| Page | Route | Purpose |
|------|-------|---------|
| DocTypes | `/workspace/admin/doctypes` | CRUD doctypes via visual form builder + live YAML panel |
| Permissions | `/workspace/admin/permissions` | Role × DocType permission matrix, inline editing |
| Workflows | `/workspace/admin/workflows` | Define state machines for submittable doctypes |
| Versions | `/workspace/admin/versions` | Config version history with Activate/Discard/Rollback |

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
| `schema/` | INFORMATION_SCHEMA diff → DDL (additive + rename), column rename via `renamed_from` |
| `api/` | REST handlers, envelope, CRUD, workflow actions, system endpoints, YAML validation, **AI Chat** |
| `auth/` | Session auth (bcrypt), in-memory session cache (30s TTL), CSRF (double-submit cookie), SystemGuard, SiteGuard |
| `net/` | SiteRouter with host validation, security headers, CORS, rate limiter, TLS (autocert), ULID request IDs |
| `cli/` | Cobra CLI: serve, setup, migrate, config (import/export/versions/diff/rollback), new-site, mcp, secret |
| `configstore/` | Read/write config to/from DB (_kora_doctype, _kora_field, etc.) |
| `workspace/` | SPA serving (go:embed dist/*), NoRoute handler, static file server |
| `console/` | System console (server-rendered Go templates, SystemGuard auth) |
| `scheduler/` | Cron-style background jobs |
| `secret/` | Encrypted API key storage (AES-256-GCM) for AI provider keys |
| `mcp/` | Model Context Protocol server — auto-generates tools from doctype registry for Claude Desktop |
| `ui/` | React SPA (Vite + TanStack + shadcn) with floating AI Chat Widget |

### AI Chat System

The AI chat at `POST /api/chat` auto-generates OpenAI-compatible function definitions from the doctype registry and executes tools via the ORM. Supports OpenAI, DeepSeek, and Anthropic providers (all via `/chat/completions` endpoint).

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

MySQL errors are parsed into clean validation errors before reaching the AI:
- Error 1062 (Duplicate) → `"X must be unique."` (`parseDuplicateError`)
- Error 1364/1048 (NOT NULL) → `"X is required."` (`parseNotNullError`)

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

Complete database isolation per site. Each site = separate MySQL database. No cross-site data leakage. System console at `/console` sees all sites (SystemGuard, separate `system_credentials.yaml`). Workspace at `/workspace` is per-site (SiteGuard, per-site `_kora_user` table).

### Config is DB-Sourced

YAML files are one-shot imports. Config lives in `_kora_*` tables. Versioned with changelog. Additive schema changes auto-applied. Destructive changes (DROP COLUMN, CHANGE TYPE) require `--allow-breaking`. Export via `kora config export`.

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

## Contributing

See `CONTRIBUTING.md` for full guidelines. PRs must pass CI before merging. All changes to `main` go through pull requests — never push directly to `main`.

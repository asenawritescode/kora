# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Backend (Go)
go build -o kora .                           # Build binary
go run . serve --port 8000                   # Dev run
go run . setup --site airtime.local ...      # One-command site setup
go run . migrate --all                       # Apply all pending migrations
go run . config import --site X --path Y     # Re-import YAML config to DB
go test ./...                                # Run tests

# Frontend (React SPA in ui/)
cd ui && bun install                         # Install deps
cd ui && bun run build                       # Build SPA → dist/
cd ui && bun run dev                         # Dev server (proxies /api → :8000)

# Full build (SPA must be built first — embedded via go:embed)
cd ui && bun run build && cp -r dist ../workspace/dist && cd .. && go build -o kora .

# Docker
docker compose up -d mysql                   # MySQL 8.0 (root:kora123)
docker compose up -d                         # MySQL + Redis
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

### API Envelope

All responses: `{"data": ..., "meta": {"doctype": "...", "total": N, "config_version": N}}`  
Errors: `{"error": "plain message"}` or `{"error": {"type": "UniqueConstraint", "message": "...", "field": "fieldname"}}`  
Multiple: `{"error": {"errors": [{"type": "...", "message": "...", "field": "..."}]}}`

### DocType & Field Config (`config/{app}/doctypes/*.yaml`)

Fields map to DB columns. Key field types: Data, Int, Float, Currency, Select, Link (autocomplete to target doctype), Table (child table — separate DB table with parent/parentfield/parenttype columns), Section Break, Column Break.

**New config-driven properties:**
- `computed: "quantity * unit_price"` — expression auto-calculated when dependencies change. Supports `+`, `-`, `*`, `/`, `SUM(table.field)`, `ROUND(expr, N)`
- `linked_field: "product.selling_price"` — auto-populates from linked document when Link field changes
- `unique: true` — DB UNIQUE index + pre-save SELECT check → field-level error

### Frontend (`ui/`)

React 19 + TanStack Router/Query/Table/Form + shadcn/ui + Tailwind CSS v4 + Zustand. All views are **config-driven** — the SPA reads `/api/system/doctype/:name` and renders forms, lists, and workflow generically. No per-doctype components.

Key patterns:
- `router.tsx`: Auto-detects basepath for multi-site (`/s/:site` prefix)
- `lib/basepath.ts`: `sitePath()` helper — all navigation uses this to preserve site prefix
- `lib/computed-fields.ts`: Expression evaluator for `computed` fields
- `lib/expression-eval.ts`: Parses `SUM()`, `ROUND()`, arithmetic
- Forms served via `NoRoute` handler in `workspace/spa.go` (not middleware — Gin's radix tree conflicts)

### Key Packages

| Package | Purpose |
|---|---|
| `doctype/` | DocType, Field, Constraint, Document, Registry, PermissionMatrix, Workflow, expression engine |
| `orm/` | Generic CRUD (Insert, Save, GetDoc, GetList, Delete), filter parsing, unique constraint check |
| `schema/` | INFORMATION_SCHEMA diff → DDL (additive only by default) |
| `api/` | REST handlers, envelope, CRUD, workflow actions, system endpoints |
| `auth/` | Session auth (bcrypt), CSRF (double-submit cookie), SystemGuard, SiteGuard |
| `net/` | SiteRouter, security headers, CORS, rate limiter, TLS (autocert) |
| `cli/` | Cobra CLI: serve, setup, migrate, config (import/export/versions/diff/rollback), new-site |
| `configstore/` | Read/write config to/from DB (_kora_doctype, _kora_field, etc.) |
| `workspace/` | SPA serving (go:embed dist/*), NoRoute handler, static file server |
| `console/` | System console (server-rendered Go templates, SystemGuard auth) |
| `scheduler/` | Cron-style background jobs |
| `ui/` | React SPA (Vite + TanStack + shadcn) |

### ORM Document Model

Documents are `map[string]any`. Names are auto-generated: `PREFIX-NNNN` (prefix = first 4 chars of single-word names, first-letter-of-each-word for multi-word). System columns on every table: `name`, `owner`, `creation`, `modified`, `modified_by`, `doc_status`, `idx`. Child tables add: `parent`, `parentfield`, `parenttype`. Table names are backtick-quoted for SQL safety (spaces in names like "Work Order").

### Multi-Tenancy

Complete database isolation per site. Each site = separate MySQL database. No cross-site data leakage. System console at `/console` sees all sites (SystemGuard, separate `system_credentials.yaml`). Workspace at `/workspace` is per-site (SiteGuard, per-site `_kora_user` table).

### Config is DB-Sourced

YAML files are one-shot imports. Config lives in `_kora_*` tables. Versioned with changelog. Additive schema changes auto-applied. Destructive changes (DROP COLUMN, CHANGE TYPE) require `--allow-breaking`. Export via `kora config export`.

## Release Workflow

### CI/CD (GitHub Actions)

On every PR and push to `main`:
- **Go**: `golangci-lint` → `go test ./...` → `go build`
- **UI**: `bun install` → `tsc --noEmit` → `bun run build`

On tag push (`v*`): auto-generates release notes and creates a GitHub Release.

### Creating a Release

```bash
# 1. All changes go through PRs against main
# 2. CI must be green
# 3. Merge the PR
# 4. Tag and push:
git tag -a v0.2.0 -m "Description of changes"
git push origin v0.2.0
```

The release workflow auto-generates release notes from commit history.

### Branch Rules (set in GitHub Settings → Rules → Rulesets)

- Require PR before merging to `main`
- Require status checks (`Go`, `UI`) to pass
- Block force pushes
- Require linear history (rebase/squash, no merge commits)

## Contributing

See `CONTRIBUTING.md` for full guidelines. PRs must pass CI before merging. All changes to `main` go through pull requests — never push directly to `main`.

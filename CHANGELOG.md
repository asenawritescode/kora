## v1.0.0 — 2026-11-13

The Engine is API-only: the UI is deployed separately from kora-ui, and the Docker image and release workflow build only the Go binary.

### Features
- Config packs for Selling (pos-base: till sessions that must be open to sell, closed with a cash count, multi-currency, mobile-money payments, owner dashboard), Stock, Buying and Books
- Module packs: CRM, People, Documents, Assets, Projects, Fleet, Manufacturing, Subscriptions, Menu and tables, Dispensary (batch-expiry), Produce (farm-produce), Students, Shipments, Properties, Appointments, Workshop (job-cards), Members and savings, Grants, Rooms and bookings, each with roles, permissions, a view and a report
- Package upgrades are refused when they would invalidate records a tenant already has
- Tenant member plan, grant and revoke operations; optional ProvisionTenant display name
- quantity_effect applies on any create; ensure-command steps; $steps.<alias>.<field> reads
- ZANA_* environment variables, with the KORA_* names kept as a deprecated fallback for one release

### Fixes
- A unique field gets its unique index on a table that already exists
- A record type with no fields or constraints serialises them as [], never null
- 34 more fixes since v1.0.0-alpha.2; see the full diff

### Improvements
- Exported names no longer repeat their package; the contract generator and command directories were renamed

**Full diff:** https://github.com/asenawritescode/kora/compare/v1.0.0-alpha.2...v1.0.0


## v0.9.5 — 2026-08-31

### Features
- feat(storage): provision per-site buckets

### Fixes
- fix(storage): bootstrap s3 buckets on startup


## v0.9.4-alpha — 2026-07-28
### Fixes
- fix: generate template pack hashes on save


## v0.9.3-alpha — 2026-07-28
### Fixes
- fix(ai): allow channel update record names


## v0.9.2-alpha — 2026-07-28
### Fixes
- fix(ai): include record IDs in channel summaries


## v0.9.1-alpha — 2026-07-28

### Features
- feat(ai): expose channel update tools


## v0.8.20-alpha — 2026-07-22
### Fixes
- fix: skip no-op resource updates
- fix: allocate generated document names atomically

### Documentation
- fix: allocate generated document names atomically


## v0.8.19-alpha — 2026-07-22

### Features
- feat: support managed template packs


## v0.8.18-alpha — 2026-07-17
### Features
- feat: add domain template config packs

### Tests
- test: cover new template config pack parsing

## v0.8.17-alpha — 2026-07-16
### Fixes
- fix: improve form and mobile sidebar UX

### Improvements
- fix: improve form and mobile sidebar UX


## v0.8.16-alpha — 2026-07-16
### Fixes
- fix: load public access from doctype config


## v0.8.15-alpha — 2026-07-16
### Fixes
- fix: infer config import database from site registry


## v0.8.14-alpha — 2026-07-16
### Fixes
- fix: verify setup admin users


## v0.8.13-alpha — 2026-07-16

### Features
- fix: make mysql create migrations resumable

### Fixes
- fix: make mysql create migrations resumable


## v0.8.12-alpha — 2026-07-16
### Fixes
- fix: allow public content config validation


## v0.8.11-alpha — 2026-07-16

### Features
- feat: add explicit public content access


## v0.8.8-alpha — 2026-07-15

### Features
- feat: add schema-driven ai tool filters


## v0.8.6-alpha — 2026-07-11

### Fixes
- fix: preserve site context for `/s/:site/api/*` re-dispatch during path-based routing
- fix: keep path-based API routing working for non-browser callers during fallback execution paths

### UX
- feat: redesign auth screens for clearer mobile-first sign-in, better magic-link feedback, and stronger workspace context

## v0.8.5-alpha — 2026-07-11

### Fixes
- fix: preserve `/s/:site` in magic-link invite URLs for path-based tenants
- fix: keep `/s/:site/api/*` routing available even when the SPA bundle is not embedded

## v0.8.4-alpha — 2026-07-10

### Features
- feat: delegated channel auth and magic links

### Security
- feat: delegated channel auth and magic links


## v0.8.3-alpha — 2026-07-09

### Features
- feat: context-aware AI co-creator with route-aware suggestions
- feat: mobile-first workspace shell with sheet-based navigation
- feat: sectioned create/edit forms with sticky progress and draft recovery
- feat: IndexedDB-backed draft autosave with local fallback metadata
- feat: markdown-rich assistant responses with GitHub Flavored Markdown

### Fixes
- fix: child table row updates now preserve the actual field name
- fix: draft autosave no longer depends on synchronous localStorage for large payloads
- fix: restore tables, code, links, and emphasis in assistant replies

## v0.8.2-alpha — 2026-07-08

### Features
- feat: strengthen config versioning flow
- Add runtime site reload endpoint
- fix: stop auto-adding request host as tenant domain on site creation
- feat: durable site registry for console-created sites
- fix: add tzdata to Docker runtime image for Zygomys timezone support
- feat: wire DiffSExpr for change_list + LispAutocomplete for predicates
- fix: add site column to _kora_field INSERT to match DELETE's site filter
- fix: add reserved field name validation — prevent conflicts with system columns
- feat: UX gap closure — real-time Lisp preview, predicate UI, conflict diff, autocomplete
- feat: phase 3 cleanup — remove expr-lang, cut over to Lisp
- feat: phase 3 — validation predicates, PostgreSQL dialect, documentation, cutover to Lisp
- feat: phase 2 — full snapshot s-expression IR and structural diff
- feat: phase 1 — embedded Lisp runtime (Zygomys) for computed fields
- fix: add site column to LibSQL system tables for multi-tenant isolation

### Fixes
- Fix registry-only site discovery
- fix: reuse DB_DSN for site database creation instead of manual env vars
- fix: stop auto-adding request host as tenant domain on site creation
- fix: workflow conditions, table field validation, and integration tests
- fix: add tzdata to Docker runtime image for Zygomys timezone support
- fix: LibSQL-compatible DDL ordering in activation and rollback
- fix: make activation deactivation atomic within transaction
- fix: mark Draft as Superseded after activation
- fix: add site column to _kora_field INSERT to match DELETE's site filter
- fix: HandleConfigDiff uses stored change_list, not yaml.Unmarshal
- fix: add reserved field name validation — prevent conflicts with system columns
- fix: change config column from JSON to LONGTEXT for s-expression storage
- fix: update AI chat tool descriptions for s-expression syntax
- fix: robust isIdempotentSQLError — catch MySQL 1064 syntax errors for duplicate columns
- perf: batch DDL execution, batch config save, fix registry-migration ordering
- fix: unified idempotent SQL error handling in bootstrap
- fix: bootstrap skip 'no such column' errors, reorder ALTER TABLE before indexes
- fix: use go:embed all:dist to include files starting with _
- fix: add site column to LibSQL system tables for multi-tenant isolation

### Improvements
- feat: phase 3 cleanup — remove expr-lang, cut over to Lisp

### Documentation
- Document backend architecture
- feat: phase 3 — validation predicates, PostgreSQL dialect, documentation, cutover to Lisp


## v0.6.1-alpha — 2026-06-20

### Features
- Analytics Engine: EventBus + ORM hooks for real-time CDC (Phase 1)
- Analytics Query Engine + REST API + CLI backfill (Phase 2)
- Frontend Insights tab, charts, analytics navigation (Phase 3)
- WAL drain, retention cleanup, workflow tracking, query cache (Phases 4-5)
- WorkflowAction events, dashboard config, monthly rollup (backend completion)
- 32 tests for analytics package + nil-safety fixes

### Fixes
- MySQL DDL multi-statement, dimension []byte, NameGenQuery double-quoting

### Documentation
- DDL migration notes for db-compat skill
- Analytics API docs, architecture docs, CLAUDE.md package reference


## v0.6.0 — 2026-06-18

## v0.5.0-alpha.21 — 2026-06-18
### Fixes
- docs: update documentation for console-first workflow, env var config, and dialect fix

### Documentation
- docs: update documentation for console-first workflow, env var config, and dialect fix


## v0.5.0-alpha.20 — 2026-06-17
### Fixes
- fix: eliminate all hardcoded MySQL SQL for LibSQL compatibility


## v0.5.0-alpha.19 — 2026-06-17
### Fixes
- fix: workflow table schema mismatch and secret store LibSQL compatibility


## v0.5.0-alpha.18 — 2026-06-17
### Fixes
- fix: responsive sheet — bottom drawer on mobile, wider on desktop


## v0.5.0-alpha.17 — 2026-06-17

### Features
- feat: console site management — delete site, reset password, sheet-based editing
- feat: site domains persisted in DB + console edit UI
- feat: purge YAML — all site config from DB, env vars only
- feat: domains field in console create-site form + auto-detect request host
- fix: open fresh libsql connection in CreateSite via DB_DSN + add sqlite driver
- feat: wire db.Dialect into configstore and ORM for full LibSQL CRUD
- feat: wire db.Dialect into site/schema/cli/api for LibSQL support
- feat: StartupConfig — single source for all env vars, validated at boot

### Fixes
- fix: parseTime handles SQLite nanosecond+timezone format + visible edit icon
- fix: libsql connection pool — disable idle conns, set 25s lifetime
- fix: scan expires_at as string for SQLite TEXT column compatibility
- fix: replace MySQL-specific JSON_OBJECT and NOW() with portable SQL
- fix: health endpoint + path-based site routing for console-only mode
- fix: open fresh libsql connection in CreateSite via DB_DSN + add sqlite driver
- fix: reuse platform DB connection for LibSQL site creation
- fix: console site creation now respects platform DB type from env

### Documentation
- docs: update ARCHITECTURE.md and NETWORKING.md — remove YAML references
- v0.5.0-alpha: User management, secrets, libsql, console UI, docs


## v0.5.0 — 2026-06-16

### Features
- **User Management**: CRUD API + UI for site users. Admin can create, edit, disable, and reset passwords. All users are site-scoped.
- **Secrets/API Key Management**: Manage AI provider API keys via the UI (dropdown: OpenAI, DeepSeek, Anthropic). Values encrypted at rest (AES-256-GCM), never exposed by the API.
- **OpenAPI / Swagger**: Auto-generated OpenAPI 3.0 spec at `/api/openapi.json`, interactive Swagger UI at `/api/swagger-ui` linked from the workspace sidebar.
- **Console site creation**: Create new sites from the Console UI — no CLI needed.

### Fixes
- Fix: session role parsing — `CAST(? AS JSON)` in session creation to properly store roles as JSON array instead of string
- Fix: AuthGuard redirects for console paths (`/console/login`, `/console`) now recognized as public paths
- Fix: secrets page layout — added `p-8` padding to match other admin pages
- Fix: AI provider UX — replaced 3-card grid with dropdown selector for single-provider selection


## v0.4.0 — 2026-06-13
### Security
- v0.2.0: ORM optimization, YAML validation, security hardening, permission UX


## v0.3.0 — 2026-06-13

### Features
- feat: Administrator tab — visual doctype builder, permissions, workflows, versioning
- fix: create embed placeholder before go vet in CI

### Fixes
- fix: create embed placeholder before go vet in CI

### Documentation
- docs: update CLAUDE.md with release process and CI changes


## v0.2.0 — 2026-06-12

### Features
- feat: security hardening, computed fields, 10 SaaS configs, release automation
- Add GitHub Pages landing page
- Add AI skills guide for creating Kora config files
- Add Todo sample app (1 doctype, 5 fields, 3 YAML files)
- Make setup and serve depend on build (always build first)
- Add Makefile, update README, CLAUDE.md, and docs with make commands
- Add release workflow and CI/CD to CLAUDE.md
- Add CI/CD workflows and Go lint config

### Fixes
- Fix: remove Go 1.25 target from golangci-lint config (lint binary built with 1.24)

### Security
- feat: security hardening, computed fields, 10 SaaS configs, release automation

### Documentation
- Add AI skills guide for creating Kora config files
- Add Makefile, update README, CLAUDE.md, and docs with make commands

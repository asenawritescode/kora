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

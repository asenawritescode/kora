# Kora — Config-Driven Application Engine

Define your application — data model, permissions, workflows — in YAML. Kora gives you a database, REST API, React admin UI, and background jobs. No code generation.

## Quick Start

```bash
make dev                           # MySQL + build + setup + serve
```

Or step by step:

```bash
docker compose up -d mysql         # Start MySQL
make build                         # Build UI + Go binary
make setup                         # Setup airtime site
make serve                         # Start server on :8000
```

Open **http://localhost:8000/workspace** — login with `admin@airtime.local` / `kora123`.

### All Make Commands

```
make dev          Full setup: MySQL + build + setup + serve
make build        Build UI + Go binary
make serve        Start server
make restart      Kill old server + rebuild all + start fresh
make setup        Setup a site (override: SITE=fieldwork.local CONFIG=config/fieldwork/)
make test         Run Go tests
make lint         Run linters (Go + TypeScript)
make fmt          Format code (go fmt + prettier)
make release      Tag and push a release (TAG=v0.2.0)
make clean        Remove build artifacts
make help         Show all commands
```

Open **http://localhost:8000/workspace** — login with `admin@airtime.local` / `kora123`.

## Multi-Site

```
http://localhost:8000/s/airtime/workspace     → Airtime app
http://localhost:8000/s/fieldwork/workspace   → Fieldwork app
http://localhost:8000/console/login           → System console
```

No DNS or `/etc/hosts` needed. Path-based routing just works. For production, add real domains — Host-based routing takes over automatically.

## Administrator Panel

After login, the sidebar has an **Administrator** section for managing the data model entirely from the browser — no YAML files or CLI needed:

- **DocTypes** — Visual form builder with live YAML preview, collapsible field editors, Draft → Activate workflow
- **Permissions** — Role × DocType access matrix with inline editing
- **Workflows** — State machine editor (states, transitions, notifications) for submittable doctypes
- **Versions** — Config version history with diff view, rollback, and Draft activation

All pages are mobile-responsive — tables become stacked card layouts on small screens.

## Documentation

| Document | What it covers |
|---|---|
| [SETUP.md](docs/SETUP.md) | Prerequisites, quick start, multi-site setup, config management, production deployment, troubleshooting |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | System design, request flow, middleware, multi-tenancy, expression engine, schema migration, computed fields |
| [CONFIG.md](docs/CONFIG.md) | DocType/field reference, constraints, workflows, permissions, link fields, computed expressions, back-references |
| [API.md](docs/API.md) | REST API reference, auth, CRUD, workflow actions, system endpoints, error formats |
| [DECISIONS.md](docs/DECISIONS.md) | Architecture Decision Records — why React SPA, config-driven computed fields, path-based multi-site, Gin NoRoute, site-aware auth |
| [NETWORKING.md](docs/NETWORKING.md) | TLS, autocert, HTTP→HTTPS, rate limiting, security headers, CORS |

## Project Structure

```
kora/
├── cli/            # Cobra CLI: serve, setup, migrate, config
├── api/            # REST handlers, CRUD, system endpoints
├── auth/           # Session auth, CSRF, SystemGuard, SiteGuard
├── net/            # SiteRouter, TLS, security headers, rate limiting
├── doctype/        # DocType, Field, Registry, permissions, workflow, expressions
├── orm/            # Generic CRUD on map[string]any documents
├── schema/         # INFORMATION_SCHEMA diff → DDL migration
├── configstore/    # Config persistence (_kora_* tables)
├── workspace/      # React SPA serving (go:embed)
├── console/        # System admin console (server-rendered)
├── scheduler/      # Cron-style background jobs
├── site/           # Site config loading, DB connection
├── email/          # Email sending (mock for dev)
├── config/         # Sample app YAML configs (airtime, fieldwork)
├── ui/             # React 19 SPA (Vite + TanStack + shadcn/ui)
├── docs/           # Documentation
└── sites/          # Per-site config and files
```

## Tech Stack

| Layer | Technology |
|---|---|
| **Language** | Go 1.25 |
| **HTTP** | Gin, net/http |
| **Database** | MySQL 8.0 |
| **Expressions** | expr-lang/expr |
| **CLI** | Cobra |
| **TLS** | autocert (Let's Encrypt) |
| **Frontend** | React 19, TanStack Router/Query/Table/Form, shadcn/ui, Tailwind CSS v4 |
| **State** | Zustand, TanStack Query |
| **Validation** | Zod |
| **Delivery** | Single binary — everything via `go:embed` |

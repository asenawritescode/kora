# Kora — Config-Driven Application Engine

Define your application — data model, permissions, workflows — in YAML. Kora gives you a database, REST API, React admin UI, and background jobs. No code generation.

[![Docker Hub](https://img.shields.io/badge/docker-smitdockerhub%2Fkora-blue?logo=docker)](https://hub.docker.com/r/smitdockerhub/kora)
[![GitHub](https://img.shields.io/badge/github-asenawritescode%2Fkora-black?logo=github)](https://github.com/asenawritescode/kora)

## Quick Start (Docker)

```bash
docker run -d --name kora -p 8000:8000 \
  -e KORA_DB_TYPE=mysql \
  -e KORA_DB_HOST=127.0.0.1 \
  -e KORA_DB_USER=root \
  -e KORA_DB_PASSWORD=yourpassword \
  -e CONSOLE_EMAIL=admin@kora.local \
  -e CONSOLE_PASSWORD=admin123 \
  smitdockerhub/kora:latest
```

Open **http://localhost:8000/console** to create your first site. See [SETUP.md](docs/SETUP.md) for MySQL setup, multi-site, and production deployment.

## Prerequisites (Local Development)

You need three things installed before running Kora:

| Tool | Why | Version |
|------|-----|---------|
| **Go** | Backend server + ORM | 1.25+ |
| **Bun** | Frontend build (React SPA) | 1.x |
| **Docker** | MySQL database | 24+ |

### Install on Linux

```bash
# Go
wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc

# Bun
curl -fsSL https://bun.sh/install | bash

# Docker
sudo apt install docker.io docker-compose-v2    # Debian/Ubuntu
sudo systemctl start docker
```

### Install on macOS

```bash
# Go
brew install go

# Bun
brew install oven-sh/bun/bun

# Docker
brew install --cask docker
# Start Docker Desktop from Applications
```

### Install on Windows

| Tool | Download |
|------|----------|
| Go | https://go.dev/dl/ — run the `.msi` installer |
| Bun | https://bun.sh — run the `.exe` installer |
| Docker | https://www.docker.com/products/docker-desktop/ — run the installer, then start Docker Desktop |

Verify everything is installed:

```bash
go version      # go version go1.25.0 linux/amd64
bun --version   # 1.x.x
docker ps       # (should show no errors)
```

## Quick Start

```bash
# 1. Clone
git clone https://github.com/yourorg/kora.git && cd kora

# 2. Set MySQL root password (first time only)
echo 'MYSQL_ROOT_PASSWORD=kora123' > .env

# 3. Start MySQL
docker compose up -d mysql

# 4. Build, setup, and serve (one command)
make dev DB_PASS=kora123 ADMIN_PASS=kora123
```

Or step by step:

```bash
docker compose up -d mysql         # Start MySQL
make build                         # Build UI + Go binary
make setup DB_PASS=kora123 ADMIN_PASS=kora123   # Setup airtime site
make serve                         # Start server on :8000
```

### If you already have MySQL running

Skip Docker. Just pass your credentials:

```bash
make dev DB_USER=root DB_PASS=yourpassword
```

Open **http://localhost:8000/workspace** — login with `admin@airtime.local` / `kora123`.

### All Make Commands

```
make dev           MySQL + build + setup + serve (one command)
make build         Build UI (bun) + Go binary
make serve         Build + start server on :8000
make restart       Kill old server + rebuild all + start fresh
make setup         Setup a site from config YAML files
make test          Run Go tests (go test ./...)
make lint          Run linters (Go + TypeScript)
make fmt           Format code (go fmt + prettier)
make release       Tag, changelog, push release (TAG=v0.2.0)
make clean         Remove build artifacts
make help          Show all commands with descriptions
```

### Override Variables

```bash
make dev SITE=fieldwork.local CONFIG=config/fieldwork/   # Different site
make dev DB_USER=root DB_PASS=secret                      # MySQL credentials
make dev ADMIN_EMAIL=admin@test.com ADMIN_PASS=pass123    # Admin account
make build PORT=9000                                       # Custom port (serve target)
```

## Screenshot

![Kora dashboard with modules and AI chat](docs/images/dashboard-ai-chat.png)

## Features

- **AI Chat Assistant** — floating chat widget on every page. Create, find, and update records via natural language. Supports OpenAI, DeepSeek, and Anthropic. Multi-turn tool execution with finish_reason loop, stall detection, and context compaction. API keys configured in the UI at `/workspace/admin/secrets`.
- **AI Doctype Generator** — describe a form in plain English ("an Invoice with line items, customer link, computed totals, tax") — the AI generates the YAML, validates it, and saves it as Draft. A human reviews and activates.
- **YAML Strict Validation** — unknown keys rejected at parse time with line numbers and "did you mean?" suggestions
- **Visual Constraints Editor** — add min/max, regex, one_of, and other constraints via the form builder
- **Auto-Indenting YAML Editor** — Tab/Enter/Shift+Tab with context-aware indentation
- **Session Cache** — 30-second TTL in-memory cache reduces database load on every request
- **Config Import Safety** — transactional imports with field-level merge (no more DELETE+re-INSERT)
- **Site Isolation** — `kora_site` cookie validated against Host header, unknown hosts get 403
- **Console UI** — React SPA at `/console` for system administration: create sites, monitor health, manage all sites from one place
- **Swagger/OpenAPI** — auto-generated OpenAPI 3.0 spec at `/api/openapi.json`, interactive Swagger UI at `/api/swagger-ui`

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
- **Users** — User management with CRUD, role assignment, enable/disable, and admin-forced password reset
- **Secrets** — API key management for AI providers (OpenAI, DeepSeek, Anthropic) via dropdown UI. Values encrypted at rest (AES-256-GCM) and never exposed by the API
- **API Docs** — Full Swagger UI at `/api/swagger-ui` with OpenAPI 3.0 spec generated from the doctype registry

All pages are mobile-responsive — tables become stacked card layouts on small screens. All management is done from the browser — no CLI needed after initial setup.

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
├── cli/            # Cobra CLI: serve, setup, migrate, config, mcp, secret
├── api/            # REST handlers, CRUD, system endpoints, AI Chat, Users, Secrets
├── auth/           # Session auth, CSRF, SystemGuard, SiteGuard
├── net/            # SiteRouter, TLS, security headers, rate limiting
├── doctype/        # DocType, Field, Registry, permissions, workflow, expressions
├── orm/            # Generic CRUD on map[string]any documents
├── schema/         # INFORMATION_SCHEMA diff → DDL migration
├── configstore/    # Config persistence (_kora_* tables)
├── workspace/      # React SPA serving (go:embed)
├── console/        # System console (React SPA)
├── scheduler/      # Cron-style background jobs
├── site/           # Site config loading, DB connection
├── email/          # Email sending (mock for dev)
├── secret/         # Encrypted API key storage (AES-256-GCM)
├── mcp/            # Model Context Protocol server for Claude Desktop
├── config/         # Sample app YAML configs (airtime, fieldwork)
├── ui/             # React 19 SPA (Vite + TanStack + shadcn/ui) + AI Chat Widget
├── docs/           # Documentation
└── sites/          # Per-site config and files
```

## Tech Stack

| Layer | Technology |
|---|---|
| **Language** | Go 1.25 |
| **HTTP** | Gin, net/http |
| **Database** | MySQL 8.0, MariaDB, LibSQL (Turso-compatible) |
| **AI / LLM** | OpenAI, DeepSeek V4, Anthropic Claude (multi-provider, OpenAI-compatible API) |
| **Tool Protocol** | MCP (Model Context Protocol) for Claude Desktop integration |
| **Expressions** | expr-lang/expr |
| **CLI** | Cobra |
| **TLS** | autocert (Let's Encrypt) |
| **Frontend** | React 19, TanStack Router/Query/Table/Form, shadcn/ui, Tailwind CSS v4 |
| **State** | Zustand, TanStack Query |
| **Validation** | Zod |
| **Delivery** | Single binary — everything via `go:embed`, ~63MB |

## Docker

```
docker pull smitdockerhub/kora:latest
```

Multi-arch ready. Supports MySQL + LibSQL. Pure Go, no CGO. Version injected at build time — check with `curl /api/ping`.

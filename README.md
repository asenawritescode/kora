# Kora — Config-Driven Application Engine

Describe your application in YAML. Kora gives you a database, REST API, React admin UI, and background jobs. No code generation.

[![Docker Hub](https://img.shields.io/badge/docker-smitdockerhub%2Fkora-blue?logo=docker)](https://hub.docker.com/r/smitdockerhub/kora)
[![GitHub](https://img.shields.io/badge/github-asenawritescode%2Fkora-black?logo=github)](https://github.com/asenawritescode/kora)

## Quick Start (Docker)

```bash
# MySQL
docker run -d --name kora -p 8000:8000 \
  -e KORA_DB_TYPE=mysql \
  -e KORA_DB_HOST=127.0.0.1 \
  -e KORA_DB_USER=root \
  -e KORA_DB_PASSWORD=yourpassword \
  -e CONSOLE_EMAIL=admin@kora.local \
  -e CONSOLE_PASSWORD=admin123 \
  smitdockerhub/kora:latest

# LibSQL (remote)
docker run -d --name kora -p 8000:8000 \
  -e KORA_DB_TYPE=libsql \
  -e DB_DSN=http://user:pass@libsql-host:8080 \
  -e CONSOLE_EMAIL=admin@kora.local \
  -e CONSOLE_PASSWORD=admin123 \
  smitdockerhub/kora:latest
```

Open **http://localhost:8000/console** → create your first site. All config via env vars, all data in the database. No YAML files to lose.

## Local Development

```bash
git clone https://github.com/asenawritescode/kora.git && cd kora

# With local MySQL
docker compose up -d mysql
make dev DB_PASS=kora123 ADMIN_PASS=kora123

# Or with env vars directly
make build
KORA_DB_TYPE=mysql KORA_DB_HOST=127.0.0.1 KORA_DB_USER=root KORA_DB_PASSWORD=kora123 \
  CONSOLE_EMAIL=admin@kora.local CONSOLE_PASSWORD=kora123 \
  ./kora serve --port 8000
```

| Command | What it does |
|---------|-------------|
| `make build` | Build UI (bun) + Go binary |
| `make serve` | Build + start server |
| `make test` | Run Go tests (311 tests, 19/19 packages) |
| `make lint` | Run linters (Go + TypeScript) |
| `make fmt` | Format code |
| `make help` | Show all commands |

## Features

- **AI Chat Assistant** — floating chat widget. Create, find, update records via natural language. OpenAI, DeepSeek, Anthropic. Multi-turn tool execution. Keys configured at `/workspace/admin/secrets`.
- **AI Doctype Generator** — describe a form in plain English, AI generates validated YAML saved as Draft.
- **Config-Driven Admin** — forms, lists, filters, workflows rendered from doctype definitions. No per-doctype code.
- **Multi-Site** — path-based (`/s/:site/workspace`) by default, with host-based routing only for domains you explicitly configure. Sites created from console UI are persisted in DB.
- **Multi-Database** — MySQL, MariaDB, or remote LibSQL. SQL dialect abstraction handles all differences.
- **Console UI** — `/console` for system admin: create/edit sites, view health, manage all sites.
- **Self-Service Onboarding** — public site creation at `/onboard`. Users create their own sites with admin accounts. Rate-limited (3/hr/IP).
- **Shared AI Keys** — superadmins can set global AI provider keys so new sites get AI chat immediately. Toggle with `KORA_SHARED_AI_ENABLED`.
- **Swagger/OpenAPI** — auto-generated API docs at `/api/swagger-ui`.
- **Mobile Responsive** — tables become stacked cards. No horizontal scroll anywhere.
- **Marketing Website** — landing page, docs, examples, and blog at [kora.mradiafrica.com](https://kora.mradiafrica.com).
- **Extensibility** — JS runtime, event hooks, webhook extensions, custom API methods, workflow actions, scheduled scripts, computed fields
- **MCP Server** — Model Context Protocol server for Claude Desktop, Cursor, and other AI tool integration
- **Go SDK + TypeScript SDK** — official SDKs for building custom extensions, integrations, and plugins
- **API Versioning** — stable `/api/v1/` routes with backward compatibility guarantees

## Configuration

All config via environment variables. No YAML config files needed.

| Variable | Default | Description |
|----------|---------|-------------|
| `KORA_DB_TYPE` | `mysql` | `mysql` or `libsql` |
| `KORA_DB_HOST` | `127.0.0.1` | DB host (or HTTP URL for LibSQL) |
| `KORA_DB_USER` | — | DB user |
| `KORA_DB_PASSWORD` | — | DB password |
| `DB_DSN` | — | Full connection string (overrides host/user/password) |
| `KORA_HTTP_PORT` | `8000` | Server port |
| `CONSOLE_EMAIL` | `admin@kora.local` | Console admin email |
| `CONSOLE_PASSWORD` | `kora123` | Console admin password |
| `KORA_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `KORA_SESSION_HOURS` | `72` | Session lifetime in hours |
| `KORA_HOST` | — | Public app hostname (e.g., `app.kora.mradiafrica.com`) |
| `KORA_SHARED_AI_ENABLED` | `false` | Enable shared AI provider keys for all sites |
| `KORA_SHARED_OPENAI_API_KEY` | — | Shared OpenAI key (fallback when site has none) |
| `KORA_SHARED_DEEPSEEK_API_KEY` | — | Shared DeepSeek key |
| `KORA_SHARED_ANTHROPIC_API_KEY` | — | Shared Anthropic key |
| `KORA_SCRIPTS_ENABLED` | `false` | Enable JS script engine for extensions |
| `KORA_SCRIPTS_MAX_RAM` | `64` | Max RAM per script (MB) |
| `KORA_ANALYTICS` | `false` | Enable analytics event bus and rollup tables |
| `KORA_DB_PORT` | `3306` | Database port (MySQL) |
| `KORA_LOG_FORMAT` | `text` | Log format: `text` or `json` |
| `KORA_CSRF_SECURE` | `true` | Set secure flag on CSRF cookies |
| `KORA_RATE_LIMIT` | `100` | Max requests per minute per IP |
| `KORA_RATE_BURST` | `200` | Rate limiter burst allowance |

## SDK Quick Start

Add Kora to your Go project:

```go
import "github.com/asenawritescode/kora/sdk"

func main() {
    client := sdk.NewClient(sdk.Config{
        BaseURL: "http://localhost:8000/api/v1",
        APIKey: "your-api-key",
    })
    // List documents
    docs, err := client.GetList("Customer", map[string]string{})
    // Get single document
    doc, err := client.GetDoc("Customer", "CUST-0001")
    // Create document
    err := client.Insert("Customer", map[string]any{"name": "Acme Corp"})
}
```

Or in TypeScript:

```typescript
import { KoraClient } from "@kora/sdk"

const client = new KoraClient({
  baseURL: "/api/v1",
  csrfToken: await getCSRFToken(),
})
// List documents
const customers = await client.getList("Customer", {})
// Get single document
const customer = await client.getDoc("Customer", "CUST-0001")
// Create document
await client.insert("Customer", { name: "Acme Corp" })
```

## Multi-Site

```
http://host/s/airtime/workspace     → Airtime workspace (path-based, no DNS needed)
http://host/s/fieldwork/workspace   → Fieldwork workspace
http://host/console                 → System console
```

Sites created via console are persisted in `_kora_site_registry` for startup discovery, and still keep tenant-local config history in `_kora_config_version`. They survive container redeploys and can be hot-added immediately after onboarding.

For path-based access, set `KORA_HOST` to the public app hostname. That host is allowed for session and cookie flow, while tenant routing stays on `/s/:site/...` until you add real tenant domains.

## Administrator Panel

Nine admin views — all config-driven, all mobile-responsive:

- **DocTypes** — visual form builder + live YAML preview
- **Permissions** — role × doctype matrix, inline editing
- **Workflows** — state machine editor
- **Versions** — config version history, diff, rollback
- **Scripts** — JS script editor, test runner, console logs
- **Extensions** — webhook endpoints, custom API methods, event hooks
- **Users** — CRUD, roles, enable/disable, password reset
- **Secrets** — AI provider keys (encrypted at rest, AES-256-GCM)
- **API Docs** — Swagger UI at `/api/swagger-ui`

## Documentation

| Document | What it covers |
|---|---|
| [SETUP.md](docs/SETUP.md) | Prerequisites, Docker/Dev setup, env vars, multi-site, production |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | System design, request flow, middleware, multi-tenancy, SQL dialect |
| [CONFIG.md](docs/CONFIG.md) | DocType/field reference, constraints, workflows, permissions |
| [API.md](docs/API.md) | REST API reference, auth, CRUD, system endpoints |
| [DECISIONS.md](docs/DECISIONS.md) | Architecture Decision Records |
| [NETWORKING.md](docs/NETWORKING.md) | TLS, autocert, rate limiting, security headers, CORS |
| [Plugin Architecture](docs/plugin-architecture.md) | Extension & webhook system design (draft) |
| [Extensibility Plan](docs/extensibility-plan.md) | Script engine, event hooks, custom API methods, and webhook extensions |
| [Process Isolation](docs/extensibility-process-isolation.md) | Script sandboxing, security isolation, and resource limits |
| [Security Audit](docs/extensibility-security-audit.md) | Security review and threat model of the extensibility system |
| [Cloud Architecture](docs/kora-cloud-architecture.md) | Kora Cloud deployment, multi-tenant SaaS, and scaling architecture |

## Tech Stack

| Layer | Technology |
|---|---|
| **Language** | Go 1.25 |
| **HTTP** | Gin, net/http |
| **Database** | MySQL 8.0, MariaDB, LibSQL (remote HTTP) |
| **AI / LLM** | OpenAI, DeepSeek V4, Anthropic Claude |
| **Frontend** | React 19, TanStack Router/Query/Table/Form, shadcn/ui, Tailwind CSS v4 |
| **State** | Zustand, TanStack Query |
| **Delivery** | Single binary — everything via `go:embed`, ~30MB, pure Go, no CGO |

## Docker

```
docker pull smitdockerhub/kora:latest
```

Pure Go, no CGO, ~30MB. Supports MySQL + LibSQL. Version injected at build time — check with `curl /api/v1/ping`.

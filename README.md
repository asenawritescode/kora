# Kigae — Config-Driven Application Engine

Kigae is the open-source modular business software project and application engine. It currently provides a DocType-centric runtime with a kernel, typed contracts, workflow runtime, AI/MCP surfaces, and a React admin UI. Some planned capabilities remain in progress; see the implementation and tests for the current behavior.

**Kigae Cloud** is the managed hosting offering, and **Senumi** is the company and sponsor. Some compatibility-sensitive code identifiers still use the former Kora spelling; see [the brand architecture](docs/brand-architecture.md) before copying technical names from this README.

[![Docker Hub](https://img.shields.io/badge/docker-smitdockerhub%2Fkora-blue?logo=docker)](https://hub.docker.com/r/smitdockerhub/kora)
[![GitHub](https://img.shields.io/badge/github-asenawritescode%2Fkora-black?logo=github)](https://github.com/asenawritescode/kora)

## Architecture target

The [architecture specification](docs/architecture/target-state-spec.md) defines the planned module boundaries, runtime ownership, database budgets, requests and workflows, consumer contracts, implementation order and deployment gates. It describes a target; current support remains governed by the capability registry and tested implementation.

See the [database support matrix](docs/architecture/database-support-matrix.md) for tested backend evidence and its current limits.

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

Open **http://localhost:8000/console** → create your first site. Runtime configuration is loaded from environment and persisted site metadata; application configuration is still DocType/config-pack driven rather than the RFC's full generic application-definition model.

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
| `make test` | Run Go tests (18 packages) |
| `make lint` | Run linters (Go + TypeScript) |
| `make fmt` | Format code |
| `make help` | Show all commands |

## Features

- **AI Chat Assistant** — floating, page-aware chat with durable transcripts, tracked runs, and catalog-backed tool execution at `/api/v1/ai/chat`. A site or enabled shared provider key is required. Marked experimental in the capability registry.
- **AI Doctype Generator** — AI-assisted draft creation for DocType YAML.
- **Config-Driven Admin** — forms, lists, filters, workflows, and page manifests are rendered from configuration, but the runtime remains DocType-centric rather than fully generic-resource-based.
- **Multi-Site** — path-based (`/s/:site/workspace`) by default, with host-based routing only for configured domains. Sites created from the console are persisted in the database.
- **Multi-Database** — MySQL is the primary self-host backend. PostgreSQL and remote LibSQL have dialect and package-level integration coverage with remaining gaps; MariaDB is not separately validated. See the [database support matrix](docs/architecture/database-support-matrix.md).
- **Console UI** — `/console` for system admin: create/edit sites, view health, manage all sites.
- **Self-Service Onboarding** — public site creation at `/onboard` when `KORA_CONSOLE_ONBOARDING_ENABLED=true`. Users create their own sites with admin accounts. Rate-limited (3/hr/IP).
- **Cloud-managed AI** — when connected to Kigae Cloud, workspace chat uses the provider configured by Cloud. Per-site provider keys remain available for standalone deployments.
- **Swagger/OpenAPI** — auto-generated API docs at `/api/swagger-ui`.
- **Mobile Responsive** — tables become stacked cards in the current React UI.
- **Extensibility** — trigger-based automations, a permission-gated JS runtime for document hooks and custom API methods, webhook extensions, scheduled scripts, and computed fields. Automation effects use registered Engine actions.
- **MCP Server** — Model Context Protocol server for Claude Desktop and similar tooling. Capability is experimental, and tenant tool execution is still projection-based rather than a full standalone agent runtime.
- **Go SDK + TypeScript SDK** — SDKs for integrations and extensions.
- **API Versioning** — `/api/v1/` routes exist for the supported surface.
- **Analytics** — automatic per-doctype metrics with rollups and time-series queries.

## Capability Status

The public capability registry lives in `contract.BaselineCapabilities()`. It is
the source of truth for what the current codebase supports, what remains
experimental, and what is still planned.

| Capability | Status |
|---|---|
| `contract.event_envelope` | supported |
| `contract.command_envelope` | supported |
| `contract.actor_context` | experimental |
| `provider.nats` | supported |
| `outbox.transactional` | supported |
| `auth.oidc` | supported |
| `ai.chat` | experimental |
| `ai.mcp` | experimental |
| `offline.sync` | supported |

## Current Architecture Truth

- The current engine is DocType-centric, not a fully generic resource engine.
- The current frontend is a React SPA with an embedded page-manifest runtime, not an ES-module/Franken UI runtime.
- The current kernel, contract surface, workflow runtime, and AI/MCP surfaces are real, but still evolving.
- PostgreSQL and LibSQL coverage is partial; the [database support matrix](docs/architecture/database-support-matrix.md) records current evidence and open validation gates.

## Configuration

Runtime config comes from environment variables plus site metadata in the database. Application configuration still uses YAML/config packs for DocType-driven runtime state.

| Variable | Default | Description |
|----------|---------|-------------|
| `KORA_DB_TYPE` | `mysql` | `mysql`, `postgres`, or `libsql`; backend evidence and limits are in the [database support matrix](docs/architecture/database-support-matrix.md) |
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
| `KORA_CLOUD_AI_GATEWAY_URL` | — | Private Kigae Cloud AI gateway base URL (for example `http://127.0.0.1:8081/internal/engine/ai`) |
| `KORA_CLOUD_AI_GATEWAY_TOKEN` | — | Private Engine-to-Cloud bearer token; configure the matching `KORA_CLOUD_ENGINE_AI_TOKEN` in Cloud |
| `KORA_CLOUD_AI_GATEWAY_MODEL` | `cloud-managed` | Model label sent to Cloud; Cloud selects the actual provider model |
| `KORA_SCRIPTS_ENABLED` | `false` | Enable JS script engine for extensions |
| `KORA_OUTBOX` | `true` | Enable transactional kernel events; set `false` only if reports and event-driven integrations are intentionally disabled |
| `KORA_NATS_STREAM_MAX_BYTES` | `1073741824` | Maximum bytes retained by the JetStream event stream (1 GiB default; requires NATS) |
| `KORA_NATS_STREAM_MAX_MESSAGES` | `1000000` | Maximum messages retained by the JetStream event stream (requires NATS) |
| `KORA_NATS_SUBJECT_PREFIX` | `kora` | Event subject tree `<prefix>.>` captured by the event stream (requires NATS) |
| `KORA_NATS_DEAD_LETTER` | `<prefix>_deadletter` | Literal subject where exhausted deliveries are stored. The event stream captures it, but it must lie outside every consumer filter (the default consumer filter is `<prefix>.>`); startup rejects a subject the consumer filter matches. Before this default, it was `kora.deadletter` (requires NATS) |
| `KORA_DB_PORT` | `3306` | Database port (MySQL) |
| `KORA_RELOAD_TOKEN` | — | Bearer token for `/_kora/admin/reload-site` endpoint |
| `KORA_SCRIPTS_HTTP_ALLOWLIST` | — | Comma-separated domains allowed for script HTTP requests |
| `KORA_LOG_FORMAT` | `text` | Log format: `text` or `json` |
| `KORA_CSRF_SECURE` | `true` | Set secure flag on CSRF cookies |
| `KORA_RATE_LIMIT` | `100` | Max requests per minute per IP |
| `KORA_RATE_BURST` | `200` | Rate limiter burst allowance |
| `KORA_CONSOLE_ONBOARDING_ENABLED` | `false` | Enable public console onboarding at `/onboard` |

### Environment variable names (`ZANA_*` and `KORA_*`)

The product is being renamed to Zana. Every variable above is also accepted with a `ZANA_` prefix, and `ZANA_*` is the preferred spelling: `ZANA_DB_HOST` is the same setting as `KORA_DB_HOST`. The mapping is by prefix only, so `KORA_CLOUD_KORA_RELOAD_URL` becomes `ZANA_CLOUD_KORA_RELOAD_URL`.

- `KORA_*` variables are deprecated but keep working for one release, and support is planned to be removed in the release after next. When any are in use, startup logs one line listing their names (never their values).
- If both spellings of a variable are set to different values, `ZANA_*` wins and startup logs one warning naming the variable (never the values).
- An empty value counts as unset, so an empty `ZANA_X` does not blank a set `KORA_X`.

## SDK Quick Start

Add Kigae to your Go project:

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

Ten admin views — all config-driven, all mobile-responsive:

- **DocTypes** — visual form builder + live YAML preview
- **Permissions** — role × doctype matrix, inline editing
- **Automations** — record-triggered workflows and status transitions
- **Versions** — config version history, diff, rollback
- **Users** — CRUD, roles, enable/disable, password reset
- **Scripts** — JS script editor, test runner, console logs
- **Extensions** — webhook endpoints, custom API methods, event hooks
- **Secrets** — AI provider keys (encrypted at rest, AES-256-GCM)
- **Analytics** — auto-generated metrics, daily/monthly rollups, time-series charts
- **API Docs** — Swagger UI at `/api/swagger-ui`

## Documentation

| Document | What it covers |
|---|---|
| [CLAUDE.md](CLAUDE.md) | Full architecture guide, build & run commands, package map |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Development workflow, project structure, PR guidelines |
| [skills/create-config.md](skills/create-config.md) | YAML config reference — doctypes, fields, permissions, roles; Engine v2 workflow JSON and Automations UI |

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

# Kora — Setup Guide

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

Open **http://localhost:8000/console** — login with `admin@kora.local` / `admin123` — create your first site.

## Environment Variables

All configuration via env vars. No YAML config files needed.

### Required

| Variable | Description |
|----------|-------------|
| `KORA_DB_TYPE` | `mysql` or `libsql` |
| `CONSOLE_EMAIL` | Console admin email |
| `CONSOLE_PASSWORD` | Console admin password |

### Database

| Variable | Default | Description |
|----------|---------|-------------|
| `KORA_DB_TYPE` | `mysql` | `mysql` or `libsql` |
| `KORA_DB_HOST` | `127.0.0.1` | DB host, or HTTP URL for LibSQL |
| `KORA_DB_PORT` | `3306` | DB port |
| `KORA_DB_USER` | — | DB user |
| `KORA_DB_PASSWORD` | — | DB password |
| `DB_DSN` | — | Full connection string (overrides host/user/password) |

For LibSQL, `DB_DSN` is the recommended approach — it passes auth credentials in the URL:
```
DB_DSN=http://user:password@libsql-host:8080
```

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `KORA_HTTP_PORT` | `8000` | Server port |
| `KORA_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `KORA_LOG_FORMAT` | `json` | `json` or `text` |
| `KORA_SESSION_HOURS` | `72` | Session lifetime in hours |
| `KORA_APP_NAME` | `Kora` | App branding name |
| `KORA_PRIMARY_COLOR` | `#000000` | Primary UI color |

## Local Development

### Prerequisites

| Tool | Why | Version |
|------|-----|---------|
| **Go** | Backend + ORM | 1.25+ |
| **Bun** | Frontend build (React SPA) | 1.x |
| **Docker** | MySQL database (optional) | 24+ |

### Build & Run

```bash
git clone https://github.com/asenawritescode/kora.git && cd kora

# Build UI + Go binary
make build

# Run with MySQL
KORA_DB_TYPE=mysql KORA_DB_HOST=127.0.0.1 KORA_DB_USER=root KORA_DB_PASSWORD=kora123 \
  CONSOLE_EMAIL=admin@kora.local CONSOLE_PASSWORD=kora123 \
  ./kora serve --port 8000
```

### Make Commands

| Command | What it does |
|---------|-------------|
| `make build` | Build UI (bun) + Go binary |
| `make serve` | Build + start server |
| `make test` | Run Go tests |
| `make lint` | Run linters |
| `make fmt` | Format code |
| `make help` | Show all commands |

## Multi-Site Access

Kora supports three access methods:

```
1. Path-based (always works, zero config):
   localhost:8000/s/airtime/workspace
   localhost:8000/s/fieldwork/workspace

2. Host-based (production, needs DNS):
   airtime.myapp.com → airtime site

3. Console (system admin):
   http://host/console
```

**How path-based routing works:**
1. Visit `/s/:site/workspace` → sets `kora_site` cookie
2. All subsequent API calls read the cookie → route to correct site DB
3. No DNS or `/etc/hosts` needed

## Creating Sites

Via console UI:
1. Login to `/console`
2. Click "Create Site" 
3. Enter hostname (e.g., `myapp.local`), admin email, admin password
4. Optionally add domains (e.g., `myapp.example.com`)
5. Click Create

Sites are persisted in the database (`_kora_config_version`) — they survive container redeploys.

To update site domains: hover the site row in the console, click the pencil icon, edit domains, press Enter.

## Console

| URL | Purpose |
|-----|---------|
| `/console` | Dashboard — sites, health |
| `/console/` | Site list + create form |

Credentials from env vars: `CONSOLE_EMAIL` / `CONSOLE_PASSWORD`.

## AI Chat Setup

Configure via the workspace UI:
1. Login to workspace as Administrator
2. Navigate to **Administrator → Secrets**
3. Select provider (OpenAI, DeepSeek, Anthropic)
4. Enter API key → Save

Or via CLI:
```bash
./kora secret set --site myapp.local --key deepseek_api_key --value sk-...
```

## Docker Production Deployment

```bash
docker run -d --name kora -p 80:8000 \
  -e KORA_DB_TYPE=libsql \
  -e DB_DSN=http://user:pass@your-libsql:8080 \
  -e CONSOLE_EMAIL=admin@yourdomain.com \
  -e CONSOLE_PASSWORD=yourpassword \
  -v kora-data:/data \
  smitdockerhub/kora:latest
```

The binary is ~30MB, pure Go (CGO_ENABLED=0), contains React SPA + Console UI via `go:embed`.

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `site_not_found` after deploy | Sites lost from memory | New deploys auto-discover from DB — redeploy latest |
| `site_access_denied` | Host not in site domains | Edit domains from console |
| `401 BasicRejected` | LibSQL auth not passed | Use `DB_DSN` with credentials in URL |
| `Stream handle expired` | LibSQL idle connection timeout | Fixed in latest — deploys `MaxIdleConns=0` |
| `no sites found` on startup | No sites in DB | Create first site via `/console` |

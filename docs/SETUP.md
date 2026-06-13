# Kora — Setup Guide

## Prerequisites

| Tool | Required | Install |
|------|----------|---------|
| **Go** 1.25+ | Backend + ORM | [go.dev/dl](https://go.dev/dl/) |
| **Bun** 1.x | Frontend build | [bun.sh](https://bun.sh) |
| **Docker** + Compose | MySQL 8.0 | [docker.com](https://www.docker.com/products/docker-desktop/) |

**Linux:**
```bash
# Go: https://go.dev/dl/
wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc

# Bun: https://bun.sh
curl -fsSL https://bun.sh/install | bash

# Docker: Docker Engine + Compose plugin
# https://docs.docker.com/engine/install/ubuntu/
```

**macOS:**
```bash
brew install go oven-sh/bun/bun
brew install --cask docker      # Docker Desktop
```

**Windows:** Download and run the installers from the links above.

Verify:
```bash
go version      # go version go1.25.0
bun --version   # 1.x.x
docker ps       # no errors
```

## Quick Start (5 minutes)

### 1. Set MySQL password (first time only)

```bash
# Create .env with the root password Docker will use:
echo 'MYSQL_ROOT_PASSWORD=kora123' > .env
```

### 2. Start MySQL

```bash
docker compose up -d mysql
# Wait 15-30 seconds for MySQL to be ready.
# Check: docker compose ps mysql  (should show "(healthy)")
```

### 3. One-command build + setup + serve

```bash
make dev DB_PASS=kora123 ADMIN_PASS=kora123
```

This runs: MySQL health check → `make build` → `./kora setup` → `./kora serve`.

### If you already have MySQL

Skip Docker. Just pass your credentials:

```bash
make dev DB_USER=root DB_PASS=yourpassword
```

Or step by step:

```bash
# Build the UI (one-time, or after frontend changes)
cd ui && bun install && bun run build && cp -r dist ../workspace/dist && cd ..

# Build Go + setup + serve
make build-go
./kora setup --site airtime.local --path config/airtime/ \
  --db-user root --db-pass yourpassword \
  --admin-email admin@airtime.local --admin-password kora123
./kora serve --port 8000
```

### 3. One-Command Setup

```bash
go run . setup \
  --site airtime.local \
  --path config/airtime/ \
  --db-host 127.0.0.1 \
  --db-port 3306 \
  --db-user root \
  --db-pass kora123 \
  --admin-email admin@airtime.local \
  --admin-password admin123
```

Add a second site:

```bash
go run . setup \
  --site fieldwork.local \
  --path config/fieldwork/ \
  --db-host 127.0.0.1 \
  --db-port 3306 \
  --db-user root \
  --db-pass kora123 \
  --admin-email admin@fieldwork.local \
  --admin-password admin123
```

This single command:
1. Creates the MySQL database
2. Writes site configuration
3. Bootstraps all system tables (`_kora_*`)
4. Parses YAML config files (doctypes, roles, permissions, workflows)
5. Saves config to database
6. Creates application tables
7. Creates an admin user
8. Records the initial config version

**Zero manual SQL. Zero manual steps.**

### 4. Start the Server

```bash
go run . serve --port 8000
```

### 5. Open the UI

```
http://localhost:8000/workspace           → Default site (first loaded)
http://localhost:8000/s/airtime/workspace → Airtime app
http://localhost:8000/s/fieldwork/workspace → Fieldwork app
http://localhost:8000/console/login       → System console
```

No `/etc/hosts` or DNS needed for local development. The `/s/:site/` path prefix selects the site.

### 6. Console Login

```
URL:  http://localhost:8000/console/login
Email: admin@kora.local
Password: admin123
```

Credentials are in `system_credentials.yaml` (password is bcrypt-hashed on startup).

---

## Multi-Site Access (No DNS Required)

Kora supports three access methods, all automatic:

```
1. Path-based (always works, zero config):
   localhost:8000/s/airtime/workspace
   localhost:8000/s/fieldwork/workspace

2. Host-based (production, needs DNS):
   airtime.myapp.com → airtime site
   fieldwork.myapp.com → fieldwork site

3. Default fallback (localhost / IP):
   localhost:8000/workspace → first site loaded
```

**How it works:**
1. Path-based (`/s/:site/...`) sets a `kora_site` cookie with the site name
2. All subsequent API calls read the cookie and route to the correct site's database
3. Host-based routing reads the `Host` header and matches configured domains
4. Both methods coexist — path-based takes priority

### Configuring Domains

Each site has a `site_config.yaml`:

```yaml
# sites/airtime/site_config.yaml
hostname: airtime.local
domains:
  - airtime.local
  - www.airtime.local
```

For production with real domains:
```yaml
hostname: airtime.myapp.com
domains:
  - airtime.myapp.com
  - www.airtime.myapp.com
```

### Switching Sites

In the browser:
- Visit `/s/airtime/workspace` → Airtime app (login with `admin@airtime.local`)
- Visit `/s/fieldwork/workspace` → Fieldwork app (login with `admin@fieldwork.local`)
- Each sets a `kora_site` cookie that persists for 24 hours
- Credentials are **per-site** — each site has its own `_kora_user` table

**URLs stay consistent** — the SPA preserves the `/s/:site/` prefix:
- Login at `/s/fieldwork/workspace/auth/login` (not `/workspace/auth/login`)
- Dashboard at `/s/fieldwork/workspace` (not `/workspace`)
- Logout redirects to `/s/fieldwork/workspace/auth/login`

In the API:
```bash
# Airtime
curl -H "Host: airtime.local" http://localhost:8000/api/resource/Customer
# Or path-based:
curl http://localhost:8000/s/airtime/api/resource/Customer

# Fieldwork  
curl -H "Host: fieldwork.local" http://localhost:8000/api/resource/Work%20Order
# Or path-based:
curl http://localhost:8000/s/fieldwork/api/resource/Work%20Order
```

### Site Credentials

| Site | Login Email | Workspace URL |
|---|---|---|
| Airtime | `admin@airtime.local` | `/s/airtime/workspace` |
| Fieldwork | `admin@fieldwork.local` | `/s/fieldwork/workspace` |

**Console** (system admin, not per-site):
| | |
|---|---|
| URL | `/console/login` |
| Email | `admin@kora.local` |
| Password | `admin123` |
| Config file | `system_credentials.yaml` |

---

## Config Management

### Import / Update Config

Edit YAML files, then re-import:

```bash
kora config import --site airtime.local --path config/airtime/
```

This updates the doctypes, fields, permissions, and workflows in the database. The schema migrator runs automatically on next server start.

### Export Config

```bash
kora config export --site airtime.local --path backup/
```

### Version History

```bash
kora config versions --site airtime.local
```

### Diff Versions

```bash
kora config diff --site airtime.local <from-id> <to-id>
```

### Rollback

```bash
kora config rollback --site airtime.local 3
```

---

## Production Deployment

### Single Binary

```bash
cd ui && bun run build && cp -r dist ../workspace/dist && cd ..
go build -o kora .
./kora serve --port 443
```

One binary contains: Go backend + React SPA + Console UI + embedded assets.

### Common Site Configuration

`common_site_config.yaml` controls global settings across all sites. All fields have sensible defaults — only override what you need.

```yaml
# Infrastructure
redis_url: redis://localhost:6379/0
db_host: 127.0.0.1
http_port: 8000
workers: 4

# Logging
log_level: info          # debug | info | warn | error
log_format: json         # json | text

# Branding (used in sidebar, navigation API, console)
app_name: Kora
version: "0.1.0"
primary_color: "#2563eb"

# Session & security
session_lifetime_hours: 24
csrf_secure: false       # Set true in production with TLS
admin_role: Administrator

# Rate limiting (per user)
rate_limit_rps: 100
rate_limit_burst: 20

# Database connection pool
db_max_open_conns: 25
db_max_idle_conns: 5

# API pagination limits
api_default_limit: 50
api_max_limit: 500

# HTTP server timeouts (seconds)
read_timeout_secs: 30
write_timeout_secs: 30
idle_timeout_secs: 120

# TLS (Let's Encrypt)
tls_mode: off            # off | auto | manual
tls_email: ""            # Required for Let's Encrypt
```

Kora automatically provisions Let's Encrypt certs for all configured domains across all sites. Requires ports 80 and 443 accessible from the internet.

### VPS / Dokploy Deployment

```
DNS: *.myapp.com → VPS IP

./kora serve --port 443
  ├─ TLS: autocert for all domains
  ├─ Host: airtime.myapp.com → airtime site
  └─ Host: fieldwork.myapp.com → fieldwork site
```

No reverse proxy (Nginx/Caddy) needed. Kora handles TLS, HTTP→HTTPS redirect, and multi-site routing internally.

### Docker

```dockerfile
FROM node:20-alpine AS ui
WORKDIR /app/ui
COPY ui/ .
RUN npm install && npm run build

FROM golang:1.25 AS go
WORKDIR /app
COPY . .
COPY --from=ui /app/ui/dist ./workspace/dist
RUN CGO_ENABLED=0 go build -o kora .

FROM alpine:3.19
COPY --from=go /app/kora /usr/local/bin/kora
EXPOSE 8000 443
ENTRYPOINT ["kora", "serve"]
```

---

## System Console

The console at `/console` is a separate server-rendered UI for system administrators. Auth uses `system_credentials.yaml` (separate from per-site `_kora_user`).

| Page | Purpose |
|---|---|
| `/console/` | Dashboard — site count, uptime, version |
| `/console/sites` | Site list — name, database, status |
| `/console/sites/:name` | Site detail — config, domains, doctypes |
| `/console/health` | System health — runtime metrics |

---

## Verification Checklist

| Check | Command | Expected |
|---|---|---|
| Server running | `curl localhost:8000/api/ping` | `{"message":"pong"}` |
| SPA serves | `curl localhost:8000/workspace` | HTML with React bundle |
| Path-based Airtime | `curl localhost:8000/s/airtime/workspace` | 200, HTML |
| Path-based Fieldwork | `curl localhost:8000/s/fieldwork/workspace` | 200, HTML |
| Assets load | `curl localhost:8000/assets/index-*.js` | 200, `text/javascript` |
| Console login | `curl localhost:8000/console/login` | 200, login form |
| Login works | `curl -X POST .../api/auth/login` | Returns session cookie |
| CRUD works | Create → List → Get → Update → Delete | All return 200/201 |
| Computed fields | Create Order with items | line_total, subtotal, total auto-calculate |
| Linked field | Select Product in Order Item | unit_price auto-populates from selling_price |
| Workflow works | Submit Order → Confirm → Process | State transitions work |
| Permissions enforced | Access without auth | Returns 401 |

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `unknown driver "mysql"` | MySQL driver not imported | Ensure `main.go` has `_ "github.com/go-sql-driver/mysql"` |
| `Error 1045: Access denied` | Wrong DB credentials | Check `sites/*/site_config.yaml` |
| `Error 1049: Unknown database` | Database not created | Run `kora setup` which auto-creates it |
| `address already in use` | Port conflict | `fuser -k 8000/tcp` |
| `/s/:site/workspace` returns 404 | Stale binary | Rebuild: `go build -o kora .` |
| White screen after SPA loads | Assets 404 | Check Vite `base` config, rebuild UI |
| Template parse panic | Template syntax error | Check `console/templates/` files |
| `site_not_found` | Site name doesn't match | Use short name: `airtime` not `airtime.local` |

# Contributing to Kora

Kora is a config-driven application engine — define your data model in YAML, get a database, REST API, and React admin UI. We welcome contributions of all kinds.

## What You Need Installed

| Dependency | Version | Why | Install |
|-----------|---------|-----|---------|
| **Go** | 1.25+ | Backend server, CLI, ORM, schema migration | [go.dev/dl](https://go.dev/dl/) |
| **Bun** | 1.3+ | Frontend package manager, build tool (faster than npm/yarn) | [bun.sh](https://bun.sh) |
| **Docker** | 27+ | Runs MySQL and Redis for development | [docker.com](https://docker.com) |
| **MySQL** | 8.0 | Database engine (runs in Docker for dev) | Comes with Docker |

That's it. Bun replaces Node.js/npm — `bun install`, `bun run build`, `bun run dev` all work. Docker gives you MySQL without installing it locally.

### Verify Your Setup

```bash
go version      # go1.25.0 or later
bun --version   # 1.3.0 or later
docker ps       # Docker running
```

## How Everything Works Together

### The Two Halves

Kora is a **single Go binary** that embeds a React SPA inside itself:

```
┌─────────────────────────────────────────────┐
│                 kora binary                  │
│                                              │
│  ┌──────────────────┐  ┌──────────────────┐ │
│  │   Go backend     │  │  React SPA (ui/) │ │
│  │   (port :8000)   │  │  (embedded via   │ │
│  │   REST API       │  │   go:embed)       │ │
│  │   Auth           │  │                   │ │
│  │   ORM            │  │  Served at        │ │
│  │   Migrations     │  │  /workspace       │ │
│  └────────┬─────────┘  └──────────────────┘ │
│           │                                   │
│     ┌─────┴──────┐                           │
│     │   MySQL    │                           │
│     │  (docker)  │                           │
│     └────────────┘                           │
└─────────────────────────────────────────────┘
```

### Request Flow

```
Browser → :8000 → Gin router
  ├── /api/*       → SiteGuard (auth) → Permission check → ORM → MySQL
  ├── /workspace/* → NoRoute handler → serves embedded React SPA
  ├── /console/*   → SystemGuard → server-rendered admin console
  └── /api/auth/*  → Public (login, logout)
```

### Key Concept: Config-Driven

Kora has **no per-entity code**. There's no `invoice.go` or `CustomerForm.tsx`. Everything is generic:

1. You define a DocType in YAML (or via the Administrator panel in the browser)
2. Kora creates the MySQL table, builds the REST API, renders the React forms
3. All views — list, create, edit — are rendered from the DocType field definitions

```
YAML/Admin UI  →  _kora_doctype table  →  Registry (in-memory)
                                         →  Schema migration (CREATE TABLE)
                                         →  API endpoints (generic CRUD)
                                         →  React forms (config-driven)
```

## Development Workflow

### Quick Start (One Command)

```bash
make dev DB_PASS=kora123 ADMIN_EMAIL=admin@airtime.local ADMIN_PASS=kora123
```

This does everything: starts MySQL, builds the UI, builds the Go binary, sets up the `airtime.local` site with sample config, and starts the server on `:8000`.

### The Dev Loop

After the initial `make dev`, you'll iterate on either the backend or frontend:

**Frontend-only changes** (most common):
```bash
make restart          # Rebuild UI + Go, kill old server, start fresh
```

**Backend-only changes**:
```bash
go build -o kora .   # Rebuild Go binary
./kora serve --port 8000  # Restart server
```

**Frontend dev server** (hot reload, no Go rebuild):
```bash
cd ui && bun run dev  # Dev server on :5173, proxies /api → :8000
```

### Understanding the Build

`make build` runs three steps:
1. `cd ui && bun run build` — TypeScript → Vite → `ui/dist/`
2. `cp -r ui/dist workspace/dist` — Copy for Go's `go:embed` directive
3. `go build -o kora .` — Compile Go binary with embedded SPA

The `workspace/spa.go` file has `//go:embed dist/*` which bakes the React app into the binary. In dev, the `NoRoute` handler in `workspace/spa.go` serves these embedded files.

### Project Structure

```
kora/
├── cli/            # Cobra CLI commands (serve, setup, migrate, config)
├── api/            # REST handlers — router.go (CRUD), system.go (admin API)
├── auth/           # Session auth, CSRF tokens, SiteGuard, SystemGuard
├── net/            # SiteRouter (multi-site), TLS, security headers, CORS
├── doctype/        # Core types — DocType, Field, Registry, Workflow, permissions
├── orm/            # Generic CRUD on map[string]any documents
├── schema/         # INFORMATION_SCHEMA diff → DDL, migration safety tiers
├── configstore/    # Persist config to _kora_* tables, versioning, roles/permissions
├── workspace/      # SPA serving (go:embed), NoRoute handler
├── console/        # System admin console (server-rendered Go HTML templates)
├── scheduler/      # Cron-style background jobs
├── ui/             # React 19 SPA
│   ├── src/
│   │   ├── router.tsx              # TanStack Router — all routes
│   │   ├── components/
│   │   │   ├── layout/             # RootLayout, Sidebar, AuthGuard
│   │   │   ├── forms/              # FieldRenderer, LinkField, ChildTableEditor
│   │   │   └── tables/             # DataTable (shared list component)
│   │   ├── routes/workspace/
│   │   │   ├── admin/              # Administrator tab pages
│   │   │   ├── $doctype/           # Generic doctype views
│   │   │   └── auth/               # Login
│   │   ├── lib/                    # API client, auth store, utilities
│   │   └── types/                  # TypeScript type definitions
│   └── dist/                       # Built SPA output (auto-generated)
├── config/         # Sample app YAML configs
├── sites/          # Per-site config (site_config.yaml) and uploaded files
├── docs/           # Documentation
└── skills/         # Claude Code skill definitions
```

### Where to Make Changes

| You want to... | Change this |
|---------------|-------------|
| Add a new API endpoint | `api/system.go` — add handler, register in `RegisterSystemRoutes` |
| Add a new field type | `doctype/doctype.go` — add to `validateFieldType`, `Field.DBType`, `Field.IsDataField`; `ui/src/types/kora.ts` — add to `FieldType` union; `ui/src/components/forms/FieldRenderer.tsx` — add render case |
| Add a new admin page | `ui/src/routes/workspace/admin/` — new page component; `ui/src/router.tsx` — add route; `ui/src/components/layout/Sidebar.tsx` — add nav item |
| Change how data is stored | `configstore/store.go` — DB schema and queries; `doctype/` — struct definitions |
| Change the UI component library | `ui/src/components/ui/` — shadcn/ui components |
| Add a new constraint type | `doctype/validation.go` — add validation logic; `ui/src/routes/workspace/admin/doctypes/editor.tsx` — add constraint UI |
| Change mobile layout | `ui/src/components/tables/DataTable.tsx` — shared table component; individual pages use `hidden md:` / `md:hidden` pattern |

### Route Ordering (Important)

In `ui/src/router.tsx`, **admin routes must come before `$doctype`**. The `$doctype` param route catches all paths under `/workspace/`, so if it's registered first, literal paths like `admin` get captured as a doctype name. This causes TanStack Router "invariant failed" errors.

```typescript
// CORRECT: admin before $doctype
workspaceLayout.addChildren([
  dashboardRoute,
  adminRoute.addChildren([...]),      // Literal paths first
  doctypeRoute.addChildren([...]),    // Catch-all param last
  settingsRoute,
])
```

### CSS and Styling

- **Tailwind CSS v4** — utility-first CSS. No separate CSS files needed for most changes.
- **shadcn/ui** — Reusable components in `ui/src/components/ui/`. Import from there, don't add new UI deps.
- **Dark mode** — Uses `.dark` class on `<html>`. All components use `dark:` variants.
- **Mobile** — Use `md:` breakpoint for desktop/mobile split. Tables get `hidden md:table` + `md:hidden` card fallback.

## Testing

```bash
# Backend
go test ./...                       # Run all Go tests

# Frontend
cd ui && bunx tsc --noEmit         # TypeScript type checking
cd ui && bun run build              # Full production build (catches all errors)
```

## Pull Requests

1. Keep PRs focused — one feature or fix per PR
2. Update docs if your change adds or changes behavior
3. Run `go build -o kora .` and `cd ui && bun run build` before submitting
4. Test on mobile viewport if you changed UI (DevTools → toggle device toolbar)
5. Describe what you changed and why in the PR description
6. All commits to `main` go through PRs — never push directly

## Common Issues

**"invariant failed" in browser console** — Route ordering problem. The `$doctype` catch-all is capturing a literal path. Move admin routes before `$doctype` in `router.tsx`.

**"Something went wrong" on save** — Check the Network tab for the API response. Usually a validation error from the backend with a useful message. Also check Go server logs.

**Build fails with "pattern dist/*: no matching files"** — Run `cd ui && bun run build` first, then `cp -r ui/dist workspace/dist`. The Go build needs `workspace/dist/` to exist for `go:embed`.

**Port 8000 already in use** — Run `fuser -k 8000/tcp` or use `make restart` which handles this automatically.

**MySQL connection refused** — Run `docker compose up -d mysql` and wait ~5s for it to be ready. Password is `kora123` for root.

## License

Kora is licensed under the GNU Affero General Public License v3.0. By contributing, you agree that your contributions will be licensed under the same terms.

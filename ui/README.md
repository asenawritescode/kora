# Kora UI

React 19 SPA for the Kora config-driven application engine. All views are **config-driven** — the UI has no knowledge of specific doctypes. It reads schemas from the API and renders forms, lists, and workflows generically.

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Framework | React 19 |
| Router | TanStack Router (file-based routes) |
| Data Fetching | TanStack Query |
| Tables | TanStack Table |
| Forms | TanStack Form + Zod validation |
| Components | shadcn/ui |
| Styling | Tailwind CSS v4 |
| State | Zustand |
| Build | Vite + bun |

## Project Structure

```
ui/src/
├── router.tsx              # Route tree — admin routes before $doctype catch-all
├── routes/
│   ├── __root.tsx          # Root layout with AuthGuard, Sidebar, ChatWidget
│   └── workspace/
│       ├── auth/login.tsx  # Login page
│       ├── index.tsx       # Home dashboard
│       ├── $doctype/       # Dynamic CRUD routes
│       │   ├── index.tsx   # List view
│       │   ├── new.tsx     # Create form
│       │   └── $name.tsx   # View/Edit form
│       └── admin/
│           ├── doctypes.tsx     # DocType visual builder
│           ├── permissions.tsx  # Role × DocType matrix
│           ├── workflows.tsx    # State machine editor
│           ├── versions.tsx     # Config version history
│           ├── users.tsx        # User CRUD + password reset
│           └── secrets.tsx      # AI provider key management
├── components/
│   ├── layout/             # Sidebar, AuthGuard, RootLayout
│   ├── forms/              # Config-driven form widgets per field type
│   ├── tables/             # DataTable (desktop + mobile stacked cards)
│   ├── chat/               # AI ChatWidget + useChat hook
│   └── ui/                 # shadcn/ui primitives
├── lib/
│   ├── api/                # API client + typed functions
│   │   ├── client.ts       # Base fetch wrapper (CSRF, envelope)
│   │   ├── resources.ts    # CRUD for doctype resources
│   │   └── system.ts       # Admin endpoints (users, secrets, roles, etc.)
│   ├── auth-store.ts       # Zustand auth store
│   ├── ui-store.ts         # Zustand UI state (sidebar, theme)
│   ├── basepath.ts         # Multi-site path helpers
│   ├── computed-fields.ts  # Client-side computed field evaluation
│   └── expression-eval.ts  # SUM(), ROUND(), arithmetic parser
└── hooks/                  # Custom React hooks
```

## Development

```bash
cd ui
bun install          # Install dependencies
bun run dev          # Dev server (hot reload, proxies /api → :8000)
bun run build        # Production build → dist/
bun run lint         # ESLint + TypeScript check
```

### Dev Server

The dev server proxies `/api` and `/console` requests to `http://localhost:8000`. Start the Go backend separately:

```bash
# Terminal 1: Backend
make serve

# Terminal 2: Frontend
cd ui && bun run dev
```

The dev server runs on port 5173 by default. Login at the workspace running on port 8000, then switch to port 5173 for hot-reload development.

### Production Build

```bash
cd ui && bun run build
# Output: dist/ → copied to workspace/dist → go:embed in binary
```

## Key Patterns

### Config-Driven Rendering

All views read the doctype schema from `GET /api/system/doctype/:name` and render generically:

- **Form fields**: Each `fieldtype` maps to a widget component (Data→Input, Select→Dropdown, Link→Autocomplete, Table→ChildTableEditor, etc.)
- **List columns**: `in_list_view: true` fields become table columns. Field type determines cell renderer.
- **Workflow bar**: Current state badge (color from workflow config), action buttons filtered by role+state
- **Constraints**: Zod schemas built from field constraints at render time

### Multi-Site Routing

The SPA auto-detects the basepath (`/s/:site` prefix). The `sitePath()` helper preserves this prefix in all navigation. TanStack Router's `__root.tsx` wraps everything in `AuthGuard`.

### Mobile Responsive

- Sidebar collapses to hamburger menu
- Tables become stacked card layout (`hidden md:` / `md:hidden`)
- Permissions uses role drill-down accordion
- Workflow editor uses collapsible card sections

### Auth

- **Workspace auth**: SiteGuard validates `kora_sid` cookie → user + roles from `_kora_user`
- **Console auth**: Separate system (SystemGuard) with `kora_console_sid` cookie
- **CSRF**: Double-submit cookie pattern, `X-Kora-CSRF-Token` header
- **AuthGuard.tsx**: Wraps entire app, skips public paths (login, console)

### AI Chat Widget

Floating Intercom-style panel (bottom-right), embedded in `RootLayout.tsx`. Uses `POST /api/chat` with CSRF token. The backend auto-generates OpenAI-compatible function definitions from the doctype registry.

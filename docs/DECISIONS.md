# Kora — Architecture Decision Records (ADRs)

## ADR-001: Config in Database, Not Filesystem

**Date:** 2026-06-10
**Status:** Accepted

**Context:** The application config (DocTypes, fields, permissions, workflows) could be stored as YAML files on disk or in the database.

**Decision:** Config lives in the database as the source of truth. YAML files are a one-shot import mechanism.

**Rationale:**
- Config can be updated at runtime without filesystem access
- Config is versioned naturally in the database
- Config is per-site in multi-tenant setups
- The API to read/write config is the same API used for all other data
- Mitigation for Git-based workflows: `kora config export` produces YAML files that can be committed

**Trade-offs:**
- Operators lose direct Git-based config management (mitigated by export/import)
- Bootstrapping a fresh site requires database access

---

## ADR-002: Go Over Python/Node.js

**Date:** 2026-06-10
**Status:** Accepted

**Context:** Frappe (the architectural inspiration) is Python. Payload CMS is Node.js. We needed to choose a language.

**Decision:** Go.

**Rationale:**
- Single binary deployment (no interpreter, no virtualenv, no node_modules)
- Strong concurrency (goroutines for job workers)
- Fast startup (milliseconds vs seconds)
- Rich stdlib (`database/sql`, `net/http`, `embed`, `crypto/tls`)
- The Frappe pattern (config-driven engine) is language-agnostic

**Trade-offs:**
- Smaller ecosystem than Python for business applications
- No dynamic module loading (Go hooks must be compiled in)

---

## ADR-003: Generic ORM with map[string]any

**Date:** 2026-06-10
**Status:** Accepted

**Context:** The engine must work with any DocType without code generation.

**Decision:** All documents are `map[string]any` at runtime. Type safety is enforced by the constraint validation layer, not the compiler.

**Rationale:**
- Zero code generation needed
- New DocTypes work immediately after config import
- The engine is truly generic

**Trade-offs:**
- No compile-time type safety for document fields
- Reflection overhead on field access
- `[]byte` from MySQL driver must be converted to `string` for JSON serialization

---

## ADR-004: MySQL Over PostgreSQL (Initially)

**Date:** 2026-06-10
**Status:** Accepted (Phase 1-3), PostgreSQL planned for Phase 4

**Context:** We needed a database for the initial implementation.

**Decision:** MySQL 8.0 for Phase 1-3. PostgreSQL support added in Phase 4.

**Rationale:**
- MySQL DDL is simpler and well-understood
- `INFORMATION_SCHEMA` works for schema introspection
- Go's `database/sql` is database-agnostic (migration layer abstracts differences)
- Frappe uses MariaDB, proving the pattern

---

## ADR-005: HTMX + Alpine.js Over React/Vue

**Date:** 2026-06-11
**Status:** Superseded by ADR-011 (React SPA)

**Context:** The admin UI needs to be functional but minimal. It ships in the binary.

**Decision:** HTMX + Alpine.js + Tailwind CSS, loaded via CDN. HTML templates embedded via `go:embed`.

**Rationale:**
- No build step (no webpack, no npm, no node_modules)
- Ships in the binary via Go's `embed` package
- HTMX handles dynamic page loading without a SPA framework
- Alpine.js handles client-side interactivity without a heavy framework
- The admin UI is a thin layer over the REST API, not a separate application

**Trade-offs:**
- Less rich interactivity than a React/Vue SPA
- CDN dependency for JS/CSS (mitigated by optionally bundling these assets)
- Limited offline capability

---

## ADR-006: No External Reverse Proxy

**Date:** 2026-06-11
**Status:** Accepted

**Context:** Frappe uses nginx as a mandatory reverse proxy. We needed to decide whether Kora requires one.

**Decision:** Kora handles everything in-process. No nginx, no Caddy, no external reverse proxy.

**Rationale:**
- Go's `net/http` can serve TLS directly
- `autocert` provides automatic Let's Encrypt certificates
- `x/time/rate` provides in-process rate limiting
- Security headers, CORS, CSRF are all in-process middleware
- Single binary, single process = simpler operations
- Users CAN put a CDN or load balancer in front if they want, but it's optional

**Trade-offs:**
- No static file serving optimization (nginx is faster for static files)
- Rate limiting is per-process, not global (mitigated by Redis-backed limiter in future)
- TLS certificate management is the app's responsibility

**Alternatives considered:**
- **Lura (KrakenD engine):** API gateway framework; overkill since Kora IS the backend, not a proxy
- **Caddy as library:** Possible but Caddy is designed as a server first, library second
- **nginx:** Adds an external dependency and configuration file

---

## ADR-007: Session Cookies Over JWT

**Date:** 2026-06-11
**Status:** Accepted

**Context:** Authentication could use JWT tokens or session cookies.

**Decision:** Session cookies with optional Bearer token fallback.

**Rationale:**
- Session cookies are simpler to secure (HttpOnly, SameSite)
- Server-side session invalidation (logout, password change) works immediately
- No token refresh complexity
- Bearer token fallback supports API clients that can't use cookies
- CSRF protection via double-submit cookie pattern

**Trade-offs:**
- Requires session storage (currently DB, planned Redis for multi-server)
- Mitigated by in-memory session cache (30s TTL) added in ADR-023 — 99% of session lookups are cache hits, eliminating the per-request DB query

---

## ADR-008: expr-lang/expr Over Custom Expression Language

**Date:** 2026-06-11
**Status:** Accepted

**Context:** Constraint conditions and workflow conditions need a safe expression language.

**Decision:** Use `expr-lang/expr` with custom functions (`today`, `now`, `len`, `has_role`).

**Rationale:**
- Safe and sandboxed (no arbitrary code execution)
- Fast (compiles to bytecode)
- Rich operator set already built-in
- Custom functions can be registered for domain-specific needs
- Avoids building and maintaining a custom expression parser

**Trade-offs:**
- Expression syntax is fixed (can't customize operators)
- Compilation errors surface at runtime (expressions are strings in config)

---

## ADR-009: Normalized Child Tables (Not JSON Blobs)

**Date:** 2026-06-11
**Status:** Accepted

**Context:** Table fields (child/parent relationships) could be stored as JSON blobs in the parent row or as normalized separate tables.

**Decision:** Normalized tables with `parent`, `parentfield`, `parenttype` columns.

**Rationale:**
- Child rows are independently queryable
- Referential integrity via foreign keys (future)
- Consistent with the Frappe pattern
- Schema migration applies to child tables too

**Trade-offs:**
- More complex INSERT/UPDATE logic (delete old children, insert new ones)
- More database tables (one child table per Table field)

---

## ADR-010: Additive-Only Schema Migrations (Default)

**Date:** 2026-06-11
**Status:** Accepted

**Context:** Schema migrations could be fully automatic (including destructive changes) or require explicit confirmation.

**Decision:** Additive changes (ADD COLUMN, CREATE INDEX) are auto-applied. Destructive changes (DROP COLUMN, CHANGE TYPE) require `--allow-breaking` flag.

**Rationale:**
- Prevents accidental data loss
- Additive changes are safe and common (adding fields)
- Destructive changes should be intentional and reviewed
- Orphaned columns accumulate until explicitly cleaned (`kora schema clean`)

**Trade-offs:**
- Operators must explicitly approve breaking changes
- Orphaned columns waste storage until cleaned

---

## ADR-011: React SPA Over HTMX+Alpine for Admin UI

**Date:** 2026-06-11
**Status:** Accepted

**Context:** The original admin UI used HTMX + Alpine.js server-rendered templates. As complexity grew (child tables, linked fields, computed expressions, autocomplete), the imperative DOM manipulation became unwieldy.

**Decision:** React 19 + Vite + TanStack (Router/Query/Table/Form) + shadcn/ui + Tailwind CSS v4. Single binary deployment via `go:embed`.

**Rationale:**
- Declarative component model handles complex form interactions naturally
- TanStack suite provides best-in-class table, form, and query management
- shadcn/ui is copy-owned (not a dependency), fully themeable via CSS variables
- TypeScript provides end-to-end type safety from API responses to UI
- No separate deployment — SPA is embedded in the Go binary
- Config-driven: zero code per DocType, everything reads from `/api/system/doctype/:name`

**Trade-offs:**
- Requires Node.js/bun for development builds
- Larger binary (~35MB with embedded SPA) vs pure Go templates
- Build step required before Go compilation

---

## ADR-012: Config-Driven Computed Fields

**Date:** 2026-06-11
**Status:** Accepted

**Context:** Documents often have derived fields (line_total = quantity × unit_price, subtotal = SUM(items.line_total)). Initially hardcoded per doctype in the frontend.

**Decision:** New `computed` field property containing an expression string. The frontend expression evaluator reads it from the doctype schema and evaluates it generically.

**Rationale:**
- Removes all hardcoded business logic from forms
- Same expression syntax for any doctype
- Expressions: arithmetic (`+`, `-`, `*`, `/`), `SUM(table.field)`, `ROUND(expr, N)`
- No backend changes needed — evaluation happens client-side against current form state
- Cascading: changing one field triggers recomputation of all dependent computed fields

**Trade-offs:**
- Client-side evaluation means expressions can't use server-side data
- Expression syntax is limited to what the frontend evaluator supports

---

## ADR-013: Linked Field Auto-Population (`linked_field`)

**Date:** 2026-06-11
**Status:** Accepted

**Context:** When selecting a Product in an Order Item, the `unit_price` should auto-fill from the Product's `selling_price`. This is a general pattern for any Link field.

**Decision:** New `linked_field` property: `"{link_fieldname}.{source_fieldname}"`. When the Link field value changes, the frontend fetches the linked document and populates the target field.

**Rationale:**
- Config-driven — works for any Link field on any doctype
- User can override the auto-populated value
- Composes with `computed`: linked_field triggers → computed cascades
- Single property, simple syntax

---

## ADR-014: Path-Based Multi-Site Access (`/s/:site/`)

**Date:** 2026-06-12
**Status:** Accepted

**Context:** Multi-site routing via Host header requires DNS config or `/etc/hosts` entries. This adds friction for local development and testing.

**Decision:** Add path-based site access as a zero-config fallback. `/s/:site/workspace` routes to the named site. Host-based routing via `Host` header remains the primary mechanism for production. Both methods coexist.

**Rationale:**
- Zero configuration: `localhost:8000/s/airtime/workspace` works immediately
- No `/etc/hosts` entries needed for local development
- Host-based routing still works for production (cleaner URLs, SEO)
- Both methods share the same middleware chain and site context injection
- A `kora_site` cookie persists the site selection across requests for API calls

**Technical implementation:** A `NoRoute` handler intercepts `/s/:site/*` paths, looks up the site by name (with fuzzy matching — `airtime` matches `airtime.local`), injects site context, and serves or rewrites the request. For workspace paths it serves the SPA directly. For API paths it calls `router.HandleContext()` to re-dispatch.

**Trade-offs:**
- Path-based URLs are longer than host-based
- The `kora_site` cookie is needed for API calls to know which site context to use
- `HandleContext` re-runs the full middleware chain for API requests (slight overhead)

---

## ADR-015: Gin `NoRoute` for SPA Serving

**Date:** 2026-06-12
**Status:** Accepted

**Context:** The React SPA needs client-side routing — all paths under `/workspace` should serve `index.html`. Gin's radix tree forbids catch-all routes (`/workspace/*filepath`) alongside other routes at the same prefix level.

**Decision:** Use `router.NoRoute` to serve the SPA for `/workspace` and `/assets` paths, rather than registering explicit routes or middleware.

**Rationale:**
- Avoids Gin's radix tree conflicts between catch-all and exact routes
- Single handler for SPA serving, SPA fallback routing, asset serving, and path-based site access
- Middleware approach doesn't work — Gin matches routes before running middleware, and NoRoute fires after middleware with a 404 status that can't be overridden

**Trade-offs:**
- Only one `NoRoute` handler can be registered — all fallback logic must live in one function
- Any new "catch-all" behavior must be added to this handler

---

## ADR-016: Site-Aware Authentication (Per-Site DB for Sessions)

**Date:** 2026-06-12
**Status:** Accepted

**Context:** The original SessionManager was initialized with `firstDB` (the first site loaded) and used for all auth operations across all sites. This meant sessions created via `/s/fieldwork/api/auth/login` were stored in `airtime_db` (the first site), and session validation always checked `airtime_db`. Cross-site login/logout was broken.

**Decision:** All auth handlers (login, logout, /me, AuthMiddleware) read `site_db` from the Gin context and create a site-specific SessionManager on each request.

**Rationale:**
- Sessions are stored in the correct site's `_kora_session` table
- A session created on the Fieldwork site is only valid for Fieldwork
- No cross-site session leakage
- Consistent with the rest of the API (ORM reads `site_registry`, `site_db` from context)

**Trade-offs:**
- Creates a new SessionManager on each auth request (lightweight — just wraps `*sql.DB`)
- Login at `/s/airtime` with `admin@fieldwork.local` credentials fails (correct behavior — credentials are per-site)

---

## ADR-017: Server-Side Computed Field Evaluation

**Date:** 2026-06-12
**Status:** Accepted

**Context:** Computed fields (`computed: "quantity * unit_price"`) were initially evaluated only client-side in the React SPA. This meant computed values were never persisted and couldn't be used in workflow conditions (`doc.total > 0`).

**Decision:** Evaluate computed fields server-side in the ORM layer during Insert and Save. Values are persisted via a follow-up UPDATE after child items are processed.

**Rationale:**
- Workflow conditions can reference computed fields (`doc.total > 0`)
- Computed values survive server restarts and are visible in API responses
- Multi-pass evaluation ensures dependencies resolve: child fields → aggregates → dependent fields
- Frontend still evaluates for instant feedback during editing

**Implementation:** `doctype/computed.go` — `ComputeFields()` called by ORM Insert/Save. Parses expressions with regex patterns for `SUM()`, `COUNT()`, `DATEDIFF()`, `ROUND()`, then compiles arithmetic with `expr-lang/expr`. Three-pass evaluation: aggregates first, then DATEDIFF/ROUND, then simple arithmetic.

---

## ADR-018: Transaction-Based Save for Child Tables

**Date:** 2026-06-12
**Status:** Accepted

**Context:** `Save()` for documents with child tables used separate implicit transactions for DELETE (old children) and INSERT (new children). This caused "Duplicate entry" errors when child names collided.

**Decision:** Wrap the entire Save operation (UPDATE parent + DELETE children + INSERT children + computed field persistence) in a single database transaction. Use `INSERT ... ON DUPLICATE KEY UPDATE` for child rows as defense-in-depth.

**Rationale:**
- Atomicity: if any step fails, all changes roll back
- Eliminates race conditions between DELETE and INSERT
- `ON DUPLICATE KEY UPDATE` handles edge cases where a child row already exists
- A `sqlExecutor` interface abstracts `*sql.DB` and `*sql.Tx` so insert logic works in both contexts

---

## ADR-019: CSRF Middleware Ordering Fix

**Date:** 2026-06-12
**Status:** Accepted

**Context:** `SiteGuard.Middleware()` composed `AuthMiddleware` and `CSRFMiddleware` by calling them as functions. `AuthMiddleware` called `c.Next()` internally, which dispatched to the handler BEFORE `CSRFMiddleware` ran. This caused every POST/PUT response to contain both the handler's JSON AND a CSRF error — two concatenated JSON bodies.

**Decision:** Extract `validateSession()` from `AuthMiddleware` — a pure validation function that returns `bool` without calling `c.Next()`. SiteGuard calls it directly, then runs CSRF check, then calls `c.Next()` once at the end.

**Rationale:**
- Handler runs only after ALL middleware checks pass
- No double-responses — CSRF aborts before the handler executes
- `AuthMiddleware` still works standalone (wraps `validateSession` + `c.Next()`) for routes outside SiteGuard

---

## ADR-020: Console Authentication with Basic Auth + Session Tokens

**Date:** 2026-06-12
**Status:** Accepted

**Context:** The original `/console` SystemGuard accepted ANY non-empty `kora_console_sid` cookie as authenticated — zero server-side validation. The "token" was `email:unix_timestamp` with no randomness.

**Decision:** 
- In-memory session store with cryptographically random 64-char hex session IDs
- Server-side validation on every request — no cookie forgery possible
- 24-hour session expiry with background cleanup goroutine
- Two auth methods: HTTP Basic auth (`base64(email:password)`) for API clients, session cookie for browser
- bcrypt cost 12 for system admin password (vs default 10)
- Plaintext `Bearer email:password` removed — was never properly base64-decoded

**Rationale:**
- System console is single-admin, so in-memory store is sufficient (no DB dependency)
- Basic auth enables scripting and API access without browser cookies
- Session cookies enable browser-based console management
- Background cleanup prevents memory leak from expired sessions

---

## ADR-021: Cookie Security Defaults

**Date:** 2026-06-12
**Status:** Accepted

**Context:** All cookies (`kora_sid`, `kora_csrf`, `kora_console_sid`, `kora_site`) were set with `Secure: false` hardcoded and no `SameSite` attribute. This made them vulnerable to network interception and cross-site request inclusion.

**Decision:** 
- `Secure` flag auto-detected from `c.Request.TLS != nil` — cookies are Secure when the request is over HTTPS
- `SameSite=Lax` appended to all cookies — prevents cross-site request inclusion while allowing top-level navigation
- `SetSecureCookie()` helper in both `auth` and `net` packages for consistent cookie creation
- CSRF token comparison uses `crypto/subtle.ConstantTimeCompare` to prevent timing attacks

**Rationale:**
- No configuration needed — Secure flag adapts to deployment context
- SameSite=Lax is the modern default (balances security and usability)
- Constant-time comparison prevents timing side-channel attacks on CSRF tokens

## ADR-022: ULID Over UUID for Request IDs and Child Row Names

**Date:** 2026-06-13
**Status:** Accepted

**Context:** The codebase used `github.com/google/uuid` for request IDs, config version IDs, and planned child row naming. UUIDs have poor B-tree index locality and are non-sortable by creation time.

**Decision:** Migrate all UUID usage to `github.com/oklog/ulid/v2`. ULIDs are:
- 26 characters vs UUID's 36 (shorter)
- Lexicographically sortable (timestamp in first 48 bits)
- URL-safe (Crockford base32 encoding)
- Monotonically increasing within the same millisecond (no collisions)

**Impact:** Request IDs, config version IDs, and child row names all use ULID. Child rows no longer need a `SELECT COUNT(*)` query for name generation — ULID names are generated client-side with zero DB cost.

## ADR-023: Session Cache with 30-Second TTL

**Date:** 2026-06-13
**Status:** Accepted

**Context:** Every authenticated API request performed a `SELECT data, expires_at FROM _kora_session WHERE sid = ?` query. At 100 concurrent users, this generates hundreds of redundant queries per second against a small set of active sessions.

**Decision:** Add an in-memory TTL cache inside `SessionManager`:
- `sync.RWMutex`-protected `map[sid]*cacheEntry` with 30-second TTL
- Cache invalidated on logout (`DeleteSession`) and password change (`InvalidateSession`)
- Background sweep goroutine runs every 5 minutes to clean expired entries
- Session cleanup (`DELETE FROM _kora_session WHERE expires_at < NOW()`) moved from per-login to background goroutine

**Trade-off:** 30-second window where role/permission changes don't take effect for cached sessions. Acceptable for single-server deployments. Multi-server requires Redis (already configured but not yet wired).

## ADR-024: Strict YAML Validation with goccy/go-yaml

**Date:** 2026-06-13
**Status:** Accepted

**Context:** `gopkg.in/yaml.v3` silently ignores unknown YAML keys at 14+ unmarshal call sites. Typos like `feildname` or `is_submittible` pass without error, causing invisible configuration bugs.

**Decision:** Adopt `github.com/goccy/go-yaml` (already an indirect dependency) for all user-facing YAML parsing:
- `DisallowUnknownField()` rejects unknown keys at parse time
- Pre-computed known-field maps for DocType, Field, Constraint, and DocConstraint structs
- `findUnknownKeys()` walks the YAML tree recursively into `fields[]`, `constraints[]`, and `doc_constraints[]`
- Levenshtein-based "did you mean?" suggestions for typos within edit distance ≤ 3
- New `POST /api/system/doctype/validate` endpoint accepts raw YAML and returns structured errors with line numbers

**Trade-off:** `gopkg.in/yaml.v3` retained for internal serialization (export, diff) where strict mode would break things.

## ADR-025: Diff-Based Child Table Save (Three-Way Reconciliation)

**Date:** 2026-06-13
**Status:** Accepted

**Context:** `Save()` unconditionally DELETE-ed all child rows and re-INSERT-ed all incoming rows. For a document with 100 child rows where 1 field on 1 row changed, this ran 1 DELETE + 100 SELECT COUNT(*) + 100 INSERTs = 201 queries.

**Decision:** Implement three-way reconciliation (modeled on Frappe's `update_child_table()`):
1. **DELETE** rows present in old document but missing in new document (bulk, by name IN)
2. **INSERT** rows present in new document but missing in old document (batched, chunked at 100)
3. **UPDATE** rows present in both with changed data fields
4. Fall back to DELETE+re-INSERT when old document is not provided

**Impact:** Common case (no child changes) drops from 200+ queries to 0 queries. Only changed rows hit the database.

## ADR-026: Site Isolation — Host Validation for kora_site Cookie

**Date:** 2026-06-13
**Status:** Accepted

**Context:** The `kora_site` cookie (set by path-based routing at `/s/site/...`) allowed accessing a site's API from ANY Host header. An attacker on a shared network could inject a `kora_site=airtime` cookie and access airtime data from `evil.attacker.com`.

**Decision:** Add `isHostAllowedForSite()` validation in `SiteRouter.Middleware()`:
- `localhost`, `127.0.0.1`, or any IP address → allowed (dev/testing)
- Host matches one of the site's configured domains → allowed (production)
- Everything else → 403 `site_access_denied`

**Impact:** Path-based routing still works from `localhost` (no DNS needed for dev). Production requires the Host header to match a registered domain. Zero configuration needed.

## ADR-027: Explicit Permission Default — Admin-Only for New Doctypes

**Date:** 2026-06-13
**Status:** Accepted

**Context:** When a new doctype is created, the original `AutoCreatePermissionsForDoctype()` granted `read/write/create` to every existing role. This violated least privilege — a "Viewer" role would suddenly see every new doctype. The principle "explicit is better than implicit" demands that access be deliberately granted, not assumed.

**Decision:** When a doctype is created:
- **Administrator** role → ALL 10 permissions granted automatically
- **All other roles** → NO permissions (denied by default, must be explicitly granted)

If the Administrator role doesn't exist in `_kora_role`, no permissions are created. The built-in `AdminRole` constant in `doctype/permission.go` already bypasses the permission matrix for admin users regardless of DB entries.

**Rationale:**
- Zero-trust default: new resources start with no access
- Admin must consciously decide who gets access via the Permissions panel
- Prevents accidental data exposure from auto-granted permissions
- Clear audit trail: every non-admin permission grant is an explicit admin action
- Frontend already provides visual feedback (greyed-out buttons, read-only badges)

## ADR-028: React Query Cache Invalidation on Doctype Create/Update

**Date:** 2026-06-13
**Status:** Accepted

**Context:** After creating or updating a doctype via the Admin UI, the new doctype did not appear in the sidebar or admin list until the React Query cache expired (30 seconds for admin list, 5 minutes for navigation/sidebar). Users had to hard-refresh the page.

**Decision:** After a successful `createDoctype` or `updateDoctype` call, invalidate both query caches immediately:
```ts
queryClient.invalidateQueries({ queryKey: ['admin', 'doctypes'] })
queryClient.invalidateQueries({ queryKey: ['navigation'] })
```

**Impact:** New doctypes appear instantly in the sidebar, dashboard, and admin list without a page reload.

## ADR-029: Permission-Driven UI Gating

**Date:** 2026-06-13
**Status:** Accepted

**Context:** The frontend fetched permission data from the API (`DocTypeSchema.permissions`) but never consumed it. Users without create/write access saw the same UI as those with full access — they only discovered the lack of permission when the backend returned 403 after form submission.

**Decision:** Read `permissions` from the schema response and gate UI elements:
- **No `create` permission:** "New" button disabled (greyed out) with tooltip
- **No `write` permission:** Save button disabled, form fields disabled, "(read-only)" badge on list and edit pages
- Buttons stay visible — just disabled — so users know the action exists but they lack access

**Impact:** Users get immediate visual feedback about their permission level. No more filling out a form only to get a 403 on submit.

## ADR-030: AI Tool Loop — `finish_reason`-Driven Termination

**Date:** 2026-06-14
**Status:** Accepted

**Context:** The first implementation of AI chat used a single-round pattern: call AI with tools → if tool_calls present, execute and call AI again WITHOUT tools. This broke two-step workflows (find → create) because the follow-up call couldn't make a second tool call, causing the model to output tool calls as plain text.

**Alternatives considered:**
- **Max-iteration loop only:** `for round < N` — simple but doesn't use the model's own intent signal. Wastes rounds when the model is done early, breaks silently when the cap is hit.
- **Content-type check:** Check if `response.content[0].type == "text"` — fragile. Models can return text AND tool_calls in the same response.
- **`finish_reason`-driven loop:** The model signals its own intent via `finish_reason`. This is the pattern used by Anthropic SDK (`stop_reason`), OpenAI SDK, Vercel AI SDK, and LangChain.

**Decision:** Use `finish_reason` as the primary termination signal with composed safety nets:
- `"stop"` → return final response
- `"tool_calls"` → execute tools, feed results back, loop (tools ALWAYS present)
- `"length"` → return truncated response
- `"content_filter"` → return policy message
- Max rounds (configurable) as a fallback cap only — not a normal exit path

**Impact:** Two-step workflows (find → create) work correctly. Models that return text+tool_calls in one response are handled correctly. The loop exits when the model says it's done, not when a counter runs out.

## ADR-031: AI Doctype Creation — Draft-Only, Human Activation Required

**Date:** 2026-06-14
**Status:** Accepted

**Context:** When adding AI tools for doctype creation, the question was whether the AI should be allowed to activate doctypes (create DB tables + run migrations) or only create drafts.

**Alternatives considered:**
- **Full auto-activation:** AI creates and activates — fast but dangerous. A hallucinated doctype creates real DB tables with irreversible DDL.
- **No AI creation at all:** Safe but leaves the most powerful AI capability unused.
- **Draft-only:** AI creates Draft versions. A human reviews and activates. Matches the existing Config Version workflow (Draft → Active).

**Decision:** The `create_doctype_draft` tool ALWAYS sets `status: "Draft"`. It never activates — no migration runs, no tables created. The human activates from the Versions admin panel. The confirmation UX pattern: AI validates YAML → summarizes in 2-3 lines → asks "Create as draft?" → waits for confirmation → creates.

**Impact:** Zero risk of AI creating bad database tables. Human stays in control of all schema changes. The AI becomes a productivity multiplier (generates YAML from English descriptions) without any destructive capability.

## ADR-032: AI Audit Trail — `modified_by = "ai-assistant"`

**Date:** 2026-06-14
**Status:** Accepted

**Context:** The first implementation hardcoded `owner = "mcp-agent"` for AI-created records. This served as an implicit audit trail but broke permission checks (records owned by phantom user). The fix needed to preserve both audit capability and proper ownership.

**Alternatives considered:**
- **Keep `owner = "mcp-agent"`:** Easy audit but breaks filtering by owner, reports by user, and permission scoping.
- **`owner = user + " (via AI)"`:** String concatenation breaks exact-match lookups and is fragile.
- **Separate audit field:** Add a new column to every table — expensive schema change.
- **Use `modified_by`:** Every Kora table already has a `modified_by` column. On Insert, ORM sets both `owner` (to the authenticated user) and `modified_by` (to `"ai-assistant"` for AI-created records, or the user for direct REST API creates).

**Decision:** Change `Insert(dt, doc, owner)` to `Insert(dt, doc, owner, modifiedBy)`. REST API passes `owner` for both; AI Chat passes `owner = <user>` and `modifiedBy = "ai-assistant"`. Query `WHERE modified_by = 'ai-assistant'` for AI audit.

**Impact:** Records are properly owned by the real user (permissions work). AI-created records are easily queryable for audit. No schema change needed — `modified_by` already exists on every table. Two-line ORM signature change with only 2 call sites.

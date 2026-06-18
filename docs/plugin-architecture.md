# Kora — Plugin & Extension Architecture

**Version:** 2.0.0-draft
**Status:** Research complete — ready for implementation
**Research basis:** Two deep-research workflows (24 + 21 sources, 209 claims, 44 verified)
**Last Updated:** 2026-06-17

---

## Table of Contents

1. [Philosophy](#philosophy)
2. [The Event System](#the-event-system)
3. [Event Emission (Inside the Engine)](#event-emission)
4. [Webhook Delivery](#webhook-delivery)
5. [Signature Verification](#signature-verification)
6. [Extension Registry](#extension-registry)
7. [High Volume Handling](#high-volume-handling)
8. [Extension API Token Scoping](#extension-api-token-scoping)
9. [Cloudflare Workers Runtime](#cloudflare-workers-runtime)
10. [App Packs (Config-Only)](#app-packs)
11. [Admin UI](#admin-ui)
12. [Developer Experience](#developer-experience)
13. [Security Model](#security-model)
14. [What We Explicitly Rejected](#what-we-explicitly-rejected)
15. [Open Questions](#open-questions)

---

## Philosophy

> **Extensions react to things that happen. They do not participate in making them happen.**

This is the core design rule. Extensions sit *outside* the engine and respond to events it emits. They communicate with Kora through the same public REST API that any other client uses. The engine's request/response lifecycle is never blocked by extension code.

### What this means in practice

| Extension CAN | Extension CANNOT |
|---|---|
| Receive webhooks when documents change | Block or reject a document save |
| Call back into the Kora REST API to read/write data | Access the engine's database connection pool |
| Run on Cloudflare Workers, AWS Lambda, or any HTTP server | Access the engine's memory or internal state |
| Be written in any language | Assume synchronous execution in the request path |
| Be deployed, updated, and rolled back independently | Crash or slow down the engine |

### Why webhooks, not gRPC/in-process plugins

The two deep researches together studied 12 platforms (Stripe, GitHub, Shopify, Frappe, Heroku, Vercel, Railway, Supabase, Azure Event Grid, Cloudflare Workers for Platforms, Svix, Hookdeck). The universal pattern among platforms that have scaled to millions of integrations: **webhooks**.

The alternative — gRPC-based plugins where the engine calls out to external processes synchronously — was explicitly rejected because:

1. **It couples the engine's hot path to external code.** A slow extension blocks requests.
2. **It requires the engine to manage plugin processes.** Startup, health checks, restart, lifecycle.
3. **It creates a tight API contract between engine and plugin.** Versioning this is harder than versioning a REST API.
4. **No major API platform uses this model.** Stripe, GitHub, and Shopify all chose webhooks.

### Research basis

| Area | Key Sources | Confidence |
|---|---|---|
| Event naming | Stripe API docs, GitHub webhook docs | HIGH (3-0) |
| Signature verification | Stripe, GitHub, Shopify, Frappe primary docs | HIGH (3-0) |
| Retry schedules | Stripe, Shopify, Azure Event Grid docs | HIGH (3-0) |
| Secret rotation | Stripe SDK docs, Convoy analysis | HIGH (3-0) |
| Dead letter queues | Azure Event Grid docs | HIGH (3-0) |
| Workers for Platforms | Cloudflare docs, reference architectures | HIGH (3-0) |
| Rate limiting / backpressure | Minimal coverage across all platforms | MEDIUM |
| Bulk event batching | No platform documents this publicly | MEDIUM |
| API token scoping | No platform documents this publicly | MEDIUM |

---

## The Event System

### Event naming

Following Stripe's dot-notation convention (`resource.subresource.action`), Kora events use a three-segment hierarchical name:

```
{source}.{doctype}.{action}
```

**Source** is always `kora`. **Doctype** is the snake_cased DocType name. **Action** is the lifecycle verb.

**Stripe explicitly does NOT cascade events.** A `customer.subscription.updated` event does not fire a `customer.updated` event. Kora follows the same rule: child table changes do not fire parent doctype events. If an extension needs to know about child table changes, it subscribes to the child doctype's events.

### Event catalogue

#### Document lifecycle events

These are emitted for every non-system DocType (system DocTypes prefixed `_kora_` do not emit events).

| Event | Emitted when | Payload includes |
|---|---|---|
| `kora.{doctype}.before_insert` | Before a new document is committed | Full document (as received), actor |
| `kora.{doctype}.after_insert` | After a new document is committed | Full document (with generated name), actor |
| `kora.{doctype}.before_save` | Before insert or update | Full document, actor |
| `kora.{doctype}.after_save` | After insert or update | Full document, actor |
| `kora.{doctype}.before_delete` | Before a document is deleted | Document name, actor |
| `kora.{doctype}.after_delete` | After a document is deleted | Document name, actor |
| `kora.{doctype}.before_submit` | Before workflow submission | Full document, actor, from_state |
| `kora.{doctype}.after_submit` | After workflow submission | Full document, actor, from_state, to_state |
| `kora.{doctype}.before_cancel` | Before workflow cancellation | Full document, actor, from_state |
| `kora.{doctype}.after_cancel` | After workflow cancellation | Full document, actor, from_state, to_state |
| `kora.{doctype}.workflow_transition` | Any workflow state change | Full document, actor, from_state, to_state, transition_name |

**Design note on `before_*` events:** These are emitted synchronously within the request but the engine does NOT wait for extension responses. They are informational only. Extensions that need to flag a document for review use the async validation API (POST `/api/system/doctype/{name}/flag`). True synchronous blocking is deliberately not supported — see Philosophy above.

#### Auth events

| Event | Emitted when |
|---|---|
| `kora._auth.login` | Successful authentication |
| `kora._auth.logout` | Session termination |
| `kora._auth.login_failed` | Failed authentication attempt (includes IP, attempt count) |
| `kora._auth.password_reset` | Password reset completed |
| `kora._auth.user_created` | New user registered or created by admin |
| `kora._auth.role_assigned` | Role granted to a user |
| `kora._auth.role_removed` | Role revoked from a user |
| `kora._auth.token_refreshed` | Access token refreshed |

#### Config events

| Event | Emitted when |
|---|---|
| `kora._config.version_activated` | A config version transitions to Active |
| `kora._config.version_rolled_back` | Config rolled back to a previous version |
| `kora._config.app_pack_installed` | An App Pack is installed |
| `kora._config.app_pack_removed` | An App Pack is removed |

#### System events

| Event | Emitted when |
|---|---|
| `kora._system.job_failed` | Background job fails after all retries |
| `kora._system.job_completed` | Background job completes successfully |
| `kora._system.site_created` | New site provisioned (Kora Cloud only) |
| `kora._system.webhook_delivery_failed` | Webhook delivery fails permanently |
| `kora._system.error` | Engine-level error (panic recovered, DB connection lost) |

### Event envelope

Every event wraps the specific payload in a common envelope:

```json
{
  "id": "evt_01JQZTX2M3N5P7Q9R0S1T2U3V4",
  "source": "kora",
  "event": "kora.work_order.after_submit",
  "version": "1",
  "occurred_at": "2026-06-17T14:23:11.432Z",
  "config_version": 42,
  "actor": {
    "type": "user",
    "name": "ada@fieldwork.local",
    "roles": ["Field Technician"]
  },
  "site": "fieldwork.local",
  "data": {
    "doctype": "Work Order",
    "name": "WO-0042",
    "document": { ... }
  }
}
```

**Version** refers to the event schema version (always `"1"` initially). This is separate from `config_version` (the site's config version at the time the event was emitted). Extensions that depend on specific fields should check `config_version`.

### Payload design: "moderately fat"

Following GitHub and Stripe's approach, Kora sends **moderately fat** payloads: the full document snapshot is included in the webhook body. This avoids forcing the extension consumer to make a callback API request just to get the document data (the "fetch-before-process" pattern was refuted by our research — Stripe does it only for rate limiting, not as a universal recommendation).

Child table rows are NOT included by default in parent document payloads. If an extension needs child table data, it subscribes to the child doctype's events or fetches it via the API.

**Maximum payload size**: 64 KB. Documents larger than this are truncated with a `"truncated": true` flag, and the extension must fetch the full document via API.

**Design rationale**: The 64 KB cap prevents memory exhaustion in the webhook delivery worker. For documents with large child tables or binary fields, the cap is reached after serializing the top ~100 fields. Extensions that need the full document call `GET /api/resource/{doctype}/{name}`.

---

## Event Emission (Inside the Engine)

### Architecture

```
HTTP Request
    │
    ▼
API Handler (hot path)
    │
    ├── Validate + write to database (synchronous)
    │
    └── Emit event to internal event bus (fire-and-forget)
            │
            ▼
        Redis Streams (or in-memory channel for single-process)
            │
            ▼
        Webhook Delivery Worker (separate goroutine pool)
            │
            ├── Match event against extension subscriptions
            ├── Build event envelope
            ├── POST to extension endpoint
            └── Log delivery result
```

The critical design decision: **the API handler enqueues the event and returns immediately.** It does not wait for webhook delivery. This means the main request path has O(1) overhead for webhook emission — a single `XADD` to Redis Streams (or channel send for in-process).

### In-process vs Redis

For single-process deployments (self-hosted, development), events go through an in-memory Go channel. For multi-process deployments (Kora Cloud), events go through Redis Streams.

```go
// internal/events/bus.go

type EventBus interface {
    Emit(ctx context.Context, event *Event) error
    Subscribe(ctx context.Context, handler EventHandler) error
}

// In-process (single binary)
type ChannelBus struct {
    ch chan *Event  // buffered, 10000 capacity
}

// Multi-process (Kora Cloud)
type RedisStreamBus struct {
    client *redis.Client
    stream string  // "kora:events:{site}"
}
```

The `ChannelBus` has a bounded channel (10,000 events). If the channel is full, events are dropped and a `kora._system.error` event is emitted. This is a deliberate backpressure decision — the engine prioritizes API availability over webhook delivery completeness. In practice, a 10,000-event buffer covers even large bulk imports without dropping.

### Event construction

Events are constructed at the ORM layer — every `Insert`, `Save`, `Delete` call in `orm/` emits the appropriate event. The engine does not rely on individual API handlers to remember to emit.

```go
// orm/orm.go

func Insert(dt *doctype.DocType, doc Document, owner, modifiedBy string, opts *InsertOpts) error {
    // ... validate, write to database ...
    
    events.Emit(ctx, &events.Event{
        Source:  "kora",
        Event:   fmt.Sprintf("kora.%s.after_insert", dt.SnakeName()),
        Data: events.DocumentData{
            Doctype:  dt.Name,
            Name:     doc["name"].(string),
            Document: doc,
        },
        Actor: actorFromContext(ctx),
    })
    
    return nil
}
```

### `before_*` vs `after_*`

`before_*` events are emitted BEFORE the database transaction commits. They carry the document as it will be written. `after_*` events are emitted AFTER the commit succeeds. If the transaction rolls back, `after_*` events are not emitted.

`before_*` events are a best-effort notification for extensions that need to react quickly. They are NOT a two-phase commit mechanism. Extensions that need to validate or reject MUST use the async validation API, not `before_*` events.

---

## Webhook Delivery

### Delivery flow

```
Event emitted
    │
    ▼
Worker picks up event from event bus
    │
    ▼
Query _kora_extension for matching subscriptions
    │
    ├── No match → done
    │
    └── Match → for each matching extension:
            │
            ├── Build event envelope
            ├── Compute HMAC-SHA256 signature
            ├── POST to extension endpoint
            │     Headers:
            │       Content-Type: application/json
            │       X-Kora-Event: kora.work_order.after_submit
            │       X-Kora-Site: fieldwork.local
            │       X-Kora-Signature: t=1765981391,v1=abc123...
            │       X-Kora-Delivery: evt_01JQZTX2M3...
            │       X-Kora-Delivery-Attempt: 1
            │
            ├── Extension returns 2xx → log delivery as succeeded
            │
            └── Extension returns non-2xx / timeout → schedule retry
```

### Retry schedule

Based on the research showing Stripe (3 days, exponential backoff), Shopify (8 retries over 4 hours), and Azure Event Grid (10-step schedule to 24 hours), Kora uses a configurable retry schedule. The default is a middle ground:

| Attempt | Delay | Cumulative |
|---|---|---|
| 1 (initial) | Immediate | 0s |
| 2 | 30 seconds | 30s |
| 3 | 2 minutes | 2.5m |
| 4 | 10 minutes | 12.5m |
| 5 | 30 minutes | 42.5m |
| 6 | 2 hours | 2h 42m |
| 7 | 8 hours | 10h 42m |
| 8 (final) | 24 hours | 34h 42m |

**Rationale for ~35 hours vs Stripe's 3 days**: Kora extensions are typically user-facing integrations (Slack notifications, email, CRM sync), not payment processing webhooks. 35 hours in exponential backoff covers most real-world scenarios (deploy window, DNS propagation, temporary network issues). For payment-grade use cases, the retry window is configurable up to 72 hours.

**Jitter**: Each retry delay is multiplied by a random factor in [0.5, 1.5] to avoid thundering herd on recovery.

### Non-retryable failures

HTTP 4xx responses (other than 429) are NOT retried. They indicate a permanent configuration error:

| Status | Interpretation | Action |
|---|---|---|
| 200-299 | Success | Mark delivered |
| 400 | Bad request — payload shape issue | Dead letter immediately |
| 401 | Invalid signature — secret mismatch | Dead letter immediately |
| 403 | Forbidden | Dead letter immediately |
| 404 | Endpoint gone | Dead letter immediately |
| 410 | Endpoint deliberately removed | Dead letter immediately |
| 413 | Payload too large | Dead letter immediately |
| 429 | Rate limited | Retry with backoff |
| 5xx | Server error | Retry with backoff |
| Timeout | No response within 10s | Retry with backoff |
| Connection refused | Endpoint down | Retry with backoff |

This matches Azure Event Grid's pattern: immediate dead-letter on 400/401/403/413.

### Dead letter queue

After all 8 retries fail, the delivery is moved to the dead letter queue (stored in `_kora_webhook_delivery` with status `dead_lettered`).

For self-hosted Kora: dead-lettered deliveries are visible in the admin UI and can be replayed manually.

For Kora Cloud with Cloudflare Queues: dead-lettered events go to a Cloudflare Queue with configurable TTL (default: 7 days). They can be replayed from the Cloud dashboard.

### Automatic extension disabling

After an extension has **15 consecutive dead-lettered deliveries**, the engine automatically sets `is_active = false` on the extension and emits `kora._system.webhook_delivery_failed`. This is analogous to Stripe disabling endpoints after 3 days of continuous failure and Shopify auto-deleting subscriptions after 8 consecutive failures.

The extension owner receives an email notification (if configured). The extension can be re-enabled from the admin UI after the owner fixes their endpoint.

### Delivery timeout

HTTP requests to extension endpoints have a **10-second timeout** (configurable). This is based on Svix's webhook timeout best practices: long enough for a serverless cold start + some processing, short enough that a hung endpoint doesn't block the delivery worker indefinitely.

---

## Signature Verification

### Signing algorithm

Kora uses **HMAC-SHA256** over the raw request body, following the exact pattern used by Stripe, GitHub, Shopify, and Frappe:

```
signing_payload = timestamp + "." + raw_body
signature = HMAC-SHA256(signing_secret, signing_payload)
header = "t={timestamp},v1={hex_signature}"
```

The `X-Kora-Signature` header contains:
```
X-Kora-Signature: t=1765981391,v1=a1b2c3d4e5f6...
```

### Verification code (Go)

```go
package webhook

import (
    "crypto/hmac"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/hex"
    "fmt"
    "strconv"
    "strings"
    "time"
)

func VerifySignature(body []byte, header string, secrets []string, tolerance time.Duration) error {
    // Parse header: "t=1765981391,v1=abc123..."
    var timestamp int64
    var signatures []string
    for _, part := range strings.Split(header, ",") {
        kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
        if len(kv) != 2 { continue }
        switch kv[0] {
        case "t":
            timestamp, _ = strconv.ParseInt(kv[1], 10, 64)
        case "v1":
            signatures = append(signatures, kv[1])
        }
    }

    // Replay protection: 5-minute tolerance window
    if time.Since(time.Unix(timestamp, 0)).Abs() > tolerance {
        return fmt.Errorf("timestamp outside tolerance window")
    }

    // Build expected signed payload
    signedPayload := fmt.Sprintf("%d.%s", timestamp, string(body))

    // Verify against all active secrets (supports rotation)
    for _, secret := range secrets {
        for _, sig := range signatures {
            mac := hmac.New(sha256.New, []byte(secret))
            mac.Write([]byte(signedPayload))
            expected := hex.EncodeToString(mac.Sum(nil))

            // Constant-time comparison — non-negotiable
            if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) == 1 {
                return nil
            }
        }
    }
    return fmt.Errorf("no valid signature found")
}
```

### Why HMAC-SHA256 and not API key-only

The research shows every major platform (Stripe, GitHub, Shopify, Frappe) uses HMAC-SHA256, not a simple API key. The reason:

1. **API keys in headers are trivially replayable.** An attacker who intercepts one request can replay it indefinitely. HMAC with embedded timestamp prevents this.
2. **API keys leak through logs.** `Authorization: Bearer sk-...` appears in server logs, proxy logs, and error messages. HMAC signatures are per-request and useless if logged.
3. **API keys don't prove payload integrity.** A MITM could modify the body and the key would still be valid. HMAC detects any modification.

The research found multiple CVEs (CVE-2022-36885, CVE-2022-43412) from non-constant-time comparison in webhook verifiers. Constant-time comparison is non-negotiable.

### Secret rotation (zero-downtime)

Following Stripe's pattern:

1. When rotating a secret, the OLD secret is retained for **24 hours**.
2. During the overlap, the `X-Kora-Signature` header contains **multiple `v1=` entries**, one per active secret.
3. The extension verifies against ALL active secrets in its config, succeeding if ANY matches.
4. After the 24-hour window, the old secret is removed and only the new one signs.

For the extension developer, the workflow is:

```
1. Generate new secret in Kora admin UI → both old and new are now active
2. Add new secret to extension's config (keep old secret too)
3. Deploy extension
4. Wait for all instances to pick up new config (< 5 minutes)
5. Remove old secret from extension's config
6. Click "Deactivate old secret" in Kora admin UI
```

The engine never forces a hard cutover — it's always the extension developer's choice when to stop accepting the old secret.

### Extension-side verification (TypeScript SDK)

```typescript
import { verifySignature } from '@kora/extension-sdk'

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const body = await request.text()
    const signature = request.headers.get('X-Kora-Signature')

    if (!signature) {
      return new Response('Missing signature', { status: 401 })
    }

    const valid = await verifySignature(body, signature, [env.KORA_WEBHOOK_SECRET])
    if (!valid) {
      return new Response('Invalid signature', { status: 401 })
    }

    const event = JSON.parse(body)
    // ... process event ...
  }
}
```

---

## Extension Registry

### `_kora_extension` system table

```sql
CREATE TABLE `_kora_extension` (
  name            VARCHAR(255) PRIMARY KEY,
  display_name    VARCHAR(255) NOT NULL,
  description     TEXT,
  endpoint_url    VARCHAR(1024) NOT NULL,
  secret_hash     VARCHAR(64) NOT NULL,         -- SHA-256 of active signing secret
  old_secret_hash VARCHAR(64),                  -- SHA-256 of old secret (during rotation)
  old_secret_expires_at DATETIME,               -- 24h window for rotation
  secret_count    INT NOT NULL DEFAULT 1,       -- incremented on rotation
  is_active       BOOLEAN NOT NULL DEFAULT 1,
  subscriptions   JSON NOT NULL,                -- array of event subscriptions
  api_permissions JSON NOT NULL,                -- scoped permissions for callback API
  retry_schedule  JSON,                         -- custom retry config (null = default)
  timeout_sec     INT NOT NULL DEFAULT 10,
  headers         JSON,                         -- custom headers added to every delivery
  delivery_stats  JSON,                         -- rolling window stats
  consecutive_failures INT NOT NULL DEFAULT 0,
  installed_at    DATETIME NOT NULL,
  updated_at      DATETIME NOT NULL,
  last_delivery_at DATETIME,
  last_error      TEXT
);
```

### Subscription configuration

Each extension declares which events it wants to receive, with optional filters:

```yaml
# Extension registration
name: fieldwork-notifications
display_name: Fieldwork Notification Service
description: Sends Slack and email notifications for Work Order state changes.
endpoint_url: https://fieldwork-notifications.workers.dev/webhook
secret: "${KORA_WEBHOOK_SECRET}"  # generated by Kora, shown once
is_active: true

subscriptions:
  - event: kora.work_order.after_insert
    filter: {}                               # all Work Order inserts

  - event: kora.work_order.workflow_transition
    filter:
      to_state: ["Approved", "Rejected"]     # only these transitions

  - event: kora.work_order.after_save
    filter:
      field_changed: ["assigned_technician"] # only when technician is reassigned

  - event: kora._auth.login_failed
    filter: {}                               # all auth failures

api_permissions:
  - doctype: Work Order
    operations: [read]
  - doctype: Customer
    operations: [read]
  - doctype: Service Report
    operations: [create, read]
  - doctype: Notification Log
    operations: [create]

retry_schedule:                              # optional custom retry schedule
  max_attempts: 5
  max_window_hours: 12

headers:
  X-Custom-Auth: "${CUSTOM_AUTH_HEADER}"     # injected into every delivery
```

### Filter operators

| Filter type | Syntax | Example |
|---|---|---|
| Exact match | `{field: "value"}` | `filter: {doctype: "Work Order"}` |
| One of | `{field: ["a", "b"]}` | `filter: {to_state: ["Approved", "Rejected"]}` |
| Field changed | `{field_changed: ["fieldname"]}` | Only fires when that field's value changed |
| Any field changed | `{field_changed: ["*"]}` | Fires when any field changes |
| Negation | `{not: {field: "value"}}` | `filter: {not: {doctype: "_kora_user"}}` |

### CLI management

```bash
kora extension register ./my-extension/extension.yaml --site fieldwork.local
kora extension list --site fieldwork.local
kora extension show my-extension --site fieldwork.local
kora extension enable my-extension --site fieldwork.local
kora extension disable my-extension --site fieldwork.local
kora extension rotate-secret my-extension --site fieldwork.local
kora extension deliveries my-extension --site fieldwork.local --last 50
kora extension replay my-extension --event-id evt_01JQZTX2M...
kora extension stats my-extension --site fieldwork.local
kora extension remove my-extension --site fieldwork.local
```

---

## High Volume Handling

### The problem

A bulk import of 10,000 Work Orders generates 10,000 `kora.work_order.after_insert` events. If each webhook delivery takes 200ms, a single-threaded worker would take 33 minutes to deliver them all. An extension subscribed to all events would be flooded.

### Solution: Decoupled worker pool + coalescing

```
Bulk Import API Call (writes 10,000 documents)
    │
    ▼
Each Insert emits event → ChannelBus (10,000 capacity)
    │
    ▼
Worker pool (N goroutines, N = GOMAXPROCS)
    │
    ├── Batch events by (extension, doctype, event_type)
    ├── Coalesce dupes: multiple kora.stock_item.after_update
    │   on the SAME document merge into one (keep latest)
    │
    ├── Deliver coalesced batch
    └── Respect per-extension rate limits
```

### Coalescing strategy

```
Events emitted:   WO-001.after_save, WO-001.after_save, WO-001.after_save
                                 (same document updated 3 times in 1 batch)
Coalesced:        WO-001.after_save (latest state only)
```

The coalescing window is **30 seconds**. Events for the same (doctype, document_name) within that window are merged. The delivered event always carries the latest document state (fetched from the database at delivery time, not at emit time).

### Per-extension rate limiting

Each extension has configurable rate limits:

```yaml
rate_limits:
  max_deliveries_per_minute: 60      # 1/second sustained
  max_concurrent_deliveries: 3       # no more than 3 in-flight at once
  max_payload_size_kb: 64            # individual event cap
```

The delivery worker uses a token bucket per extension. When an extension hits its rate limit, deliveries are queued (not dropped) and drained as tokens replenish. The queue is bounded at **10,000 events per extension** — above that, events are dropped with `kora._system.webhook_delivery_failed` emitted.

### Database polling for missed events

For extensions that were disabled or unreachable for an extended period (beyond the retry window), Kora provides a **catch-up API**:

```
GET /api/system/events?since=2026-06-17T00:00:00Z&doctype=Work Order&event=kora.work_order.after_insert
```

This returns all events matching the query in the specified time window. Extensions can use this to self-heal after extended downtime without manual replay.

### Worker pool configuration

```go
// config/engine.go
type WebhookConfig struct {
    WorkerCount       int           // default: runtime.GOMAXPROCS(0)
    DeliveryTimeout   time.Duration // default: 10s
    RetrySchedule     RetrySchedule
    MaxQueueSize      int           // default: 10000 per extension
    CoalesceWindow    time.Duration // default: 30s
    MaxPayloadBytes   int           // default: 65536 (64KB)
}
```

---

## Extension API Token Scoping

### The gap in industry practice

The second deep research found that **no major platform publicly documents its internal webhook-to-API token scoping model.** Shopify has OAuth scopes (`read_products`, `write_orders`) but these are for third-party apps in the App Store, not for webhook receivers. Stripe uses API keys with restricted permissions but the scoping model is internal.

This means Kora needs to make original design decisions here, guided by first principles.

### Kora's model: DocType × Operation scoping

Each extension gets a dedicated API token (a JWT with embedded permissions). The token's scope is declared in `api_permissions` at registration time:

```json
{
  "sub": "extension:fieldwork-notifications",
  "site": "fieldwork.local",
  "permissions": [
    {"doctype": "Work Order", "operations": ["read"]},
    {"doctype": "Customer", "operations": ["read"]},
    {"doctype": "Service Report", "operations": ["create", "read"]}
  ],
  "iat": 1765981391,
  "exp": 1766067791   // 24h TTL, auto-refreshed
}
```

The engine's permission middleware enforces this:

```go
// auth/extension_guard.go
func ExtensionGuard(token *ExtensionToken, doctype string, operation string) error {
    for _, perm := range token.Permissions {
        if perm.Doctype == doctype {
            for _, op := range perm.Operations {
                if op == operation || op == "all" {
                    return nil
                }
            }
        }
    }
    return fmt.Errorf("extension %s has no %s permission on %s", token.Sub, operation, doctype)
}
```

**Operations**: `create`, `read`, `update`, `delete`, `submit`, `cancel`, `all`.

**Wildcards**: `{"doctype": "*", "operations": ["read"]}` allows reading all DocTypes. This is the default for new extensions but can be restricted.

### Row-level scoping (Phase 2)

In Phase 1, extensions can access any document in the DocTypes they have permission for. Row-level scoping — where an extension can only see records it created — is a Phase 2 feature.

```yaml
# Phase 2 — not implemented yet
api_permissions:
  - doctype: Service Report
    operations: [read, update]
    row_filter: "created_by_extension"   # only records where modified_by = extension name
```

### Token lifecycle

1. On extension registration, Kora generates a JWT signing key specifically for that extension.
2. The extension receives a long-lived API token at registration time (shown once).
3. The engine auto-refreshes the extension's token every 24 hours (transparent to the extension if using the SDK).
4. Rotating the extension secret also rotates the API token signing key.
5. Disabling an extension immediately invalidates all its tokens.

---

## Cloudflare Workers Runtime

### Architecture

For Kora Cloud, each tenant's extensions run on **Workers for Platforms**. This is the synthesis finding from Research 1 (Finding 7): Workers for Platforms serves as the unified compute layer for both tenant workloads and extension execution.

```
                      Cloudflare Edge
                            │
                    Dispatch Worker
                    (tenant router)
                            │
            ┌───────────────┼───────────────┐
            │               │               │
      Kora Engine    User Workers    User Workers
      (API + UI)     (tenant app)    (extensions)
                      per-tenant      per-tenant
```

The Dispatch Worker receives all requests for `*.kora.dev` and custom domains, resolves the tenant from the Host header via KV, and routes to the correct User Worker.

### Two paths, same runtime

A tenant's User Worker can serve BOTH:

1. **Custom app code**: The tenant writes a Worker that serves their custom frontend, API endpoints, or server-rendered pages. This is their custom application code.

2. **Webhook handlers**: The same Worker handles incoming webhook events from Kora. The webhook endpoint is the Worker's fetch handler. Webhook events arrive as POST requests with `X-Kora-Event` headers.

```typescript
// tenant-worker/src/index.ts
export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url)

    // Webhook path: Kora delivers events here
    if (url.pathname === '/webhook' && request.method === 'POST') {
      const signature = request.headers.get('X-Kora-Signature')
      const body = await request.text()
      if (!await verifySignature(body, signature, [env.KORA_WEBHOOK_SECRET])) {
        return new Response('Unauthorized', { status: 401 })
      }
      const event = JSON.parse(body)
      await handleKoraEvent(event, env)
      return new Response('ok', { status: 200 })
    }

    // Custom app: tenant's own logic
    if (url.pathname === '/dashboard') {
      return renderDashboard(env)
    }

    return new Response('Not found', { status: 404 })
  }
}
```

### Per-customer resource limits

Following Workers for Platforms' `limits` API, each tenant's Worker has resource caps that depend on their Kora Cloud pricing tier:

| Tier | CPU time (ms) | Subrequests | Memory |
|---|---|---|---|
| Free | 10 | 50 | 128 MB |
| Starter | 50 | 200 | 128 MB |
| Pro | 200 | 500 | 128 MB |
| Enterprise | 500 | 1000 | 128 MB |

### Complex extensions with Workflows

For multi-step extension logic (generate PDF, wait for external API, send reminders), tenants use Cloudflare Workflows:

```typescript
// tenant-worker/src/invoice-workflow.ts
import { WorkflowEntrypoint, WorkflowEvent, WorkflowStep } from 'cloudflare:workers'

interface InvoiceParams {
  work_order_name: string
  site: string
}

export class InvoiceWorkflow extends WorkflowEntrypoint<Env, InvoiceParams> {
  async run(event: WorkflowEvent<InvoiceParams>, step: WorkflowStep) {
    // Step 1: Fetch Work Order from Kora API
    const doc = await step.do('fetch', async () => {
      const res = await fetch(`https://${event.payload.site}/api/resource/Work Order/${event.payload.work_order_name}`, {
        headers: { Authorization: `Bearer ${this.env.KORA_TOKEN}` }
      })
      return res.json()
    })

    // Step 2: Generate PDF (external service)
    const pdfUrl = await step.do('generate-pdf', async () => {
      const res = await fetch('https://pdf-service.example.com/invoice', {
        method: 'POST', body: JSON.stringify(doc.data)
      })
      return (await res.json()).url
    })

    // Step 3: Attach PDF to Work Order via Kora API
    await step.do('attach-pdf', async () => {
      await fetch(`https://${event.payload.site}/api/resource/Work Order/${event.payload.work_order_name}`, {
        method: 'PUT',
        headers: { Authorization: `Bearer ${this.env.KORA_TOKEN}` },
        body: JSON.stringify({ invoice_pdf: pdfUrl })
      })
    })

    // Step 4: Wait 2 days, send payment reminder
    await step.sleep('reminder-delay', '2 days')
    await step.do('send-reminder', async () => {
      // ... send email/SMS via external service
    })
  }
}
```

For self-hosted Kora, extensions run wherever the user deploys them — any HTTP server, AWS Lambda, or a local process. Cloudflare is the recommended but not required runtime.

---

## App Packs (Config-Only)

### Concept

App Packs are the other extension mechanism — no webhooks, no compute, no executable code. They are bundles of DocType configs, permissions, workflows, and fixtures that extend Kora's data model.

App Packs and webhook extensions are complementary:
- `kora-crm` (App Pack): defines Lead, Opportunity, Pipeline Stage DocTypes and workflows
- `kora-crm-slack` (Webhook Extension): receives `kora.lead.after_insert` → posts to Slack

### Structure

```
kora-crm/
├── plugin.yaml
├── doctypes/
│   ├── lead.yaml
│   ├── opportunity.yaml
│   └── pipeline_stage.yaml
├── permissions.yaml
├── workflows/
│   └── lead_workflow.yaml
├── fixtures/
│   ├── roles.yaml
│   └── pipeline_stages.yaml
└── README.md
```

### Manifest (`plugin.yaml`)

```yaml
name: kora-crm
version: 1.2.0
display_name: CRM
description: Customer relationship management for Kora.
author: Kora Community
homepage: https://github.com/kora-hub/kora-crm
license: MIT
kora_version: ">=1.0.0"

depends_on:
  - name: kora-core
    version: ">=1.0.0"

modules:
  - name: CRM
    label: CRM
    icon: users
    color: "#4F46E5"
    doctypes:
      - Lead
      - Opportunity
      - Pipeline Stage

extensions:
  - name: kora-crm-slack       # companion webhook extension (optional)
    install_prompt: "Would you like to enable Slack notifications for new leads?"
```

### Installation

```bash
kora pack install kora-crm                          # from Kora Hub
kora pack install ./kora-crm/                       # from local directory
kora pack install github.com/kora-hub/kora-crm       # from git
kora pack install github.com/kora-hub/kora-crm@v1.2.0 # pinned version

kora pack list
kora pack remove kora-crm
kora pack upgrade kora-crm
```

Installation creates a new config version with the App Pack's DocTypes, permissions, workflows, and fixtures. The engine runs migration automatically. Removal warns if data exists in the pack's DocTypes and requires a `--force` flag to drop tables with data.

### Safety rules

- Cannot define DocTypes starting with `_kora_` (reserved for system)
- Cannot override existing DocTypes unless `extends` is declared in the manifest
- Cannot override system permissions (System Manager role)
- All fixtures go through the same validation as regular API writes
- Installation is a config version change — it appears in version history and can be rolled back

---

## Admin UI

### Extensions page (`/workspace/admin/extensions`)

A dedicated page under the Administrator section:

```
┌──────────────────────────────────────────────────────────┐
│  Extensions                                               │
│                                                            │
│  ┌──────────────────────────────────────────────────────┐ │
│  │  🔌 Webhook Extensions                 [+ Register]  │ │
│  │                                                        │ │
│  │  ┌──────────────────────────────────────────────┐     │ │
│  │  │ fieldwork-notifications         ● Active      │     │ │
│  │  │ Sends Slack/email notifications               │     │ │
│  │  │ Endpoint: ...workers.dev/webhook              │     │ │
│  │  │ Subscriptions: 3 events                       │     │ │
│  │  │ Success rate: 99.7% (last 24h)               │     │ │
│  │  │ Last delivery: 2 min ago                      │     │ │
│  │  │                            [View] [Disable]   │     │ │
│  │  └──────────────────────────────────────────────┘     │ │
│  │                                                        │ │
│  │  ┌──────────────────────────────────────────────┐     │ │
│  │  │ mpesa-integration              ○ Disabled     │     │ │
│  │  │ M-Pesa payment processing                     │     │ │
│  │  │ 15 consecutive failures — auto-disabled       │     │ │
│  │  │                            [View] [Re-enable] │     │ │
│  │  └──────────────────────────────────────────────┘     │ │
│  └──────────────────────────────────────────────────────┘ │
│                                                            │
│  ┌──────────────────────────────────────────────────────┐ │
│  │  📦 App Packs                         [+ Install]    │ │
│  │                                                        │ │
│  │  kora-crm v1.2.0          ● Installed                 │ │
│  │  kora-mpesa v1.0.0        ● Installed                 │ │
│  │  kora-inventory v0.9.0    ○ Available (Hub)           │ │
│  └──────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

### Extension detail view

```
┌──────────────────────────────────────────────────────────┐
│  fieldwork-notifications                                 │
│  Status: ● Active    Endpoint: ...workers.dev/webhook    │
│                                                            │
│  [Overview] [Deliveries] [Settings] [Secret]              │
│                                                            │
│  ── Deliveries (last 50) ────────────────────────────     │
│                                                            │
│  Event ID        Event                        Status      │
│  evt_01JQ...     kora.work_order.after_submit  ✓ 200      │
│  evt_01JQ...     kora.work_order.after_submit  ✓ 200      │
│  evt_01JQ...     kora.work_order.after_submit  ✗ 500      │
│  evt_01JQ...     kora.work_order.after_submit  ↻ Retry 3  │
│  evt_01JQ...     kora._auth.login_failed       ✓ 200      │
│                                                            │
│  [Replay selected] [Export CSV]              [Load more]   │
└──────────────────────────────────────────────────────────┘
```

### Delivery log

Every delivery attempt is logged with:
- Event ID, event type, timestamp
- HTTP status code, response time
- Response body (first 1KB)
- Retry attempt number
- Error message (if failed)

This matches Shopify's delivery log UI, which the research confirmed as the best debugging tool for extension developers.

---

## Developer Experience

### `@kora/extension-sdk` (TypeScript)

```typescript
import { KoraExtension } from '@kora/extension-sdk'

const ext = new KoraExtension({
  secret: process.env.KORA_WEBHOOK_SECRET,
  secrets: [process.env.KORA_WEBHOOK_SECRET],  // array for rotation
})

// Event handler with filter
ext.on('kora.work_order.workflow_transition', {
  filter: { to_state: 'Approved' },
  handler: async (event, { kora }) => {
    // kora is a pre-authenticated API client
    const customer = await kora.get('Customer', event.data.document.customer)
    await kora.create('Service Report', {
      work_order: event.data.name,
      customer_name: customer.name,
      status: 'Pending',
    })
  }
})

// Wildcard handler
ext.on('kora.work_order.*', {
  handler: async (event) => {
    console.log(`Work Order event: ${event.event}`)
  }
})

export default ext.fetch  // standard Cloudflare Worker fetch handler
```

The SDK handles:
- Signature verification (constant-time, multi-secret for rotation)
- Filter matching
- Error handling and logging
- Kora API client with automatic token refresh

### Local development

```bash
# Terminal 1: Start Kora engine
kora serve --site fieldwork.local

# Terminal 2: Start extension dev server with live event forwarding
kora extension dev my-extension --site fieldwork.local --forward http://localhost:8787

# This:
# 1. Opens a cloudflared tunnel to localhost:8787
# 2. Temporarily registers the tunnel URL as the extension endpoint
# 3. Forwards live events to the local worker
# 4. Shows a live delivery log in the terminal
```

### Event replay

```bash
# Replay a single event
kora extension replay my-extension --event-id evt_01JQZTX2M...

# Replay all events in a time window
kora extension replay my-extension --since 2026-06-17T00:00:00Z --doctype "Work Order"

# Replay failed deliveries only
kora extension replay my-extension --status dead_lettered --last 20
```

### Scaffolding

```bash
npm create kora-extension@latest my-extension
# Scaffolds:
#   my-extension/
#   ├── src/index.ts           (handler boilerplate)
#   ├── extension.yaml          (Kora registration manifest)
#   ├── wrangler.toml           (Cloudflare Workers config)
#   ├── package.json
#   └── tsconfig.json
```

---

## Security Model

### Threat model

| Threat | Mitigation |
|---|---|
| Replayed webhook | HMAC timestamp + 5-minute tolerance — expired timestamps rejected |
| Modified payload | HMAC-SHA256 detects any body modification |
| Stolen webhook secret | Multi-signature rotation (24h overlap), secret hashed at rest |
| Extension accessing unauthorized DocType | JWT with embedded DocType×Operation scoping |
| Extension token leaked | Token auto-expires in 24h, immediate invalidation on disable |
| Slow extension blocking workers | 10s timeout per delivery, separate worker pool from API |
| Extension flooding API with callbacks | Per-extension rate limiting on API token |
| Malicious App Pack overriding system | Cannot define `_kora_*` DocTypes, validation on import |
| Timing attack on signature check | `subtle.ConstantTimeCompare` — non-negotiable |

### Secrets management

- Webhook signing secrets are generated by Kora (not user-provided)
- Shown once at registration time
- Stored as SHA-256 hash in `_kora_extension.secret_hash`
- Never logged, never included in API responses
- Rotation generates a new secret, keeps old one for 24 hours
- Old secret hash stored in `_kora_extension.old_secret_hash` with expiry

### Extension isolation

- Extensions have NO access to the engine's internal state, database, or Redis
- The ONLY channel from extension to Kora is the public REST API
- API tokens are scoped to declared DocTypes and operations
- Extension API calls are rate-limited (configurable, default 1000 req/min)
- Extension API calls are logged to the audit table

---

## What We Explicitly Rejected

| Idea | Why rejected |
|---|---|
| **gRPC plugins that block the engine** | Couples hot path to external code; no major platform uses this |
| **In-process Go plugins** | Requires recompile, no runtime extensibility |
| **API key-only webhook auth** | Trivially replayable, no payload integrity, leaks through logs |
| **Synchronous `before_*` hooks that can reject** | Blocks engine on external code; violates "extensions don't participate" |
| **19-retry, 48-hour schedule (old Shopify)** | Shopify themselves abandoned this for 8-retry, 4-hour |
| **Per-tenant dedicated Redis for webhook delivery** | Overkill — shared Redis with per-tenant keyspace prefix is sufficient |
| **Thin payloads (identifier-only)** | Forces N+1 API calls from every extension consumer |
| **Event cascading (parent events for child changes)** | Stripe explicitly doesn't do this; it creates event storms |
| **Custom plugin binary format** | Cloudflare Workers for Platforms provides industry-standard isolation |

---

## Open Questions

1. **128 MB Worker memory**: Can Workers for Platforms' 128 MB per-Worker limit support ORM-like operations that a config-driven engine's extensions might need (PDF generation with large documents, bulk data processing)? If not, do we fall back to Cloudflare Containers or dedicated VMs for heavy extensions?

2. **Same Worker for sync + async**: Can the same Dispatch Worker serve both user-facing API requests (sync, <100ms latency) and webhook-triggered background jobs (async, bursty, can take seconds) without resource contention, or should they use separate dispatch namespaces?

3. **Stripe Metronome data pipeline**: What aggregation window and event schema minimizes the race between usage overage detection and plan changes mid-cycle?

4. **Event replay for schema changes**: If a DocType field is renamed or removed, what happens when a consumer tries to replay an old event that references retired fields? Do we need an event schema registry with backward compatibility guarantees?

5. **Webhook delivery and GDPR**: For EU customers on Kora Cloud, webhook payloads contain document data that may include PII. Does the webhook delivery path need the same data residency guarantees as the database, or is it the extension owner's responsibility?

6. **App Pack dependency resolution**: When App Pack A depends on App Pack B v1.0, and Pack C depends on Pack B v2.0, can both coexist? Or does the engine enforce a single version of Pack B per site (like npm peer dependencies)?

---

## Appendix: Retry Schedules — Platform Comparison

| Platform | Retries | Window | Backoff | Dead letter | Auto-disable |
|---|---|---|---|---|---|
| **Stripe (live)** | Continuous | 3 days | Exponential | No (just stops) | Yes, after 3 days |
| **Stripe (sandbox)** | 3 | "Few hours" | Exponential | No | No |
| **GitHub** | Continuous | 72 hours | Exponential | No | No |
| **Shopify** | 8 | 4 hours | Exponential | No | Yes, auto-delete subscription |
| **Azure Event Grid** | 1-30 (config) | 1-1440 min (config) | 10-step fixed + jitter | Yes, configurable threshold | No (dead letter instead) |
| **Svix (webhook SaaS)** | Continuous | User-configured | Exponential | Yes | Yes |
| **Kora (default)** | 8 | ~35 hours | Exponential + jitter [0.5-1.5] | Yes, after 8 failures | Yes, after 15 consecutive DL |

## Appendix: Signature Headers — Platform Comparison

| Platform | Header name | Format | Encoding |
|---|---|---|---|
| **Stripe** | `Stripe-Signature` | `t={ts},v1={sig}[,v1={sig2}]` | Hex |
| **GitHub** | `X-Hub-Signature-256` | `sha256={sig}` | Hex |
| **Shopify** | `X-Shopify-Hmac-Sha256` | `{sig}` | Base64 |
| **Frappe** | `X-Frappe-Webhook-Signature` | `{sig}` | Base64 |
| **Kora** | `X-Kora-Signature` | `t={ts},v1={sig}[,v1={sig2}]` | Hex |

Kora follows Stripe's format because:
1. Embedded timestamp enables replay protection (GitHub/Shopify don't embed timestamp)
2. Multiple `v1=` entries enable zero-downtime secret rotation (only Stripe has this)
3. Hex encoding is more debuggable than base64 (visible in logs)

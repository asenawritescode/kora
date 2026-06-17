# Kora — Networking Layer

## Overview

Kora's networking layer is entirely **Go-native**. No nginx, no Caddy, no external reverse proxy. Everything runs in the Kora binary.

This design was chosen for:
- **Simplicity** — one binary, one process
- **Go-native** — uses stdlib + x/ packages
- **Programmable** — middleware is Go code, not config files
- **Lightweight** — zero external dependencies at runtime

## Architecture

```
                    Internet
                       │
               ┌───────┴───────┐
               │  TLS (autocert)│ ← golang.org/x/crypto/acme/autocert
               │  HTTP→HTTPS   │ ← :80 redirect (automatic)
               │  HTTP/2       │ ← net/http (automatic with TLS)
               └───────┬───────┘
                       │
         ┌─────────────┴─────────────┐
         │     Middleware Stack       │
         │                            │
         │  1. gin.Recovery           │ ← Panic recovery
         │  2. RequestID              │ ← X-Request-Id: <ulid>
         │  3. SecurityHeaders        │ ← HSTS, CSP, XFO, XSS
         │  4. CORS                   │ ← Access-Control-*
         │  5. SiteRouter             │ ← Host → DB + Registry
         │  6. RateLimiter            │ ← Token bucket per user
         │  7. AuthMiddleware         │ ← Session cookie
         │  8. CSRFMiddleware         │ ← Double-submit cookie
         └─────────────┬─────────────┘
                       │
              ┌────────┼────────┐
              ▼        ▼        ▼
           MySQL    Redis    Files
```

## Component Details

### TLS + Autocert

Kora uses `golang.org/x/crypto/acme/autocert` for automatic Let's Encrypt certificates.

**Modes:**

| Mode | Behavior |
|---|---|
| `off` | Plain HTTP (development) |
| `auto` | Let's Encrypt via autocert |
| `manual` | User-provided cert files |

**Auto mode flow:**
1. Kora starts, autocert sees no certificate for the domain
2. Let's Encrypt sends an HTTP-01 challenge
3. Autocert responds to the challenge automatically on port 80
4. Let's Encrypt issues the certificate
5. Certificate is cached in the `certs/` directory
6. Renewal happens automatically 30 days before expiry

**Configuration (env vars):**
```
KORA_TLS_MODE=auto
KORA_TLS_EMAIL=admin@example.com
```

### Site Router

Maps `Host` headers to site contexts (database connection + registry).

```
Request: Host: acme.com → site = acme.com → DB = acme_db
Request: Host: beta.io  → site = beta.io  → DB = beta_db
Request: Host: localhost → falls back to default site
Request: Host: 127.0.0.1 → falls back to default site
```

**Fallback behavior:** If the Host header doesn't match any configured domain, and it's an IP address or `localhost`, the default site is used. This enables development without DNS configuration.

**Multi-domain sites:** A single site can respond to multiple domains. Domains are configured via the console UI or API — persisted in `_kora_config_version.config`.

### Security Headers

Provided by `github.com/gin-contrib/secure`. Every response includes:

```http
Content-Security-Policy: default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://unpkg.com https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; img-src 'self' data:
Referrer-Policy: same-origin
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-Xss-Protection: 1; mode=block
```

When TLS is enabled:
```http
Strict-Transport-Security: max-age=31536000; includeSubDomains
```

### CORS

Provided by `github.com/gin-contrib/cors`. Configurable per site:

```yaml
# site_config.yaml
cors_origins:
  - https://app.example.com
  - https://admin.example.com
```

Default: allow all origins (development). In production, restrict to specific origins.

### Rate Limiting

Uses `golang.org/x/time/rate` — an in-process token bucket implementation.

**Behavior:**
- Default: 100 requests/second per user
- Burst: 20 requests
- Key: `{site}:{user}:{endpoint}:{method}`
- Anonymous requests keyed by IP: `{site}:ip:{client_ip}`
- Cleanup: idle entries purged every 5 minutes
- Response: `429 Too Many Requests`

**Configuration (env vars):**
```
KORA_RATE_LIMIT=100
KORA_RATE_BURST=20
```

### CSRF Protection

Double-submit cookie pattern.

**Flow:**
1. On the first GET request, a random CSRF token is set as a cookie: `kora_csrf=<token>`
2. The cookie has `HttpOnly=false` so JavaScript can read it
3. On POST/PUT/DELETE, the client sends the same token in the `X-Kora-CSRF-Token` header
4. The server verifies the header value matches the cookie value
5. If they don't match → `403 Forbidden`

**Why this pattern:**
- A malicious site cannot read cookies from another domain (same-origin policy)
- Therefore it cannot include the CSRF token in its requests
- Simple to implement — no server-side token storage needed

### Request ID

Every request gets a unique ULID (or uses the one from `X-Request-Id` header if provided by an upstream proxy). ULIDs are 26-character, lexicographically sortable, URL-safe identifiers — an upgrade from UUID for better index performance. The ID is returned in the response header and included in all log messages.

### Graceful Shutdown

On SIGINT or SIGTERM:
1. Stop accepting new connections
2. Drain in-flight requests (30-second timeout)
3. Stop the scheduler
4. Close all database connections

```go
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
sig := <-sigCh
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
srv.Shutdown(ctx)
for _, site := range sites { site.DB.Close() }
```

## Request Lifecycle

```
1. CLIENT → TCP connection
2. TLS handshake (autocert provisions cert if needed)
3. HTTP request received
4. Recovery middleware (recover from panics)
5. RequestID: generate or propagate X-Request-Id
6. SecurityHeaders: set response headers
7. CORS: check Origin, set Access-Control-* headers
8. SiteRouter: Host header → site DB + registry
9. RateLimiter: check token bucket
10. Auth: kora_sid cookie → session → user + roles
11. CSRF: verify X-Kora-CSRF-Token header (POST/PUT/DELETE only)
12. Permission check: user roles → allowed operation?
13. Handler: business logic
14. ORM: database operation
15. Response: JSON envelope with meta
16. Log entry with request_id, user, duration, status
```

## Library Dependencies

| Concern | Library | Type |
|---|---|---|
| HTTP server | `net/http` | stdlib |
| HTTP router | `github.com/gin-gonic/gin` | 3rd party |
| TLS | `crypto/tls` | stdlib |
| Let's Encrypt | `golang.org/x/crypto/acme/autocert` | x/ stdlib |
| Security headers | `github.com/gin-contrib/secure` | 3rd party |
| CORS | `github.com/gin-contrib/cors` | 3rd party |
| Rate limiting | `golang.org/x/time/rate` | x/ stdlib |
| CSRF | Custom (40 lines) | built-in |
| Site routing | Custom (100 lines) | built-in |
| Request ID | Custom (15 lines) | built-in |
| Graceful shutdown | `os/signal` + `net/http` | stdlib |

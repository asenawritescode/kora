# Kora — REST API Reference

## Authentication

All `/api/resource/*` and `/api/system/*` endpoints require authentication.

### Login

```http
POST /api/auth/login
Content-Type: application/json

{
  "email": "admin@fieldwork.local",
  "password": "admin123"
}
```

Response:
```json
{
  "data": {
    "name": "Administrator",
    "email": "admin@fieldwork.local",
    "full_name": "Administrator",
    "roles": ["Administrator"]
  },
  "sid": "abc123..."
}
```

The session ID is set as a cookie (`kora_sid`) and also returned in the response body. For browser clients, the cookie is used automatically. For API clients, pass it as:

```http
Cookie: kora_sid=abc123...
```
or
```http
Authorization: Bearer abc123...
```

### CSRF Protection

State-changing requests (POST/PUT/DELETE) require a CSRF token. A token is automatically set as a cookie (`kora_csrf`) on your first GET request. Include it as a header:

```http
X-Kora-CSRF-Token: <token-value>
```

The header value must match the `kora_csrf` cookie value.

### Logout

```http
POST /api/auth/logout
Cookie: kora_sid=abc123...
```

### Get Current User

```http
GET /api/auth/me
Cookie: kora_sid=abc123...
```

---

## CRUD Endpoints

All endpoints follow the pattern `/api/resource/{DocType}`. The `{DocType}` name is case-sensitive and space-preserved (e.g., `/api/resource/Work%20Order`).

### List Documents

```http
GET /api/resource/Customer
```

**Query Parameters:**

| Parameter | Type | Default | Description |
|---|---|---|---|
| `limit` | int | 50 | Max results (max 500) |
| `offset` | int | 0 | Pagination offset |
| `order_by` | string | `modified DESC` | Sort column + direction |
| `fields` | JSON array | all | Fields to return: `["name","company_name"]` |
| `filters` | JSON array | none | Filter conditions |

**Filter format:** `[["field", "operator", value], ...]`

Supported operators: `=`, `!=`, `>`, `>=`, `<`, `<=`, `like`, `not like`, `in`, `not in`, `between`, `is`, `is not`

Example:
```http
GET /api/resource/Work%20Order?filters=[["status","in",["Approved","In Progress"]],["priority","=","High"]]&limit=25&offset=0
```

**Response:**
```json
{
  "data": [
    {
      "name": "CUST-0001",
      "company_name": "Acme Corp",
      "email": "info@acme.com",
      "doc_status": 0
    }
  ],
  "meta": {
    "doctype": "Customer",
    "total": 42
  }
}
```

### Get Document

```http
GET /api/resource/Customer/CUST-0001
```

**Response:**
```json
{
  "data": {
    "name": "CUST-0001",
    "company_name": "Acme Corp",
    "email": "info@acme.com",
    ...
  },
  "meta": {
    "doctype": "Customer"
  }
}
```

### Create Document

```http
POST /api/resource/Customer
Content-Type: application/json

{
  "company_name": "Acme Corp",
  "email": "info@acme.com",
  "phone": "555-0100",
  "city": "New York"
}
```

**Response:** `201 Created`
```json
{
  "data": {
    "name": "CUST-0002",
    "company_name": "Acme Corp",
    ...
  },
  "meta": {
    "doctype": "Customer"
  }
}
```

**With child table:**
```json
{
  "title": "Fix HVAC",
  "customer": "CUST-0001",
  "scheduled_date": "2026-07-15",
  "items": [
    {
      "equipment": "EQUI-0001",
      "description": "Annual maintenance",
      "estimated_hours": 2.0
    }
  ]
}
```

### Update Document

```http
PUT /api/resource/Customer/CUST-0001
Content-Type: application/json

{
  "phone": "555-0200",
  "city": "Boston"
}
```

Only the fields you send are updated. Read-only fields are silently ignored. Child tables are fully replaced (old rows deleted, new rows inserted).

**Response:** `200 OK`

### Delete Document

```http
DELETE /api/resource/Customer/CUST-0001
```

**Response:** `200 OK`
```json
{
  "data": {
    "message": "deleted"
  },
  "meta": {
    "doctype": "Customer"
  }
}
```

---

## Workflow Actions

```http
POST /api/resource/Work%20Order/WO-0001/workflow_action
Content-Type: application/json

{
  "action": "Submit for Approval"
}
```

**Response:** `200 OK` — document with updated status.
**Errors:** `400` — transition not available (wrong role, condition not met, missing required fields).

---

## AI Chat

### Send Chat Message

```
POST /api/chat
```

Send a natural language message to the AI assistant. The AI auto-generates tool definitions from the doctype registry and executes CRUD operations via a multi-round `finish_reason`-driven loop.

**Request:**
```json
{
  "message": "Create a customer with ACME Corp, john@acme.com, 0701002000",
  "history": [
    {"role": "user", "content": "..."},
    {"role": "assistant", "content": "..."}
  ],
  "model": "gpt-4o"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `message` | string | Yes | Natural language instruction |
| `history` | ChatMessage[] | No | Previous conversation turns (capped at configurable limit, default 20) |
| `model` | string | No | Override default model (e.g., `gpt-4o`, `claude-sonnet-4-6`, `deepseek-v4-pro`) |

**Response:** `200 OK`
```json
{
  "reply": "Created Customer \"ACME Corp\" (CUST-0002).",
  "action": "listed 3 customers"
}
```

**Provider Support:** OpenAI GPT-4o, DeepSeek V4, Anthropic Claude. Configure API keys via the workspace UI at `/workspace/admin/secrets` (dropdown selector + key input) or via CLI: `./kora secret set --site X --key <provider>_api_key --value sk-...`.

**AI Tools Generated Per DocType:**

| Tool | Purpose |
|------|---------|
| `<doctype>_find` | Search by field values (duplicate check before create) |
| `<doctype>_list` | List documents (paginated, markdown table) |
| `<doctype>_get` | Get single document by name |
| `<doctype>_create` | Create new document |

**System Tools:**

| Tool | Purpose |
|------|---------|
| `list_doctypes` | List all doctypes with field counts |
| `validate_doctype_yaml` | Validate YAML without saving (line-numbered errors) |
| `create_doctype_draft` | Create new DocType as **Draft only** — never activates, human required |

**Multi-Round Execution:** The loop runs until `finish_reason == "stop"` with safety nets: max rounds (10 default), stall detection, error circuit breaker, context compaction at 80% token budget, tool result size caps (4KB).

**AI Audit Trail:** AI-created records set `modified_by = "ai-assistant"` for audit queries.

**CSRF:** Required. Include `X-Kora-CSRF-Token` header.

---

## System Schema API

### Get Doctype Schema

Returns the full DocType definition, workflow, permissions, and inbound references. The frontend form and list engines derive their structure from this response.

```http
GET /api/system/doctype/Order
```

**Response:**
```json
{
  "data": {
    "doctype": {
      "name": "Order",
      "module": "Airtime",
      "title_field": "title",
      "fields": [
        {"fieldname": "title", "fieldtype": "Data", "label": "Order Title", "reqd": true, ...},
        {"fieldname": "customer", "fieldtype": "Link", "label": "Customer", "options": "Customer", ...},
        {"fieldname": "items", "fieldtype": "Table", "label": "Items", "options": "Order Item", ...},
        {"fieldname": "subtotal", "fieldtype": "Currency", "computed": "SUM(items.line_total)", "read_only": true, ...},
        {"fieldname": "unit_price", "fieldtype": "Currency", "linked_field": "product.selling_price", ...}
      ]
    },
    "workflow": {
      "states": [{"state": "Draft", "doc_status": 0, "allow_edit": "Sales Agent", "style": "default"}],
      "transitions": [{"action": "Confirm Order", "from": "Draft", "to": "Confirmed", ...}],
      "state_field": "status"
    },
    "permissions": {"read": true, "write": true, "create": true, "delete": false, ...},
    "transitions": [{"action": "Confirm Order", "from": "Draft", ...}],
    "referenced_by": [
      {"doctype": "Service Report", "fieldname": "order", "label": "Order"}
    ]
  }
}
```

**Query Parameters:**

| Param | Purpose |
|---|---|
| `?state=Draft` | Return available transitions from this state for current user |

**`referenced_by` field:** Lists all doctypes that have Link fields pointing to this doctype. Used by the frontend to show "Related Documents" panels. E.g., viewing a Customer shows related Orders because `Order.customer → Customer`.

**`computed` and `linked_field`:** Fields may include `computed` (expression string like `"quantity * unit_price"`) or `linked_field` (like `"product.selling_price"`). The frontend uses these to auto-populate and auto-calculate field values. See CONFIG.md for details.

### Get Navigation

Returns sidebar structure and current user info.

```http
GET /api/system/navigation
```

**Response:**
```json
{
  "data": {
    "modules": [
      {
        "module": "Airtime",
        "label": "Airtime",
        "doctypes": [
          {"name": "Customer", "label": "Customer", "is_child": false},
          {"name": "Order", "label": "Order", "is_child": false}
        ]
      }
    ],
    "branding": {"app_name": "Kora", "primary_color": "#2563eb"},
    "user": {"name": "admin", "full_name": "Administrator", "email": "admin@...", "roles": ["Administrator"]}
  }
}
```

### Get Auth Providers

Public endpoint — no auth required. Returns enabled authentication methods.

```http
GET /api/auth/providers
```

**Response:**
```json
{
  "data": {
    "providers": [{"name": "password", "label": "Email & Password"}]
  }
}
```

---

## User Management API

All endpoints require Administrator role. Responses use the standard envelope.

### List Users

```http
GET /api/system/users
```

Returns all users with their roles, status, and timestamps.

**Response:**
```json
{
  "data": [
    {
      "name": "USER-0001",
      "email": "admin@airtime.local",
      "full_name": "Administrator",
      "roles": ["Administrator"],
      "enabled": true,
      "created": "2026-06-10T12:00:00Z",
      "modified": "2026-06-15T08:30:00Z"
    }
  ]
}
```

### Get User

```http
GET /api/system/users/:name
```

### Create User

```http
POST /api/system/users
Content-Type: application/json

{
  "email": "agent@airtime.local",
  "password": "secret123",
  "full_name": "Sales Agent",
  "roles": ["Sales Agent"]
}
```

**Response:** `201 Created`

Password requirements: minimum 8 characters. Email must be unique per site. A ULID-based `name` is auto-generated.

### Update User

```http
PUT /api/system/users/USER-0002
Content-Type: application/json

{
  "full_name": "Senior Sales Agent",
  "roles": ["Sales Agent", "Manager"],
  "enabled": true
}
```

Optionally include `"password": "newpassword"` to change the password.

### Delete User

```http
DELETE /api/system/users/USER-0002
```

Prevents self-delete (cannot delete your own account). All sessions for the deleted user are terminated.

### Reset Password

```http
POST /api/system/users/USER-0002/reset-password
Content-Type: application/json

{
  "password": "newpassword123"
}
```

Sets a new password and invalidates all existing sessions for that user, forcing re-login. Minimum 8 characters. No email infrastructure needed — the admin communicates the new password to the user.

**Errors:** `403` — non-Administrator. `400` — password too short. `404` — user not found.

---

## Secrets Management API

Manage encrypted configuration secrets (API keys, credentials). Values are encrypted at rest with AES-256-GCM and **never returned by the API**. All endpoints require Administrator role.

### List Secrets

```http
GET /api/system/secrets
```

**Response:**
```json
{
  "data": [
    {"key_name": "openai_api_key", "updated_at": "2026-06-15T10:00:00Z"},
    {"key_name": "deepseek_api_key", "updated_at": "2026-06-14T09:00:00Z"}
  ]
}
```

Only key names and timestamps are returned. Values are never exposed.

### Set Secret

```http
POST /api/system/secrets
Content-Type: application/json

{
  "key": "openai_api_key",
  "value": "sk-proj-..."
}
```

Creates or updates a secret. The value is encrypted before storage.

### Delete Secret

```http
DELETE /api/system/secrets/:key
```

Removes the secret. Any functionality depending on it (e.g., AI Chat) will stop working.

**AI Provider Quick-Set:** The workspace UI at `/workspace/admin/secrets` provides a dropdown to select the provider (OpenAI, DeepSeek, Anthropic) and enter the key — no need to remember the exact key name.

---

## OpenAPI / Swagger

### OpenAPI Spec

```http
GET /api/openapi.json
```

Returns a full OpenAPI 3.0.3 spec auto-generated from the doctype registry — all CRUD endpoints, schemas, and auth.

### Swagger UI

```http
GET /api/swagger-ui
```

Interactive API documentation. Also linked from the workspace sidebar as "API Docs" (opens in new tab).

---

## Validation Errors

### Field-level errors (single)

```json
{
  "error": {
    "type": "ValidationError",
    "message": "Full Name is required.",
    "field": "full_name",
    "doctype": "Customer"
  }
}
```

### Unique constraint errors

When a `unique: true` field has a duplicate value:

```json
{
  "error": {
    "type": "UniqueConstraint",
    "message": "ID Number must be unique. Value \"33333390\" already exists in CUST-0001.",
    "field": "id_number",
    "doctype": "Customer"
  }
}
```

The frontend displays this as an inline error on the specific field, with a red border and error text.

### Multiple validation errors
```json
{
  "error": {
    "errors": [
      {"type": "ValidationError", "message": "...", "field": "title"},
      {"type": "UniqueConstraint", "message": "...", "field": "email"}
    ]
  }
}
```

---

## Admin API

These endpoints power the Administrator tab in the workspace. They manage doctypes, roles, permissions, workflows, and config versions. All require authentication.

### List All DocTypes

```http
GET /api/system/doctypes
```

Returns a flat array of all DocType objects (excluding child tables).

### Create DocType

```http
POST /api/system/doctype?activate=true|false
Content-Type: application/json

{
  "name": "Invoice",
  "module": "Billing",
  "title_field": "invoice_number",
  "is_submittable": true,
  "fields": [...]
}
```

Set `?activate=false` to save as Draft (version only, no migration). Default is `activate=true` which applies immediately.

### Update DocType

```http
PUT /api/system/doctype/:doctype?activate=true|false
```

Same body as create. Updates an existing doctype. Uses optimistic locking via a `version` column.

### Delete DocType

```http
DELETE /api/system/doctype/:doctype
```

Removes config from `_kora_doctype`/`_kora_field`. Does NOT drop the data table.

### Validate DocType

```http
POST /api/system/doctype/validate
```

Accepts JSON (`Content-Type: application/json`) or YAML (`Content-Type: application/x-yaml`). For JSON, returns validated DocType with defaults. For YAML, returns structured syntax errors with line numbers and "did you mean?" suggestions:

```json
{
  "valid": false,
  "syntax": [
    {"line": 4, "column": 1, "key": "is_submittible", "context": "doctype", "detail": "did you mean \"is_submittable\"?"},
    {"line": 9, "column": 5, "key": "icon", "context": "doctype"}
  ],
  "validations": null
}
```

Unknown keys inside `fields[]`, `constraints[]`, and `doc_constraints[]` are checked recursively.

### Dry-Run Impact Analysis

```http
POST /api/system/doctype/dry-run
```

Returns the DDL and safety tier classification for a proposed doctype change without applying it:

```json
{
  "data": {
    "ddl": ["ALTER TABLE tabInvoice ADD COLUMN discount DECIMAL(21,9) DEFAULT NULL"],
    "changes": [{"tier": "safe", "doctype": "Invoice", "rows": 1250, "message": "..."}],
    "blocked": [],
    "warnings": []
  }
}
```

### Get References (dependency check)

```http
GET /api/system/doctype/:doctype/references
```

Returns other doctypes that have Link fields pointing to this doctype.

### YAML Export

```http
GET /api/system/doctype/:doctype?format=yaml
```

Returns the DocType serialized as YAML (`text/yaml` content type).

### Roles CRUD

```http
GET    /api/system/roles              # List all roles
POST   /api/system/roles              # Create a role
PUT    /api/system/roles/:name        # Update a role
DELETE /api/system/roles/:name        # Delete a role (returns affected user count)
```

Role body: `{"name": "Sales Agent", "workspace_access": true, "description": "..."}`

### Permissions

```http
GET /api/system/permissions           # List all permissions
PUT /api/system/permissions           # Save full permission set (replaces all)
```

Permission body: `{"doctype": "Invoice", "role": "Sales Agent", "read": true, "write": true, ...}`

### Workflows CRUD

```http
GET    /api/system/workflows           # List all workflows
GET    /api/system/workflows/:doctype  # Get workflow for a specific doctype
POST   /api/system/workflows           # Create or update a workflow
DELETE /api/system/workflows/:doctype  # Remove a workflow
```

### Config Version Actions

```http
POST /api/system/config/versions/:id/activate   # Activate a Draft version
POST /api/system/config/versions/:id/discard    # Discard a Draft version
POST /api/system/config/versions/:id/rollback   # Rollback to a previous version
```

### YAML Import

```http
POST /api/system/config/import
Content-Type: multipart/form-data

file: doctype.yaml
```

Parses a YAML file and returns the structured DocType JSON. Does not save — the frontend loads it into the editor for review.

---

## System Config API

### List Config Versions

```http
GET /api/system/config/versions
```

### Get Config Version

```http
GET /api/system/config/versions/cv-fieldwork.local-1
```

### Diff Config Versions

```http
GET /api/system/config/diff?from=cv-fieldwork.local-1&to=cv-fieldwork.local-2
```

---

## Analytics API

All endpoints require Administrator role. The analytics system is opt-in — enable via `KORA_ANALYTICS=true` and `KORA_MYSQL_BUS_HOST`.

### Status

```http
GET /api/analytics/status
```

Returns whether analytics is enabled and the event processing stats.

**Response:**
```json
{
  "data": {
    "enabled": true,
    "events_processed": 15420,
    "events_dropped": 0
  }
}
```

### List Metrics

```http
GET /api/analytics/metrics
```

Returns all metrics for the site — auto-generated from DocType metadata plus user-defined custom metrics.

**Response:**
```json
{
  "data": [
    {"name": "total_customer", "label": "Total Customers", "type": "count", "doctype": "Customer", "auto_generated": true},
    {"name": "total_work_order", "label": "Total Work Orders", "type": "count", "doctype": "Work Order", "auto_generated": true},
    {"name": "work_order_by_status", "label": "Work Orders by Status", "type": "count", "doctype": "Work Order", "field": "status", "auto_generated": true}
  ]
}
```

### Get Metric

```http
GET /api/analytics/metrics/:name
```

### Query Metric

```http
POST /api/analytics/metrics/:name/query
Content-Type: application/json

{
  "from": "2026-06-01",
  "to": "2026-06-30",
  "group_by": "month"
}
```

Returns aggregated time-series data. `group_by` supports `day`, `week`, or `month`.

**Response:**
```json
{
  "data": {
    "metric": "total_customer",
    "columns": ["period", "value"],
    "rows": [{"period": "2026-06-01", "value": 42}, {"period": "2026-06-08", "value": 58}],
    "total": 100
  }
}
```

### Create Custom Metric

```http
POST /api/analytics/metrics
Content-Type: application/json

{
  "name": "high_priority_orders",
  "label": "High Priority Orders",
  "type": "count",
  "doctype": "Order",
  "field": "priority",
  "group_by_field": "customer"
}
```

### Insights

```http
GET /api/analytics/insights/:doctype
```

Returns time-series, group-by breakdowns, funnel data, and duration stats for a specific DocType. Used by the frontend Insights tab on the DocType detail page.

---

## Response Envelope

All success responses follow:

```json
{
  "data": { ... },
  "meta": {
    "config_version": 14,
    "doctype": "Customer",
    "total": 42
  }
}
```

All error responses follow:

```json
{
  "error": {
    "type": "ValidationError",
    "message": "Estimated hours must be at least 0.5.",
    "field": "estimated_hours",
    "doctype": "Work Order"
  }
}
```

Multiple validation errors:

```json
{
  "error": {
    "errors": [
      {"type": "ValidationError", "message": "...", "field": "title"},
      {"type": "ValidationError", "message": "...", "field": "customer"}
    ]
  }
}
```

---

## HTTP Status Codes

| Code | Meaning |
|---|---|
| 200 | OK (GET, PUT, DELETE) |
| 201 | Created (POST) |
| 204 | No Content (OPTIONS preflight) |
| 400 | Bad Request (invalid JSON, validation errors, workflow errors) |
| 401 | Unauthorized (missing or expired session) |
| 403 | Forbidden (permission denied, CSRF mismatch) |
| 404 | Not Found (missing DocType or document) |
| 429 | Too Many Requests (rate limit exceeded) |
| 500 | Internal Server Error (DB errors) |

---

## Security Headers (Every Response)

```
Content-Security-Policy: default-src 'self'; script-src ...; style-src ...
Referrer-Policy: same-origin
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-Request-Id: <ulid>
X-Xss-Protection: 1; mode=block
Strict-Transport-Security: max-age=31536000 (if TLS enabled)
```

## CORS Headers

```
Access-Control-Allow-Credentials: true
Access-Control-Allow-Origin: <configured-origin>
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Origin, Content-Type, Accept, Authorization, X-Kora-CSRF-Token, X-Request-Id
```

## Rate Limiting

Default: 100 requests/second per user. Returns `429 Too Many Requests` when exceeded. Configurable via `common_site_config.yaml`.

# Kora — Configuration Reference

## File Layout

```
config/myapp/
├── roles.yaml           # User role definitions
├── permissions.yaml     # RBAC rules
├── scheduler.yaml       # Background jobs
└── doctypes/
    ├── customer.yaml    # DocType definition
    ├── order.yaml
    ├── order_workflow.yaml  # Workflow definition
    └── ...
```

All YAML files are imported once via `kora config import` and then live in the database.

---

## DocType

### Top-Level Properties

| Property | Type | Required | Default | Description |
|---|---|---|---|---|
| `name` | string | **yes** | — | Unique name. Becomes the REST resource and table name |
| `module` | string | **yes** | — | Logical grouping for navigation |
| `is_submittable` | bool | no | false | Enables Submit/Cancel lifecycle |
| `is_child_table` | bool | no | false | Only exists inside a parent DocType (Table field) |
| `is_single` | bool | no | false | Only one record exists (settings page) |
| `track_changes` | bool | no | false | Log every save to change history |
| `title_field` | string | no | `name` | Field used as document title |
| `search_fields` | string | no | — | Comma-separated fields for global search |
| `sort_field` | string | no | `modified` | Default sort column |
| `sort_order` | string | no | `DESC` | `ASC` or `DESC` |
| `description` | string | no | — | Human-readable description |

### Field Properties

| Property | Type | Required | Description |
|---|---|---|---|
| `fieldname` | string | **yes** | Internal name (database column). Must be `snake_case` |
| `fieldtype` | string | **yes** | One of the supported types (see below) |
| `label` | string | no | UI display label. Defaults to title-cased fieldname |
| `options` | string | varies | Meaning depends on fieldtype |
| `reqd` | bool | no | Must have a value on save |
| `unique` | bool | no | Database unique constraint |
| `default` | string | no | Default value for new documents |
| `hidden` | bool | no | Not shown in UI |
| `read_only` | bool | no | Shown but not editable |
| `bold` | bool | no | Rendered bold in forms |
| `in_list_view` | bool | no | Column in list view |
| `in_standard_filter` | bool | no | Quick filter in list view |
| `search_index` | bool | no | Database index on this column |
| `description` | string | no | Help text below field |
| `depends_on` | string | no | Expression to show/hide the field |
| `linked_field` | string | no | `"{link_field}.{source}"` — auto-populate from a linked document |
| `computed` | string | no | Expression to auto-compute this field's value |
| `constraints` | array | no | Validation rules |

### Field Types

| Fieldtype | SQL Type | UI Widget | `options` |
|---|---|---|---|
| `Data` | VARCHAR(140) | Text input | Format hint: `Email`, `Phone`, `URL` |
| `Text` | TEXT | Textarea | — |
| `Text Editor` | LONGTEXT | Rich text | — |
| `Int` | BIGINT | Number | — |
| `Float` | DECIMAL(21,9) | Number | — |
| `Currency` | DECIMAL(21,9) | Currency | — |
| `Percent` | DECIMAL(21,9) | Percent | — |
| `Check` | TINYINT(1) | Checkbox | — |
| `Date` | DATE | Date picker | — |
| `Time` | TIME(6) | Time picker | — |
| `Datetime` | DATETIME(6) | Datetime picker | — |
| `Select` | VARCHAR(140) | Dropdown | Newline-separated options |
| `Link` | VARCHAR(140) | Autocomplete | Target DocType name |
| `Dynamic Link` | VARCHAR(140) | Dynamic | Fieldname holding target DocType |
| `Table` | *(child table)* | Inline grid | Child DocType name |
| `Attach` | TEXT | File upload | — |
| `Attach Image` | TEXT | Image upload | — |
| `JSON` | JSON | JSON editor | — |
| `Password` | VARCHAR(255) | Password | Never returned by API |
| `Section Break` | *(none)* | Section divider | — |
| `Column Break` | *(none)* | Column divider | — |
| `Heading` | *(none)* | Bold heading | — |

### Constraints

#### Scalar

| Type | Applies To | Properties |
|---|---|---|
| `min` | Int, Float, Currency | `value` |
| `max` | Int, Float, Currency | `value` |
| `min_length` | Data, Text | `value` |
| `max_length` | Data, Text | `value` |
| `min_date` | Date, Datetime | `value` (`today`, `today+N`, ISO date) |
| `max_date` | Date, Datetime | `value` |
| `min_rows` | Table | `value` |
| `max_rows` | Table | `value` |
| `regex` | Data | `pattern` |
| `one_of` | Data, Select | `values` (list) |
| `not_one_of` | Data, Select | `values` (list) |

#### Conditional

| Type | Description |
|---|---|
| `required_if` | Field mandatory when `condition` is true |
| `readonly_if` | Field read-only when `condition` is true |
| `hidden_if` | Field hidden when `condition` is true |

Condition example: `"doc.status == 'Approved'"`

### Document-Level Constraints

| Type | Description |
|---|---|
| `field_dependency` | If `condition` is true, listed fields are required |
| `cross_field` | Relationship between two fields (`lhs >= rhs`) |
| `unique_together` | Combination of fields must be unique |
| `immutable_after` | Fields become read-only at given statuses |

---

## Roles

```yaml
# roles.yaml
- name: Field Technician
  workspace_access: true
  description: Creates and manages their own work orders.

- name: Service Manager
  workspace_access: true
  description: Approves work orders and manages all field operations.

- name: Administrator
  workspace_access: true
  description: Full system access.
```

---

## Permissions

```yaml
# permissions.yaml
- doctype: Work Order
  role: Field Technician
  read: true
  write: true
  create: true
  delete: false
  submit: false
  if_owner: true      # Only documents owned by this user
```

| Property | Description |
|---|---|
| `read` | Can view documents |
| `write` | Can update documents |
| `create` | Can create documents |
| `delete` | Can delete documents |
| `submit` | Can execute workflow transitions |
| `cancel` | Can cancel submitted documents |
| `amend` | Can create amended copies |
| `export` | Can export to CSV |
| `import` | Can import from CSV |
| `report` | Can run reports |
| `if_owner` | Above apply only to documents owned by the user |

---

## Workflows

```yaml
# work_order_workflow.yaml
name: Work Order Approval
document_type: Work Order
is_active: true
workflow_state_field: status

states:
  - state: Draft
    doc_status: 0
    allow_edit: Field Technician
    style: default

  - state: Submitted
    doc_status: 0
    allow_edit: Service Manager
    style: warning

transitions:
  - action: Submit for Approval
    from: Draft
    to: Submitted
    allowed: Field Technician
    condition: "len(doc.items) > 0"

  - action: Approve
    from: Submitted
    to: Approved
    allowed: Service Manager

notifications:
  - event: state_change
    to_state: Approved
    recipients:
      - field: assigned_technician
    subject: "Work Order {title} has been approved"
    message: "Your work order for {customer} has been approved."
```

---

## Scheduler

```yaml
# scheduler.yaml
jobs:
  - name: overdue_work_order_alert
    type: doctype_alert
    schedule: "0 9 * * *"     # Daily at 9 AM
    config:
      doctype: Work Order
      filters:
        - [status, in, [Approved, In Progress]]
        - [scheduled_date, <, today]
      notify_field: assigned_technician
      subject: "Overdue: Work Order {title}"
      message: "Work Order {title} for {customer} is past its scheduled date."

  - name: weekly_summary
    type: email_report
    schedule: "0 8 * * MON"   # Every Monday at 8 AM
    config:
      doctype: Work Order
      filters:
        - [status, =, Completed]
      recipients:
        - email: manager@fieldwork.local
      subject: "Weekly Work Order Summary"
      message: "Completed work orders this week:"
```

### Cron Expression Format

Standard 5-field cron: `minute hour day-of-month month day-of-week`

| Expression | Meaning |
|---|---|
| `0 9 * * *` | Every day at 9:00 AM |
| `0 8 * * MON` | Every Monday at 8:00 AM |
| `*/15 * * * *` | Every 15 minutes |
| `0 0 1 * *` | Midnight on the 1st of each month |

### Job Types

| Type | Description |
|---|---|
| `doctype_alert` | Query a DocType; notify user field if matches found |
| `email_report` | Query a DocType; send results as email to recipients |

---

## How Features Work (Config-Driven)

### Link Fields & Searchable Dropdowns

A `Link` field stores a reference to another document. The `options` property names the target DocType.

```yaml
- fieldname: customer
  fieldtype: Link
  label: Customer
  options: Customer        # ← target DocType
  reqd: true
```

**How it works in the UI:**
1. The frontend reads `options: Customer` → knows the target doctype
2. Reads the target's `title_field` from its schema (e.g., `full_name` for Customer)
3. Loads the first 50 records from `GET /api/resource/Customer?limit=50`
4. Shows a searchable dropdown — user can type to filter, or trigger server search for large datasets
5. On select, stores the linked document's `name` (e.g., `CUST-0001`)

**Back-referencing:** The backend automatically detects which doctypes link to a given doctype. When you view a Customer, the `referenced_by` array in the schema response tells the frontend: "Order links to Customer via the `customer` field." The frontend renders a "Related Orders" panel using `GET /api/resource/Order?filters=[["customer","=","CUST-0001"]]`.

No config needed for back-references — it's derived at runtime by scanning all doctypes' Link fields.

### Unique Validation

Set `unique: true` on any field to enforce uniqueness:

```yaml
- fieldname: id_number
  fieldtype: Data
  label: National ID
  unique: true            # ← creates UNIQUE index + pre-save check
```

**How it works:**
1. Schema migrator creates a `UNIQUE KEY uq_id_number (id_number)` on the database table
2. Before Insert/Update, the ORM runs `SELECT name FROM tabX WHERE id_number = ?` — if a duplicate exists, returns a field-level `UniqueConstraint` error
3. The frontend shows an inline error on the specific field: *"National ID must be unique. Value "33333390" already exists in CUST-0001."*
4. The database unique index is the final guard against race conditions

### Child Tables

A `Table` field embeds a child DocType within the parent form:

```yaml
# Parent (Order)
- fieldname: items
  fieldtype: Table
  label: Items
  options: Order Item     # ← child DocType
  constraints:
    - type: min_rows
      value: 1
      message: "At least one item is required."

# Child (Order Item)
- fieldname: product
  fieldtype: Link
  options: Product        # ← links to Product
  reqd: true
```

**How it works:**
1. Schema migrator creates a junction table `tabOrder__items` with `parent`, `parentfield`, `parenttype` columns
2. Frontend renders an inline editable grid — each row is a mini-form with the child's fields
3. Child rows are sent as an array within the parent document on save
4. On load, child rows are expanded into the parent document's `items` array

### Computed Fields

Use `computed` to auto-calculate a field's value from other fields. The expression engine evaluates the expression whenever a referenced field changes.

```yaml
# Simple arithmetic
- fieldname: line_total
  fieldtype: Currency
  computed: "quantity * unit_price"
  read_only: true

# Sum across a child table
- fieldname: subtotal
  fieldtype: Currency
  computed: "SUM(items.line_total)"
  read_only: true

# Arithmetic with rounding
- fieldname: total
  fieldtype: Currency
  computed: "ROUND(subtotal - discount, 2)"
  read_only: true
```

**Supported functions:**

| Expression | Description | Example |
|---|---|---|
| `fieldname` | Current value of a field on the same document | `quantity` |
| `a + b`, `a - b`, `a * b`, `a / b` | Arithmetic | `quantity * unit_price` |
| `SUM(table.field)` | Sum of a field across all rows in a child table | `SUM(items.line_total)` |
| `COUNT(table)` | Number of rows in a child table | `COUNT(attendees)` |
| `ROUND(expr, N)` | Round to N decimal places | `ROUND(subtotal * 1.1, 2)` |
| `DATEDIFF(a, b)` | Days between two dates (fields, `today()`, or literals) | `DATEDIFF(today(), end_date)` |
| `today()` | Current date at midnight | `DATEDIFF(due_date, today())` |

Computed fields are evaluated **server-side** on Insert and Save, then persisted to the database via UPDATE. The frontend also evaluates them client-side for instant feedback. Values cascade: child `line_total` → parent `subtotal` → `total`.

**No code is written per doctype.** The expression engine reads `computed` from the field config and evaluates it generically for any doctype.

### Linked Field Auto-Population

Use `linked_field` to auto-fill a field from a linked document when a Link field changes:

```yaml
- fieldname: product
  fieldtype: Link
  options: Product          # ← when this changes...

- fieldname: unit_price
  fieldtype: Currency
  linked_field: "product.selling_price"  # ← fetch from Product.selling_price
```

**How it works:**
1. User selects a Product in the Link dropdown
2. Frontend fetches `GET /api/resource/Product/{name}`
3. Reads `selling_price` from the response
4. Sets `unit_price` to that value
5. `computed` expressions depending on `unit_price` (like `line_total`) cascade automatically

The user can override the auto-populated value. The `linked_field` property is generic — works for any Link field on any doctype.

### File Upload

Upload files via `POST /api/upload` with `multipart/form-data`. The endpoint returns a file path that can be stored in an `Attach` or `Attach Image` field.

```bash
curl -F "file=@resume.pdf" http://localhost:8000/api/upload
# → {"data": {"path": "sites/crm/files/2026/06/resume.pdf", "filename": "resume.pdf"}}
```

Files are stored under `sites/<site>/files/<YYYY>/<MM>/`. Duplicate filenames get a numeric suffix to avoid collisions. The site is determined from the request context (Host header or `kora_site` cookie).

**Attach field types:**
- `Attach` — stores a file path. Renders as a text input + upload button in the UI
- `Attach Image` — stores an image path. Renders with image preview

```yaml
- fieldname: resume
  fieldtype: Attach
  label: Resume
```

## Site Config

```yaml
# sites/fieldwork.local/site_config.yaml
db_host: 127.0.0.1
db_port: 3306
db_name: fieldwork_local
db_user: kora
db_password: secret

redis_url: redis://localhost:6379/0

file_storage: local
files_path: sites/fieldwork.local/files

hostname: fieldwork.local
domains:
  - fieldwork.local
  - www.fieldwork.local

apps:
  - core
```

## Common Site Config

```yaml
# common_site_config.yaml
redis_url: redis://localhost:6379/0
db_host: 127.0.0.1
http_port: 8000
workers: 4
log_level: info       # debug | info | warn | error
log_format: json      # json | text
rate_limit: 100       # requests/sec per user
rate_limit_burst: 20
tls_mode: off         # off | auto | manual
tls_email: ""         # For Let's Encrypt
```

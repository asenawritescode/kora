# Skill: Create Kora Config Files

You are generating YAML configuration files for Kora, a config-driven application engine. These files define the entire application: data model, permissions, roles, and workflows. The engine reads them and provides database schema, REST API, and a React admin UI automatically.

> **Alternative:** Config can also be created via the **Administrator tab** in the workspace UI — a visual form builder with live YAML preview. Go to `/workspace/admin/doctypes` after logging in. This guide covers the YAML format used by both the visual builder and manual file creation.

## Directory Structure

```
config/<app-name>/
  doctypes/
    <entity>.yaml         # One file per DocType
    <entity>_workflow.yaml # Optional workflow definitions
  roles.yaml              # User roles (flat YAML array)
  permissions.yaml         # Access control rules (flat YAML array)
```

## Doctype File (`doctypes/<name>.yaml`)

### Top-Level Properties

| Property | Required | Type | Description |
|---|---|---|---|
| `name` | **yes** | string | Unique name. Becomes the REST resource (e.g., `name: Todo` → `/api/resource/Todo`) |
| `module` | **yes** | string | Groups doctypes in the sidebar navigation |
| `title_field` | no | string | Field used as the document title (default: `name`) |
| `search_fields` | no | string | Comma-separated fields for search |
| `is_submittable` | no | bool | Enables Submit/Cancel lifecycle |
| `is_child_table` | no | bool | Set true only for doctypes used inside a Table field |
| `is_single` | no | bool | Only one record (e.g., settings page) |
| `sort_field` | no | string | Default sort column (default: `modified`) |
| `sort_order` | no | string | `ASC` or `DESC` (default: `DESC`) |

### Field Properties

| Property | Required | Type | Description |
|---|---|---|---|
| `fieldname` | **yes** | string | Internal name. **snake_case, lowercase, no spaces** |
| `fieldtype` | **yes** | string | One of the valid field types below |
| `label` | no | string | Display label (defaults to title-cased fieldname) |
| `options` | varies | string | Meaning depends on fieldtype (see below) |
| `reqd` | no | bool | Required field — shows `*` and validates non-empty |
| `unique` | no | bool | Creates DB unique index + pre-save uniqueness check |
| `default` | no | string | Default value for new documents |
| `read_only` | no | bool | Shown but not editable |
| `hidden` | no | bool | Not shown in UI |
| `bold` | no | bool | Renders label in bold |
| `in_list_view` | no | bool | Shows as a column in the list view |
| `in_standard_filter` | no | bool | Shows as a quick filter above the list |
| `search_index` | no | bool | Creates a database index on this column |
| `description` | no | string | Help text shown below the field |
| `depends_on` | no | string | Expression to show/hide the field |
| `computed` | no | string | Expression that auto-calculates this field's value |
| `linked_field` | no | string | `"link_field.source_field"` — auto-populates from linked doc |
| `renamed_from` | no | string | Old column name for non-breaking rename during migration |
| `constraints` | no | array | Validation rules (see constraints section) |

### Valid Field Types

| Fieldtype | SQL Column | UI Widget | `options` field |
|---|---|---|---|
| `Data` | VARCHAR(140) | Text input | Format hint: `Email`, `Phone`, `URL` (optional) |
| `Text` | TEXT | Textarea | — |
| `Int` | BIGINT | Number input (step=1) | — |
| `Float` | DECIMAL(21,9) | Number input | — |
| `Currency` | DECIMAL(21,9) | Number input | — |
| `Percent` | DECIMAL(21,9) | Number input | — |
| `Check` | TINYINT(1) | Toggle switch | — |
| `Date` | DATE | Date picker | — |
| `Time` | TIME(6) | Time picker | — |
| `Datetime` | DATETIME(6) | Datetime picker | — |
| `Select` | VARCHAR(140) | Dropdown | **Newline-separated options** |
| `Link` | VARCHAR(140) | Searchable autocomplete | **Target DocType name** |
| `Dynamic Link` | VARCHAR(140) | Dynamic autocomplete | Fieldname holding the target doctype |
| `Table` | *(child table)* | Inline editable grid | **Child DocType name** |
| `JSON` | JSON | JSON textarea | — |
| `Password` | VARCHAR(255) | Password input | — |
| `Section Break` | *(none)* | Section divider | Section title (label) |
| `Column Break` | *(none)* | Column divider | — |
| `Heading` | *(none)* | Bold heading | Heading text (label) |

### Constraints

```yaml
constraints:
  - type: min              # Minimum numeric value
    value: 5
    message: "Must be at least 5."

  - type: max              # Maximum numeric value
    value: 100
    message: "Cannot exceed 100."

  - type: min_length       # Minimum string length
    value: 3

  - type: max_length       # Maximum string length
    value: 140

  - type: regex            # Pattern match (Data, Text fields)
    pattern: "^[0-9+\\-\\s]{7,20}$"
    message: "Enter a valid phone number."

  - type: exists           # Link field must reference a real target
    message: "Customer does not exist."

  - type: min_rows         # Minimum child table rows
    value: 1
    message: "At least one item is required."

  - type: max_rows         # Maximum child table rows
    value: 50
```

### Computed Fields

```yaml
# Simple arithmetic
- fieldname: line_total
  fieldtype: Currency
  computed: "quantity * unit_price"
  read_only: true

# Sum across child table
- fieldname: subtotal
  fieldtype: Currency
  computed: "SUM(items.line_total)"
  read_only: true

# Rounding
- fieldname: total
  fieldtype: Currency
  computed: "ROUND(subtotal - discount, 2)"
  read_only: true
```

### Linked Field Auto-Population

```yaml
- fieldname: product
  fieldtype: Link
  options: Product
  reqd: true

- fieldname: unit_price
  fieldtype: Currency
  linked_field: "product.selling_price"    # Fetches selling_price from selected Product
```

## Layout Fields

`Section Break`, `Column Break`, and `Heading` control form layout. They don't create database columns.

```yaml
# Full-width section
- fieldname: section_details
  fieldtype: Section Break
  label: Customer Details

# Two-column layout: left column
- fieldname: col_left
  fieldtype: Column Break

- fieldname: first_name
  fieldtype: Data

- fieldname: email
  fieldtype: Data

# Two-column layout: right column starts here
- fieldname: col_right
  fieldtype: Column Break

- fieldname: phone
  fieldtype: Data

- fieldname: address
  fieldtype: Text
```

Fields between `col_left` and `col_right` stack on the left. Fields after `col_right` stack on the right. A new `Section Break` resets to full width.

## Roles (`roles.yaml`)

Flat YAML array — NOT a map with a `roles:` key:

```yaml
- name: Sales Agent
  workspace_access: true          # Can access the workspace UI
  description: Creates and manages customer orders.

- name: Administrator
  workspace_access: true
  description: Full system access.
```

The role named `Administrator` (or whatever `admin_role` is set to in `common_site_config.yaml`) bypasses all permission checks. Every app should have at least one role with this name.

## Permissions (`permissions.yaml`)

Flat YAML array — NOT a map with a `permissions:` key:

```yaml
- doctype: Customer
  role: Sales Agent
  can_read: true
  can_write: true
  can_create: true
  can_delete: false           # Sales agents can't delete customers
  can_submit: false
  can_cancel: false
  if_owner: false             # If true, only applies to documents they own

- doctype: Customer
  role: Administrator
  can_read: true
  can_write: true
  can_create: true
  can_delete: true
```

Operations: `can_read`, `can_write`, `can_create`, `can_delete`, `can_submit`, `can_cancel`, `can_amend`, `can_export`, `can_import`, `can_report`.

## Workflow (`doctypes/<name>_workflow.yaml`)

Defines a document lifecycle with states and transitions:

```yaml
name: Order Workflow
document_type: Order          # Which doctype this applies to
is_active: true
workflow_state_field: status  # Field that stores the current state

states:
  - state: Draft
    doc_status: 0             # 0 = Saved, 1 = Submitted, 2 = Cancelled
    allow_edit: Sales Agent   # Role allowed to edit in this state
    style: default            # Badge color: default | warning | success | danger | info

  - state: Confirmed
    doc_status: 0
    allow_edit: Sales Agent
    style: warning

  - state: Completed
    doc_status: 1
    allow_edit: ""            # No one can edit after submission
    style: success

transitions:
  - action: Confirm Order
    from: Draft
    to: Confirmed
    allowed: Sales Agent,Administrator    # Roles allowed to perform this
    condition: "len(doc.items) > 0"       # Expression that must be true
    require_fields:                       # Fields that must be non-empty
      - payment_method

notifications:                           # Optional — email on state change
  - event: state_change
    to_state: Shipped
    recipients:
      - field: customer.email
    subject: "Your order {name} has shipped"
    message: "Order {name} is on its way."
```

## Complete Example: Todo App

### `config/todo/doctypes/todo.yaml`

```yaml
name: Todo
module: Tasks
title_field: title
search_fields: title, description
sort_field: modified
sort_order: DESC

fields:
  - fieldname: title
    fieldtype: Data
    label: Task
    reqd: true
    bold: true
    in_list_view: true
    search_index: true

  - fieldname: description
    fieldtype: Text
    label: Description

  - fieldname: status
    fieldtype: Select
    label: Status
    options: |
      Pending
      In Progress
      Done
    default: Pending
    in_list_view: true
    in_standard_filter: true

  - fieldname: priority
    fieldtype: Select
    label: Priority
    options: |
      Low
      Medium
      High
    default: Medium
    in_list_view: true
    in_standard_filter: true

  - fieldname: due_date
    fieldtype: Date
    label: Due Date
    in_list_view: true
```

### `config/todo/roles.yaml`

```yaml
- name: Todo User
  workspace_access: true
  description: Can create and manage todos.

- name: Administrator
  workspace_access: true
  description: Full system access.
```

### `config/todo/permissions.yaml`

```yaml
- doctype: Todo
  role: Todo User
  can_read: true
  can_write: true
  can_create: true
  can_delete: true

- doctype: Todo
  role: Administrator
  can_read: true
  can_write: true
  can_create: true
  can_delete: true
```

## Validation Checklist

Before outputting a config, verify:
- [ ] `fieldname` values are snake_case, lowercase, no spaces
- [ ] `fieldtype` is one of the valid types listed above
- [ ] `options` for Link fields is a valid DocType name from the same config
- [ ] `options` for Select fields is newline-separated (use `|` literal block scalar)
- [ ] `options` for Table fields is a valid child DocType name
- [ ] `computed` expressions reference fields that exist on the same doctype
- [ ] `linked_field` format is `"link_fieldname.source_fieldname"` — both must exist
- [ ] At least one role named `Administrator` exists
- [ ] Permissions cover every doctype for at least the Administrator role
- [ ] `roles.yaml` and `permissions.yaml` are flat arrays, NOT maps with keys
- [ ] Workflow `document_type` matches a doctype name exactly
- [ ] Workflow `from` and `to` states exist in the `states` list

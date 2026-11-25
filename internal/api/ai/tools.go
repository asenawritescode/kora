package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

type ToolDescriptor struct {
	ID                      string            `json:"id"`
	Source                  string            `json:"source"`
	Name                    string            `json:"name"`
	Description             string            `json:"description"`
	InputSchema             map[string]any    `json:"input_schema"`
	SafetyLevel             string            `json:"safety_level"`
	RequiresConfirmation    bool              `json:"requires_confirmation"`
	RequiresRecentAuth      bool              `json:"requires_recent_auth"`
	ChannelAllowlist        []string          `json:"channel_allowlist"`
	ArgumentContractVersion string            `json:"argument_contract_version"`
	Operation               string            `json:"operation"`
	Doctype                 string            `json:"doctype,omitempty"`
	DoctypeLabel            string            `json:"doctype_label,omitempty"`
	TitleField              string            `json:"title_field,omitempty"`
	SearchFields            []string          `json:"search_fields,omitempty"`
	SortField               string            `json:"sort_field,omitempty"`
	SortOrder               string            `json:"sort_order,omitempty"`
	FieldHints              []FieldHint       `json:"field_hints,omitempty"`
	SystemFields            []SystemFieldHint `json:"system_fields,omitempty"`
}

type FieldHint struct {
	Name           string               `json:"name"`
	Label          string               `json:"label,omitempty"`
	Fieldtype      string               `json:"fieldtype"`
	Type           string               `json:"type,omitempty"`
	Format         string               `json:"format,omitempty"`
	Options        []string             `json:"options,omitempty"`
	LinkTarget     string               `json:"link_target,omitempty"`
	TableTarget    string               `json:"table_target,omitempty"`
	Required       bool                 `json:"required,omitempty"`
	ReadOnly       bool                 `json:"read_only,omitempty"`
	Computed       bool                 `json:"computed,omitempty"`
	Writable       bool                 `json:"writable"`
	StandardFilter bool                 `json:"standard_filter,omitempty"`
	SearchIndex    bool                 `json:"search_index,omitempty"`
	InListView     bool                 `json:"in_list_view,omitempty"`
	Unique         bool                 `json:"unique,omitempty"`
	Description    string               `json:"description,omitempty"`
	Constraints    []doctype.Constraint `json:"constraints,omitempty"`
}

type SystemFieldHint struct {
	Name      string `json:"name"`
	Fieldtype string `json:"fieldtype"`
	Writable  bool   `json:"writable"`
}

type ToolCatalog struct {
	Version string           `json:"version"`
	Tools   []ToolDescriptor `json:"tools"`
}

// ---------------------------------------------------------------------------
// Tool function generation
// ---------------------------------------------------------------------------

// BuildToolCatalog projects the registry into the tool catalog: the fixed record tools
// (scoped by opts), then the system tools.
func BuildToolCatalog(reg *doctype.Registry, opts CatalogOptions) ToolCatalog {
	catalog := recordToolDescriptors(readableRecordTypes(reg, opts))
	if opts.Operations != nil {
		catalog = append(catalog, packActionDescriptors()...)
	}
	for _, raw := range buildSystemFunctions(reg) {
		if descriptor, ok := descriptorFromRawFunction(raw); ok {
			catalog = append(catalog, descriptor)
		}
	}
	return ToolCatalog{
		Version: toolCatalogVersion(catalog),
		Tools:   catalog,
	}
}

func buildOpenAIToolsFromCatalog(catalog ToolCatalog) []map[string]any {
	funcs := make([]map[string]any, 0, len(catalog.Tools))
	for _, tool := range catalog.Tools {
		funcs = append(funcs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			},
		})
	}
	return funcs
}

func descriptorFromRawFunction(raw map[string]any) (ToolDescriptor, bool) {
	fn, ok := raw["function"].(map[string]any)
	if !ok {
		return ToolDescriptor{}, false
	}
	name, _ := fn["name"].(string)
	description, _ := fn["description"].(string)
	params, _ := fn["parameters"].(map[string]any)
	descriptor := ToolDescriptor{
		ID:                      "tenant." + name,
		Source:                  "tenant",
		Name:                    name,
		Description:             description,
		InputSchema:             params,
		SafetyLevel:             classifyToolSafety(name),
		ChannelAllowlist:        []string{"web", "whatsapp"},
		ArgumentContractVersion: "v2",
		Operation:               "system",
	}
	if name == "mutate_records" {
		// Multi-record writes can create linked business records and advance a
		// configured workflow, so this operation always requires explicit approval.
		descriptor.RequiresConfirmation = true
		// Its durable approval is bound to an AI run and is not implemented for
		// the channel tool endpoint's header-only confirmation handshake.
		descriptor.ChannelAllowlist = []string{"web"}
	}
	if name == "script_create" {
		// AI may write an inactive script draft after an explicit user request.
		// Activation remains an Administrator action in the script editor.
		descriptor.RequiresRecentAuth = true
	}
	if name == "update_doctype_draft" || name == "create_doctype_draft" {
		descriptor.RequiresConfirmation = true
		descriptor.RequiresRecentAuth = true
	}
	return descriptor, true
}

func toolCatalogVersion(catalog []ToolDescriptor) string {
	data, err := json.Marshal(catalog)
	if err != nil {
		return "catalog-error"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func aiFieldSchema(f *doctype.Field) map[string]any {
	schema := map[string]any{}
	switch f.Fieldtype {
	case "Int":
		schema["type"] = "integer"
	case "Float", "Currency", "Percent":
		schema["type"] = "number"
	case "Check":
		schema["type"] = "boolean"
	case "Date":
		schema["type"], schema["format"] = "string", "date"
	case "Time":
		schema["type"], schema["format"] = "string", "time"
	case "Datetime":
		schema["type"], schema["format"] = "string", "date-time"
	case "JSON":
		schema["type"] = "object"
	case "Table":
		schema["type"] = "array"
	default:
		schema["type"] = "string"
	}
	if f.Label != "" {
		schema["description"] = f.Label
	}
	if f.Fieldtype == "Select" {
		if options := splitOptions(f.Options); len(options) > 0 {
			schema["enum"] = options
		}
	}
	return schema
}

func splitOptions(value string) []string {
	var out []string
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ToolRequest is one tool call made by an authenticated adapter.
type ToolRequest struct {
	Name  string
	Args  map[string]any
	Owner string
	Site  string
	// Confirmed marks a call whose tool catalog and explicit confirmation the
	// adapter already enforced at its boundary. Kernel authorization still
	// runs. A confirmed call must carry the client's Idempotency-Key in
	// IdempotencyKey so retried mutations replay safely; IdempotencyKey is
	// ignored when Confirmed is false.
	Confirmed      bool
	IdempotencyKey string
	// Commands resolves the pack commands a module-owned write must use.
	Commands *kernel.CommandRegistry
	// Operations runs the pack-action tools. Nil refuses them.
	Operations ModuleOperations
	// Execution is the verified principal the resulting kernel commands are
	// bound to. A zero value falls back to the transaction manager's own.
	Execution policy.Execution
}

// ExecuteTool is the authenticated-adapter entry point. It keeps the existing
// tool catalog and confirmation policy while binding resulting kernel commands
// to the verified principal supplied by the authentication boundary.
func ExecuteTool(tx *orm.TxManager, reg *doctype.Registry, req ToolRequest) string {
	key := ""
	if req.Confirmed {
		key = channelToolMutationIdempotencyKey(req.Site, req.IdempotencyKey)
	}
	return executeSingleTool(tx, reg, toolInvocation{Name: req.Name, Args: req.Args, Owner: req.Owner, Site: req.Site, IdempotencyKey: key, Execution: req.Execution, Commands: req.Commands, Operations: req.Operations})
}

// holdForApproval records a durable pending approval for target unless one is
// already granted. It reports the refusal to return to the model when the tool
// must not run yet.
func holdForApproval(tx *orm.TxManager, target ApprovalTarget, owner, stepID string) (string, bool) {
	if ok, _ := HasGrantedApproval(tx.Context, tx.DB, target); ok {
		return "", false
	}
	_ = EnsureApprovalPending(tx.Context, tx.DB, ApprovalRequest{Target: target, ActorID: owner, ActorType: "agent"})
	if stepID != "" && target.OperationID != "" {
		_ = MarkRunPendingApproval(tx.Context, tx.DB, target.OperationID, stepID, "pending_approval")
	}
	return "Approval required for " + target.ToolName + ". A durable approval has been recorded and the tool was not executed.", true
}

func classifyToolSafety(name string) string {
	switch {
	case name == toolFindRecords, name == toolGetRecord, name == toolDescribeRecord, name == toolListActions, name == "list_doctypes", name == toolListMetrics, name == toolQueryMetrics, name == toolDrillMetrics, name == toolAskChoice:
		return "safe"
	case name == toolCreateRecord, name == toolUpdateRecord:
		return "guarded"
	case strings.HasSuffix(name, "_delete"), name == "create_doctype_draft", name == "update_doctype_draft", name == "script_create":
		return "admin"
	default:
		return "guarded"
	}
}

func sanitizeName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// argsToJSON extracts a key from args and returns it as a JSON string.
func argsToJSON(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	// If already a string, return as-is.
	if s, ok := v.(string); ok {
		return s
	}
	// Marshal object to JSON.
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// System-level tools — doctype creation, validation, dry-run.
// These always create as Draft. Only a human can activate a draft.
// ---------------------------------------------------------------------------

func buildSystemFunctions(registries ...*doctype.Registry) []map[string]any {
	var registry *doctype.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	// YAML examples embedded in tool descriptions so the AI generates correct syntax.
	simpleExample := `name: Supplier
module: Buying
title_field: company_name
search_fields: company_name, email
sort_field: modified
sort_order: DESC
fields:
  - fieldname: company_name
    fieldtype: Data
    label: Company Name
    reqd: true
    in_list_view: true
  - fieldname: contact_person
    fieldtype: Data
    label: Contact Person
    in_list_view: true
  - fieldname: email
    fieldtype: Data
    label: Email
  - fieldname: phone
    fieldtype: Data
    label: Phone
  - fieldname: website
    fieldtype: Data
    label: Website
  - fieldname: address
    fieldtype: Text
    label: Address`

	complexExample := `name: Invoice
module: Accounting
title_field: customer
search_fields: customer, status
sort_field: modified
sort_order: DESC
is_submittable: true
track_changes: true
fields:
  - fieldname: customer
    fieldtype: Link
    label: Customer
    options: Customer
    reqd: true
    in_list_view: true
  - fieldname: invoice_date
    fieldtype: Date
    label: Invoice Date
    reqd: true
    in_list_view: true
  - fieldname: due_date
    fieldtype: Date
    label: Due Date
  - fieldname: status
    fieldtype: Select
    label: Status
    options: |
      Draft
      Sent
      Paid
      Overdue
      Cancelled
    default: Draft
    in_list_view: true
  - fieldname: section_items
    fieldtype: Section Break
    label: Items
  - fieldname: items
    fieldtype: Table
    label: Items
    options: Invoice Item
  - fieldname: section_totals
    fieldtype: Section Break
    label: Totals
  - fieldname: subtotal
    fieldtype: Currency
    label: Subtotal
    computed: (sum "items" "line_total")
    read_only: true
  - fieldname: tax_rate
    fieldtype: Percent
    label: Tax Rate
    default: "16"
  - fieldname: tax_amount
    fieldtype: Currency
    label: Tax Amount
    computed: (/ (* subtotal tax_rate) 100)
    read_only: true
  - fieldname: grand_total
    fieldtype: Currency
    label: Grand Total
    computed: (+ subtotal tax_amount)
    read_only: true`

	childTableExample := `name: Invoice Item
module: Accounting
is_child_table: true
title_field: product
sort_field: idx
sort_order: ASC
fields:
  - fieldname: product
    fieldtype: Link
    label: Product
    options: Product
    reqd: true
    in_list_view: true
  - fieldname: quantity
    fieldtype: Int
    label: Quantity
    reqd: true
    default: "1"
  - fieldname: unit_price
    fieldtype: Currency
    label: Unit Price
    reqd: true
  - fieldname: line_total
    fieldtype: Currency
    label: Line Total
    computed: (* quantity unit_price)
    read_only: true`

	return append([]map[string]any{
		mutateRecordsToolDef(registry),
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_doctypes",
				"description": "List all DocTypes in this site. Use this BEFORE creating a new doctype to see what already exists and what Link targets are available.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "validate_doctype_yaml",
				"description": "Validate a DocType YAML definition WITHOUT saving. Always call this first before create_doctype_draft. Returns syntax errors with line numbers and 'did you mean?' suggestions for unknown keys.\n\nFIELD TYPES: Data, Text, Text Editor, Int, Float, Currency, Percent, Check, Date, Time, Datetime, Select (with options), Link (set options to target doctype name), Dynamic Link, Table (set options to child doctype name), Attach, Attach Image, Attach Audio, Password, JSON, Section Break, Column Break, Heading.\n\nFIELD PROPERTIES: reqd (required), unique (must be unique across all records), in_list_view (show in table), in_standard_filter (show in filter sidebar), search_index (full-text searchable), read_only (non-editable), bold (highlight in forms), default (default value), accept (accepted file formats for Attach fields, e.g. '.pdf,.docx' or 'image/*'), dependency_scope (self/children/cross_doctype). NEVER use reserved field names: name, owner, creation, modified, modified_by, doc_status, idx, parent, parentfield, parenttype.\n\nTEMPORAL DEFAULTS: Do not generate default: Today or default: Now in AI-created YAML. Leave Date, Time, and Datetime defaults empty unless the user explicitly asks for a literal value like 2026-07-02.\n\nLINKED FIELDS: Use linked_field: \"target.fieldname\" on a Link field to auto-populate data from the linked document (e.g., linked_field: \"product.selling_price\" auto-fills the price when a Product is selected).\n\nDEPENDS_ON: Use depends_on: \"fieldname\" to show/hide a field based on another field. Use mandatory_depends_on: \"fieldname\" to make the dependency required.\n\nCONSTRAINTS: Per-field as [{type, value, message}]. DOC CONSTRAINTS (doc_constraints): [{type, predicate, condition, message}]. Predicate: (> end_date start_date). Condition: doc.type == \"wholesale\". Types:\n- min: maximum numeric value\n- max: maximum numeric value\n- min_length: minimum string length\n- max_length: maximum string length\n- regex: pattern to match\n- one_of: array of allowed values\n- not_one_of: array of disallowed values\nCOMPUTED FIELDS: S-expression (preferred): (* qty price), (sum \"items\" \"amount\"), (round expr N). Legacy syntax also works. Set read_only: true.\n\nTABLE (CHILD TABLE): Create the child doctype FIRST (with is_child_table: true), then the parent. The child doctype name goes in the Table field's 'options'.\n\nSIMPLE EXAMPLE:\n" + simpleExample + "\n\nCOMPLEX EXAMPLE (with Table, computed, Link, Select):\n" + complexExample + "\n\nCHILD TABLE EXAMPLE:\n" + childTableExample,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"yaml": map[string]any{"type": "string", "description": "YAML content to validate"},
					},
					"required": []string{"yaml"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "update_doctype_draft",
				"description": "Update an EXISTING DocType as DRAFT. Provide the FULL YAML for the doctype (include all existing fields plus your changes). The existing doctype is replaced with this definition. Always call validate_doctype_yaml first. Only call AFTER user confirms they want to update.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"yaml": map[string]any{"type": "string", "description": "Complete updated doctype YAML"},
					},
					"required": []string{"yaml"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "create_doctype_draft",
				"description": "Create a NEW DocType as DRAFT only. Does NOT create database tables — a human must review and activate. If the doctype has a Table field, create the child doctype FIRST (as a separate call), then the parent. Always call validate_doctype_yaml before this. Only call this AFTER the user confirms they want to create.\n\nFIELD TYPES: Data, Text, Text Editor, Int, Float, Currency, Percent, Check, Date, Time, Datetime, Select (with options using | prefix for multi-line), Link (options = target doctype name), Dynamic Link, Table (options = child doctype name), Attach, Attach Image, Attach Audio, Password, JSON, Section Break, Column Break, Heading.\n\nFIELD PROPERTIES: reqd, unique, in_list_view, in_standard_filter, search_index, read_only, bold, default, linked_field, depends_on, mandatory_depends_on, accept, dependency_scope (self/children/cross_doctype). NEVER use reserved field names: name, owner, creation, modified, modified_by, doc_status, idx, parent, parentfield, parenttype.\n\nTEMPORAL DEFAULTS: Never generate default: Today or default: Now in AI-created YAML. For Date, Time, and Datetime fields, omit the default unless the user explicitly asks for a literal value.\n\nCONSTRAINTS: [{type, value, message}]. Types: min, max, min_length, max_length, regex, one_of, not_one_of. DOC CONSTRAINTS (doc_constraints): [{type, predicate, condition, message}]. Predicate: (> end_date start_date). Condition: doc.type == \"wholesale\".\n\nCOMPUTED: S-expression (preferred): (* qty price), (sum \"items\" \"amount\"), (round expr N). Legacy also works. Set read_only: true.\n\nFor child tables: set is_child_table: true. Create child FIRST, then parent. Do NOT include table columns (parent, parentfield, parenttype, idx) — the system adds them automatically.\n\nSIMPLE EXAMPLE:\n" + simpleExample + "\n\nCOMPLEX EXAMPLE (with Table, Link, Select, computed fields, submittable):\n" + complexExample + "\n\nCHILD TABLE EXAMPLE:\n" + childTableExample,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"yaml": map[string]any{"type": "string", "description": "Complete doctype YAML. Use the examples above as templates."},
					},
					"required": []string{"yaml"},
				},
			},
		},
		// Script management tools.
		{
			"type": "function",
			"function": map[string]any{
				"name":        "script_create",
				"description": "Create an INACTIVE JavaScript script draft after the user explicitly requests it. A workspace Administrator must review and activate it in Workspace → Admin → Scripts before it runs. Never claim the draft is live. Scripts must use authorized Kora APIs and may not bypass domain workflows or approvals.\n\nSCRIPT TYPES:\n- doc_event: Runs on document lifecycle. Requires doctype + event.\n- api_method: Custom API at /api/method/{method_path}.\n- scheduled: Runs on a cron schedule.\n- computed: Computes field values from other fields.\n- validate: Custom validation logic.\n\nEVENTS (for doc_event): before_insert, after_insert, before_save, after_save, before_delete, after_delete, before_submit, after_submit, before_cancel, after_cancel, validate\n\nAVAILABLE JS API (kora global): kora.log.info/warn/error(msg...), kora.getDoc(doctype,name), kora.getList(doctype,filters,orderBy,limit,offset), kora.saveDoc(doctype,doc), kora.createDoc(doctype,doc), kora.deleteDoc(doctype,name), kora.secrets.get(key), kora.http.fetch({method,url,headers,body}), kora.context.user/roles/site, kora.now()\n\nEXAMPLE: {name:\"validate_order\", script_type:\"doc_event\", doctype:\"Order\", event:\"before_save\", script:\"if (!event.doc.total || event.doc.total <= 0) { throw new Error('Total required'); } return { doc: event.doc };\"}",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string", "description": "Unique script name (lowercase, underscores)"},
						"script_type": map[string]any{"type": "string", "description": "Type: doc_event, api_method, scheduled, computed, validate"},
						"script":      map[string]any{"type": "string", "description": "JavaScript code (ES5.1+). Access event.doc and kora.* API."},
						"doctype":     map[string]any{"type": "string", "description": "Target doctype (required for doc_event, computed, validate)"},
						"event":       map[string]any{"type": "string", "description": "Event name (required for doc_event)"},
						"method_path": map[string]any{"type": "string", "description": "API path (required for api_method, e.g., 'send_invoice')"},
						"schedule":    map[string]any{"type": "string", "description": "Cron expression (required for scheduled, e.g., '0 9 * * *')"},
						"description": map[string]any{"type": "string", "description": "Human-readable description of what this script does"},
						"priority":    map[string]any{"type": "integer", "description": "Execution order (1-100, default 10)"},
					},
					"required": []string{"name", "script_type", "script"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "script_list",
				"description": "List all scripts in this site with their types, status, and associated doctypes/events.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "script_validate",
				"description": "Validate JavaScript syntax WITHOUT saving. Always call this before script_create to catch errors early.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"script": map[string]any{"type": "string", "description": "JavaScript code to validate"},
					},
					"required": []string{"script"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "script_get",
				"description": "Get the full details and JavaScript source of a single script by name.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "Script name"},
					},
					"required": []string{"name"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "script_executions",
				"description": "View the last 10 execution logs for a script.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "Script name"},
					},
					"required": []string{"name"},
				},
			},
		},

		// View management tools.
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_views",
				"description": "List all views (screens) configured for this site. Shows name, route, type, layout, and component count.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_view",
				"description": "Get the full JSON configuration of a view by name. Use this before updating a view to see current state.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"name": map[string]any{"type": "string", "description": "View name"}},
					"required":   []string{"name"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "validate_view",
				"description": "Validate a view configuration WITHOUT saving. Checks source doctypes, field bindings, and component structure.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"view": map[string]any{"type": "object", "description": "View config to validate"}},
					"required":   []string{"view"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "create_view",
				"description": "Create a new view as DRAFT. Always call validate_view first. Available component types: record_table, record_list, record_cards, record_form, record_detail, filter_bar, search_box, split_view, kanban_board, approval_queue, calendar_view, dashboard_grid, metric_card, chart, scanner_input, product_grid, cart_panel, payment_panel, scanner_count, document_preview, confirmation_step, receipt_preview, drawer, category_tabs, tabs, line_item_builder, wizard, checklist, recent_records, public_form, workspace_dashboard, print_layout. Use JSON format: {\"name\":\"...\",\"route\":\"/...\",\"type\":\"workspace\",\"layout\":\"two_panel\",\"components\":[...]}",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"view": map[string]any{"type": "object", "description": "View config JSON"}},
					"required":   []string{"view"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "update_view",
				"description": "Update an existing view as DRAFT. Get current state via get_view first, modify the JSON, then pass the full config here.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "View name"},
						"view": map[string]any{"type": "object", "description": "Complete updated view config"},
					},
					"required": []string{"name", "view"},
				},
			},
		},
	}, metricsToolDefs()...)
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// ToolTurn is the model's tool calls for one step of a chat turn; RunToolTurn runs them.
type ToolTurn = toolCallBatch

// toolCallBatch is the model's tool calls for one step. A zero Execution falls
// back to the transaction manager's; Owner is replaced by the verified actor.
type toolCallBatch struct {
	Calls          []any
	Owner          string
	Site           string
	RunID          string
	StepID         string
	ConversationID string
	Execution      policy.Execution
	Turn           *answerTurn
	Commands       *kernel.CommandRegistry
	Operations     ModuleOperations
}

// RunToolTurn runs the model's tool calls and returns results in
// OpenAI tool message format. The acting user and roles are taken from the
// verified execution identity only; a caller cannot supply them.
// RunToolTurn runs the tool calls of one chat step: the chat route and the scripted turn tests use it.
func RunToolTurn(ctx context.Context, tx *orm.TxManager, reg *doctype.Registry, batch ToolTurn) []map[string]any {
	return executeToolCallsForAI(ctx, tx, reg, batch)
}

func executeToolCallsForAI(ctx context.Context, tx *orm.TxManager, reg *doctype.Registry, batch toolCallBatch) []map[string]any {
	if !batch.Execution.Valid() && tx != nil {
		batch.Execution = tx.Execution
	}
	if batch.Execution.Valid() {
		batch.Owner = executionOwner(batch.Execution)
		tx.Execution = batch.Execution
	}
	ctx = WithDialect(ctx, tx.Dialect)
	tx.Context = WithDialect(tx.Context, tx.Dialect)
	var results []map[string]any
	for _, tc := range batch.Calls {
		call, ok := tc.(map[string]any)
		if !ok {
			_ = RecordAudit(ctx, tx.DB, AuditEvent{
				Site:           batch.Site,
				RunID:          batch.RunID,
				StepID:         batch.StepID,
				ConversationID: batch.ConversationID,
				Kind:           "tool_call",
				Name:           "",
				Status:         "failed",
				Details: map[string]any{
					"error": "invalid tool call format from AI",
				},
			})
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": "unknown",
				"content":      "Error: invalid tool call format from AI",
			})
			continue
		}

		id := safeGetString(call, "id")
		fn := safeGetMap(call, "function")
		if fn == nil {
			_ = RecordAudit(ctx, tx.DB, AuditEvent{
				Site:           batch.Site,
				RunID:          batch.RunID,
				StepID:         batch.StepID,
				ConversationID: batch.ConversationID,
				Kind:           "tool_call",
				Name:           "",
				Status:         "failed",
				Details: map[string]any{
					"tool_call_id": id,
					"error":        "missing function in tool call",
				},
			})
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      "Error: missing function in tool call",
			})
			continue
		}

		name := safeGetString(fn, "name")
		argsJSON := safeGetString(fn, "arguments")

		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			_ = RecordAudit(ctx, tx.DB, AuditEvent{
				Site:           batch.Site,
				RunID:          batch.RunID,
				StepID:         batch.StepID,
				ConversationID: batch.ConversationID,
				Kind:           "tool_call",
				Name:           name,
				Status:         "failed",
				Details: map[string]any{
					"tool_call_id": id,
					"args_json":    argsJSON,
					"error":        err.Error(),
				},
			})
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      aiToolError{Code: errInvalidArguments, Message: "The arguments are not valid JSON.", Fix: "Send the same call again with valid JSON arguments."}.text(),
			})
			continue
		}

		result := executeSingleTool(tx, reg, toolInvocation{Name: name, Args: args, Owner: batch.Owner, Site: batch.Site, RunID: batch.RunID, StepID: batch.StepID, ConversationID: batch.ConversationID, PreviewWrites: true, IdempotencyKey: aiToolMutationIdempotencyKey(batch.Site, batch.RunID, batch.StepID, id), Execution: batch.Execution, Turn: batch.Turn, Commands: batch.Commands, Operations: batch.Operations}) //nolint:contextcheck // the context travels in tx.Context
		status := "completed"
		if isToolError(result) {
			status = "failed"
		}
		_ = RecordAudit(ctx, tx.DB, AuditEvent{
			Site:           batch.Site,
			RunID:          batch.RunID,
			StepID:         batch.StepID,
			ConversationID: batch.ConversationID,
			Kind:           "tool_call",
			Name:           name,
			Status:         status,
			Details: map[string]any{
				"tool_call_id": id,
				"args":         args,
			},
		})
		results = append(results, map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"content":      result,
		})
	}
	return results
}

// toolInvocation is one tool call to run under a verified execution.
// IdempotencyKey is the mutation receipt key; a zero Execution falls back to
// the transaction manager's.
type toolInvocation struct {
	Name           string
	Args           map[string]any
	Owner          string
	Site           string
	RunID          string
	StepID         string
	IdempotencyKey string
	Execution      policy.Execution
	Turn           *answerTurn
	// ConversationID and PreviewWrites are set by the chat loop. A chat write is previewed,
	// then runs only through confirm_write. Other callers keep the direct write path.
	ConversationID string
	PreviewWrites  bool
	// Commands resolves the pack commands a module-owned write must use. Nil names none.
	Commands *kernel.CommandRegistry
	// Operations runs the pack-action tools. Nil refuses them.
	Operations ModuleOperations
}

// executeSingleTool is the production tool path. DocType tools use the same
// role permission matrix as human requests; AI callers derive the mutation
// receipt key from the persisted run/step/tool-call identity.
func executeSingleTool(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation) string {
	toolName, args, owner, siteName, runID, stepID, execution := inv.Name, inv.Args, inv.Owner, inv.Site, inv.RunID, inv.StepID, inv.Execution
	if !execution.Valid() && tx != nil {
		execution = tx.Execution
	}
	if !execution.Valid() {
		return "Permission denied: verified execution is required"
	}
	if tx != nil {
		tx.Execution = execution
	}
	if err := requireRecentAuthForTool(tx.Context, toolName); err != nil {
		return err.Error()
	}
	approval := ApprovalTarget{Site: siteName, OperationID: runID, ToolName: toolName, Args: args}
	// --- System tools (no doctype prefix) ---
	switch toolName {
	case "mutate_records":
		return executeAIMutationBundle(tx, reg, inv) //nolint:contextcheck // the context travels in tx.Context
	case "list_doctypes":
		return executeListDoctypes(reg)
	case "validate_doctype_yaml":
		yamlStr, _ := args["yaml"].(string)
		return executeValidateYAML(yamlStr)
	case toolListMetrics, toolQueryMetrics, toolDrillMetrics, toolAskChoice:
		return executeMetricsTool(inv.Turn, toolName, args)
	case "create_doctype_draft":
		yamlStr, _ := args["yaml"].(string)
		if msg, held := holdForApproval(tx, approval, owner, stepID); held {
			return msg
		}
		result := executeCreateDoctypeDraft(tx, reg, yamlStr, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, approval, owner)
		return result
	case "update_doctype_draft":
		yamlStr, _ := args["yaml"].(string)
		if msg, held := holdForApproval(tx, approval, owner, stepID); held {
			return msg
		}
		result := executeUpdateDoctypeDraft(tx, reg, yamlStr, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, approval, owner)
		return result
	case "script_create":
		if !executionHasRole(execution, doctype.AdminRole) {
			return "Permission denied: only a workspace Administrator can author script drafts."
		}
		return executeScriptCreate(tx, args, siteName, owner)
	case "script_list":
		return executeScriptList(tx, siteName)
	case "script_validate":
		return executeScriptValidate(tx, args)
	case "script_get":
		return executeScriptGet(tx, args, siteName)
	case "script_executions":
		return executeScriptExecutions(tx, args, siteName)
	case "list_views":
		return executeListViews(reg)
	case "get_view":
		name, _ := args["name"].(string)
		return executeGetView(reg, name)
	case "validate_view":
		viewJSON := argsToJSON(args, "view")
		return executeValidateView(viewJSON, reg)
	case "create_view":
		viewJSON := argsToJSON(args, "view")
		if msg, held := holdForApproval(tx, approval, owner, stepID); held {
			return msg
		}
		result := executeCreateViewDraft(tx, reg, viewJSON, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, approval, owner)
		return result
	case "update_view":
		name, _ := args["name"].(string)
		viewJSON := argsToJSON(args, "view")
		if msg, held := holdForApproval(tx, approval, owner, stepID); held {
			return msg
		}
		result := executeUpdateViewDraft(tx, reg, viewUpdate{Name: name, ViewJSON: viewJSON, Owner: owner, Site: siteName})
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, approval, owner)
		return result
	}

	inv.Execution = execution
	if result, ok := executeFixedTool(tx, reg, inv); ok {
		return result
	}
	return fmt.Sprintf("Unknown tool: %s", toolName)
}

const recentAuthWindow = 10 * time.Minute

func requireRecentAuthForTool(ctx context.Context, toolName string) error {
	descriptor := toolDescriptorForName(toolName)
	if descriptor == nil || !descriptor.RequiresRecentAuth {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("recent authentication required for %s", toolName)
	}
	sessionAt, ok := auth.SessionCreatedAt(ctx)
	if !ok || sessionAt.IsZero() {
		return fmt.Errorf("recent authentication required for %s", toolName)
	}
	if time.Since(sessionAt) > recentAuthWindow {
		return fmt.Errorf("recent authentication required for %s", toolName)
	}
	return nil
}

func executionHasRole(execution policy.Execution, role string) bool {
	if !execution.Valid() || strings.TrimSpace(role) == "" {
		return false
	}
	for _, actorRole := range execution.Actor().Roles {
		if actorRole == role {
			return true
		}
	}
	return false
}

func toolDescriptorForName(toolName string) *ToolDescriptor {
	for _, dt := range buildSystemFunctions() {
		if descriptor, ok := descriptorFromRawFunction(dt); ok && descriptor.Name == toolName {
			return &descriptor
		}
	}
	return nil
}

// executeUpdateTool updates the record named by inv.Args from the remaining writable fields.
func executeUpdateTool(tx *orm.TxManager, reg *doctype.Registry, dt *doctype.DocType, inv toolInvocation) string {
	name, changes, refusal := updateChanges(dt, inv.Args)
	if refusal != nil {
		return refusal.text()
	}
	result, err := executeAIRecordMutation(tx, reg, recordMutation{Command: kernel.CommandRecordUpdate, Doctype: dt.Name, Name: name, Fields: changes, IdempotencyKey: inv.IdempotencyKey, Execution: inv.Execution})
	if err != nil {
		return recordWriteErrorText(err)
	}
	return fmt.Sprintf("Updated %s %q.", dt.Name, result.Name)
}

// updateChanges returns the record name and the writable fields of an update call, or the
// structured refusal when the name or a field is not writable.
func updateChanges(dt *doctype.DocType, args map[string]any) (string, map[string]any, *aiToolError) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, &aiToolError{Code: errMissingFields, Message: dt.Name + " update needs the record name.", Field: "name", Fix: "Find the record with find_records, then pass its name."}
	}
	if unknown := unknownFieldsExcept(args, dt, map[string]bool{"name": true}); len(unknown) > 0 {
		return "", nil, unknownFieldError(dt, unknown)
	}
	if refusal := readOnlyDataError(dt, args); refusal != nil {
		return "", nil, refusal
	}
	changes := map[string]any{}
	for key, value := range args {
		if key == "name" || strings.TrimSpace(key) == "" {
			continue
		}
		field := dt.GetField(key)
		if field == nil {
			continue
		}
		if field.Fieldtype == "Table" || field.Computed != "" {
			return "", nil, &aiToolError{Code: errInvalidValue, Message: "Field " + key + " is not writable.", Field: key, Fix: "Remove " + key + " from data."}
		}
		changes[key] = value
	}
	if len(changes) == 0 {
		return "", nil, &aiToolError{Code: errInvalidValue, Message: "Provide at least one writable field in data.", Field: "data"}
	}
	return name, changes, nil
}

// unknownFieldError names the fields the record type does not have, with the closest valid ones.
func unknownFieldError(dt *doctype.DocType, unknown []string) *aiToolError {
	return &aiToolError{
		Code:        errInvalidValue,
		Message:     "Unknown fields: " + strings.Join(unknown, ", ") + ".",
		Field:       unknown[0],
		Fix:         "Use only the field names listed for " + dt.Name + ".",
		ValidValues: limitValues(strings.Split(availableFieldNames(dt), ", ")),
	}
}

// readOnlyDataError refuses data that names a read-only field, with one sentence per field in
// field order. The name key is the update target, not data, so it is never checked.
func readOnlyDataError(dt *doctype.DocType, args map[string]any) *aiToolError {
	var sentences, named []string
	for _, field := range dt.DataFields() {
		if !field.ReadOnly || field.Fieldname == "name" {
			continue
		}
		if _, ok := args[field.Fieldname]; !ok {
			continue
		}
		sentences = append(sentences, readOnlyReason(dt, field))
		named = append(named, field.Fieldname)
	}
	if len(named) == 0 {
		return nil
	}
	return &aiToolError{Code: errInvalidValue, Message: strings.Join(sentences, " "), Field: named[0], Fix: "Leave " + strings.Join(named, ", ") + " out of data."}
}

// readOnlyReason is the sentence that tells the model why one field cannot be written.
func readOnlyReason(dt *doctype.DocType, field doctype.Field) string {
	label := field.Label
	if label == "" {
		label = field.Fieldname
	}
	why := "it is set by the system"
	if description := strings.TrimSuffix(strings.TrimSpace(field.Description), "."); description != "" {
		why = description
	}
	return label + " is read-only on " + dt.Name + ": " + why + "."
}

// recordMutation is one kernel record command issued on behalf of an AI tool.
// Authorization and exact delegated grants come from the verified Execution.
type recordMutation struct {
	Command        string
	Doctype        string
	Name           string
	Fields         map[string]any
	IdempotencyKey string
	Execution      policy.Execution
	// Preview validates and names the record without keeping any write.
	Preview bool
}

func executeAIRecordMutation(tx *orm.TxManager, reg *doctype.Registry, m recordMutation) (*kernel.ResultData, error) {
	command, doctypeName, name, fields, idempotencyKey, execution := m.Command, m.Doctype, m.Name, m.Fields, m.IdempotencyKey, m.Execution
	if tx == nil || !execution.Valid() {
		return nil, contract.NewError(contract.CodeUnauthenticated, "AI record mutation requires verified execution identity")
	}
	actor := execution.Actor()
	if actor.Site != tx.SiteName {
		return nil, fmt.Errorf("verified execution does not match AI tenant")
	}
	payload := struct {
		Doctype string         `json:"doctype"`
		Name    string         `json:"name,omitempty"`
		Data    map[string]any `json:"data"`
	}{Doctype: doctypeName, Name: name, Data: fields}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode record operation: %w", err)
	}
	site := tx.SiteName
	if site == "" {
		return nil, fmt.Errorf("operation site is missing")
	}
	actorRoles := actor.Roles
	role := ""
	if len(actorRoles) > 0 {
		role = actorRoles[0]
	}
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	tx.Execution = execution
	engine := kernel.New(tx.Dialect, tx.Outbox)
	engine.TxManager = tx
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	op := kernel.Operation{
		Context: kernel.OperationContext{Site: site, TenantID: execution.Tenant(), Execution: execution, BusinessScope: tx.BusinessScope, BusinessReferenceValidator: tx.BusinessReferenceValidator, Actor: actor, User: user, UserRole: role, Roles: actorRoles, Source: kernel.SourceAI, IdempotencyKey: idempotencyKey},
		Command: command, Payload: raw,
	}
	var result contract.CommandResult
	var cerr *contract.Error
	if m.Preview {
		result, cerr = engine.Preview(ctx, tx.DB, reg, op)
	} else {
		result, cerr = engine.Execute(ctx, tx.DB, reg, op)
	}
	if cerr != nil {
		return nil, cerr
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		return nil, fmt.Errorf("decode record result: %w", err)
	}
	return &operation, nil
}

func aiToolMutationIdempotencyKey(site, runID, stepID, toolCallID string) string {
	if site == "" || runID == "" || stepID == "" || toolCallID == "" {
		return ""
	}
	identity := site + "\x00" + runID + "\x00" + stepID + "\x00" + toolCallID
	digest := sha256.Sum256([]byte(identity))
	return "ai-tool-" + hex.EncodeToString(digest[:])
}

func channelToolMutationIdempotencyKey(site, clientKey string) string {
	clientKey = strings.TrimSpace(clientKey)
	if site == "" || clientKey == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(site + "\x00" + clientKey))
	return "channel-tool-" + hex.EncodeToString(digest[:])
}

// formatDocSummary is one find_records line. A Link field shows its name with the code (see linkNames.show).
func formatDocSummary(dt *doctype.DocType, doc *doctype.Document, index int, names linkNames) string {
	fields := map[string]any{}
	if doc != nil && doc.Fields != nil {
		fields = aiReadableDocumentFields(dt, doc.Fields)
	}
	primary := primaryDisplayValue(dt, fields)
	parts := []string{fmt.Sprintf("%d. %s", index, primary)}
	if doc != nil && strings.TrimSpace(doc.Name) != "" {
		parts = append(parts, "ID: "+strings.TrimSpace(doc.Name))
	}
	for _, f := range summaryFields(dt) {
		if f.Fieldname == dt.TitleField {
			continue
		}
		value, ok := fields[f.Fieldname]
		if !ok || isEmptyDisplayValue(value) {
			if isDateLikeDocField(f.Fieldtype) {
				parts = append(parts, fmt.Sprintf("%s: None", fieldDisplayLabel(f)))
			}
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", fieldDisplayLabel(f), summaryValueText(f, value, names)))
	}
	return strings.Join(parts, " - ")
}

// summaryValueText formats one value of a find_records line. A Link shows its name beside its code.
func summaryValueText(f doctype.Field, value any, names linkNames) string {
	if code, isText := value.(string); isText && f.Fieldtype == "Link" {
		return names.show(f.Fieldname, code)
	}
	return formatDocValue(f.Fieldname, f.Fieldtype, value)
}

func primaryDisplayValue(dt *doctype.DocType, fields map[string]any) string {
	candidates := []string{dt.TitleField, "name"}
	for _, f := range dt.DataFields() {
		if f.InListView && aiReadableField(f) {
			candidates = append(candidates, f.Fieldname)
		}
	}
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if value, ok := fields[name]; ok && !isEmptyDisplayValue(value) {
			return formatDocValue(name, "", value)
		}
	}
	return dt.Name
}

func summaryFields(dt *doctype.DocType) []doctype.Field {
	var fields []doctype.Field
	seen := map[string]bool{}
	add := func(f doctype.Field) {
		if seen[f.Fieldname] || !isUserDisplayField(f) || !aiReadableField(f) {
			return
		}
		seen[f.Fieldname] = true
		fields = append(fields, f)
	}
	for _, f := range dt.DataFields() {
		if f.InListView {
			add(f)
		}
	}
	for _, f := range dt.DataFields() {
		switch f.Fieldtype {
		case "Select", "Date", "Datetime", "Link", "Data", "Currency", "Int", "Float", "Percent", "Check":
			add(f)
		}
		if len(fields) >= 5 {
			break
		}
	}
	return fields
}

func isUserDisplayField(f doctype.Field) bool {
	if f.Fieldtype == "Table" || f.Fieldtype == "Section Break" || f.Fieldtype == "Column Break" || f.Fieldtype == "Heading" {
		return false
	}
	if f.Fieldname == "owner" || f.Fieldname == "creation" || f.Fieldname == "modified" || f.Fieldname == "modified_by" {
		return false
	}
	return true
}

func aiReadableField(field doctype.Field) bool {
	return !field.Hidden && field.Fieldtype != "Password"
}

func aiReadableDocumentFields(dt *doctype.DocType, fields map[string]any) map[string]any {
	visible := make(map[string]any, len(fields))
	if dt == nil {
		return visible
	}
	for _, field := range dt.DataFields() {
		if !aiReadableField(field) {
			continue
		}
		if value, ok := fields[field.Fieldname]; ok {
			visible[field.Fieldname] = value
		}
	}
	return visible
}

func isEmptyDisplayValue(value any) bool {
	if value == nil {
		return true
	}
	s := strings.TrimSpace(fmt.Sprint(value))
	return s == "" || s == "<nil>"
}

func fieldDisplayLabel(f doctype.Field) string {
	if strings.TrimSpace(f.Label) != "" {
		return strings.TrimSpace(f.Label)
	}
	return humanizeFieldLabel(f.Fieldname)
}

func humanizeFieldLabel(name string) string {
	parts := strings.Split(strings.ReplaceAll(name, "-", "_"), "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func isDateLikeDocField(fieldType string) bool {
	return fieldType == "Date" || fieldType == "Datetime"
}

func formatDocValue(fieldname, fieldType string, value any) string {
	if isEmptyDisplayValue(value) {
		return ""
	}
	text := fmt.Sprint(value)
	if isDateLikeDocField(fieldType) {
		if len(text) >= 10 && regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`).MatchString(text) {
			return text[:10]
		}
	}
	return formatCell(fieldname, value)
}

type toolFilterArg struct {
	Field string
	Op    string
	Value any
}

func buildValidatedFindArgs(dt *doctype.DocType, args map[string]any) (string, int, int, string, error) {
	for key := range args {
		switch key {
		case "filters", "limit", "offset", "order_by":
		default:
			return "", 0, 0, "", fmt.Errorf("unsupported find argument %q; use filters, limit, offset, and order_by", key)
		}
	}
	limit, err := boundedQueryInt(args["limit"], 5, 1, 100)
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("limit must be an integer between 1 and 100")
	}
	offset, err := boundedQueryInt(args["offset"], 0, 0, 10000)
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("offset must be an integer between 0 and 10000")
	}
	orderBy, _ := args["order_by"].(string)
	if strings.TrimSpace(orderBy) != "" && !validOrderByFields(dt, orderBy) {
		return "", 0, 0, "", fmt.Errorf("invalid order_by %q", orderBy)
	}
	filters, err := parseToolFilters(args["filters"])
	if err != nil {
		return "", 0, 0, "", err
	}
	ormFilters := make([][]any, 0, len(filters))
	for _, filter := range filters {
		value, err := validateAndCoerceFilter(dt, filter)
		if err != nil {
			return "", 0, 0, "", err
		}
		ormFilters = append(ormFilters, []any{filter.Field, strings.ToLower(filter.Op), value})
	}
	data, err := json.Marshal(ormFilters)
	if err != nil {
		return "", 0, 0, "", err
	}
	return string(data), limit, offset, orderBy, nil
}

func boundedQueryInt(value any, fallback, minimum, maximum int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case float32:
		number = float64(v)
	case int:
		number = float64(v)
	case int64:
		number = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, err
		}
		number = parsed
	default:
		return 0, fmt.Errorf("expected integer")
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < float64(minimum) || number > float64(maximum) {
		return 0, fmt.Errorf("integer outside allowed range")
	}
	return int(number), nil
}

func parseToolFilters(raw any) ([]toolFilterArg, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("filters must be an array")
	}
	if len(items) > 20 {
		return nil, fmt.Errorf("filters must contain at most 20 items")
	}
	filters := make([]toolFilterArg, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("each filter must be an object")
		}
		filter := toolFilterArg{
			Field: strings.TrimSpace(fmt.Sprint(obj["field"])),
			Op:    strings.TrimSpace(fmt.Sprint(obj["op"])),
			Value: obj["value"],
		}
		if filter.Field == "" || filter.Op == "" {
			return nil, fmt.Errorf("each filter requires field and op")
		}
		filters = append(filters, filter)
	}
	return filters, nil
}

func validateAndCoerceFilter(dt *doctype.DocType, filter toolFilterArg) (any, error) {
	fieldType, ok := filterFieldType(dt, filter.Field)
	if !ok {
		return nil, fmt.Errorf("unknown filter field %q", filter.Field)
	}
	op := strings.ToLower(filter.Op)
	if !operatorAllowedForFieldType(fieldType, op) {
		return nil, fmt.Errorf("operator %q is not valid for %s field %q", filter.Op, fieldType, filter.Field)
	}
	if op == "in" || op == "not in" || op == "between" {
		values, ok := filter.Value.([]any)
		if !ok {
			return nil, fmt.Errorf("operator %q requires an array value", filter.Op)
		}
		if op == "between" && len(values) != 2 {
			return nil, fmt.Errorf("between requires exactly two values")
		}
		if len(values) > 100 {
			return nil, fmt.Errorf("filter value array must contain at most 100 items")
		}
		out := make([]any, 0, len(values))
		for _, value := range values {
			coerced, err := coerceFilterValue(fieldType, value)
			if err != nil {
				return nil, err
			}
			out = append(out, coerced)
		}
		return out, nil
	}
	if op == "is" || op == "is not" {
		if filter.Value != nil {
			return nil, fmt.Errorf("%s only supports null", filter.Op)
		}
		return nil, nil
	}
	return coerceFilterValue(fieldType, filter.Value)
}

func filterFieldType(dt *doctype.DocType, field string) (string, bool) {
	switch field {
	case "name", "owner", "modified_by":
		return "Data", true
	case "creation", "modified":
		return "Datetime", true
	case "doc_status":
		return "Int", true
	}
	for _, f := range dt.DataFields() {
		if f.Fieldname == field && f.Fieldtype != "Table" && aiReadableField(f) {
			return f.Fieldtype, true
		}
	}
	return "", false
}

func operatorAllowedForFieldType(fieldType, op string) bool {
	switch op {
	case "=", "!=", "in", "not in", "is", "is not":
		return true
	case "like", "not like":
		return !isNumericOrBooleanField(fieldType)
	case ">", ">=", "<", "<=", "between":
		return fieldType == "Int" || fieldType == "Float" || fieldType == "Currency" || fieldType == "Percent" || fieldType == "Date" || fieldType == "Time" || fieldType == "Datetime"
	default:
		return false
	}
}

func isNumericOrBooleanField(fieldType string) bool {
	return fieldType == "Int" || fieldType == "Float" || fieldType == "Currency" || fieldType == "Percent" || fieldType == "Check"
}

func coerceFilterValue(fieldType string, value any) (any, error) {
	if text, ok := value.(string); ok && len(text) > 4096 {
		return nil, fmt.Errorf("filter text must be at most 4096 bytes")
	}
	switch fieldType {
	case "Int":
		switch v := value.(type) {
		case float64:
			return int(v), nil
		case string:
			return strconv.Atoi(strings.TrimSpace(v))
		default:
			return value, nil
		}
	case "Float", "Currency", "Percent":
		if s, ok := value.(string); ok {
			return strconv.ParseFloat(strings.TrimSpace(s), 64)
		}
	case "Check":
		if s, ok := value.(string); ok {
			return strconv.ParseBool(strings.TrimSpace(s))
		}
	case "Date":
		s := strings.TrimSpace(fmt.Sprint(value))
		if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(s) {
			return nil, fmt.Errorf("Date filters require YYYY-MM-DD values")
		}
		return s, nil
	case "Datetime":
		s := strings.TrimSpace(fmt.Sprint(value))
		if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`).MatchString(s) {
			return nil, fmt.Errorf("Datetime filters require ISO-like date-time values")
		}
		return s, nil
	}
	return value, nil
}

func validOrderByFields(dt *doctype.DocType, orderBy string) bool {
	parts := strings.Fields(orderBy)
	if len(parts) == 0 || len(parts) > 2 {
		return false
	}
	if _, ok := filterFieldType(dt, parts[0]); !ok {
		return false
	}
	if len(parts) == 2 {
		dir := strings.ToUpper(parts[1])
		return dir == "ASC" || dir == "DESC"
	}
	return true
}

// executeFixedTool runs the fixed record tools and the pack-action tools. It reports false for any other name.
func executeFixedTool(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation) (string, bool) {
	if inv.Name == toolConfirmWrite {
		return executeConfirmWrite(tx, reg, inv), true
	}
	if inv.Name == toolListActions || inv.Name == toolRunAction {
		return executePackAction(tx, inv), true
	}
	operation, ok := recordToolOperation[inv.Name]
	if !ok {
		return "", false
	}
	return executeRecordTool(tx, reg, inv, operation), true
}

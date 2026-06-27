package ai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

// ---------------------------------------------------------------------------
// Tool function generation
// ---------------------------------------------------------------------------

// fieldNamesForDescription returns the names of data fields for use in tool descriptions.
func fieldNamesForDescription(dt *doctype.DocType) []string {
	var names []string
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			name := f.Fieldname
			if f.Reqd {
				name += " (required)"
			}
			names = append(names, name)
		}
	}
	return names
}

func buildFunctions(reg *doctype.Registry) []map[string]any {
	var funcs []map[string]any
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		lower := sanitizeName(dt.Name)
		props := make(map[string]any)
		required := []string{}
		for _, f := range dt.DataFields() {
			if f.Fieldtype == "Table" {
				continue
			}
			s := map[string]any{"description": f.Label}
			switch f.Fieldtype {
			case "Int":
				s["type"] = "integer"
			case "Float", "Currency", "Percent":
				s["type"] = "number"
			case "Check":
				s["type"] = "boolean"
			default:
				s["type"] = "string"
			}
			props[f.Fieldname] = s
			if f.Reqd {
				required = append(required, f.Fieldname)
			}
		}

		funcs = append(funcs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_find",
				"description": "Find " + dt.Name + " by field values. Fields: " + strings.Join(fieldNamesForDescription(dt), ", "),
				"parameters": map[string]any{
					"type":       "object",
					"properties": props,
				},
			},
		}, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_list",
				"description": "List " + dt.Name + " documents (recent first). Use after _find to browse all records.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit":  map[string]any{"type": "integer", "description": "Max results (default 20)"},
						"offset": map[string]any{"type": "integer", "description": "Pagination offset"},
					},
				},
			},
		}, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_get",
				"description": "Get a single " + dt.Name + " by name",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "Document name"},
					},
					"required": []string{"name"},
				},
			},
		}, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_create",
				"description": "Create a new " + dt.Name + ". Available fields: " + strings.Join(fieldNamesForDescription(dt), ", "),
				"parameters": map[string]any{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		})
	}
	return funcs
}

// ---------------------------------------------------------------------------
// Tool name parsing (suffix-based — handles multi-word doctype names)
// ---------------------------------------------------------------------------

var knownOps = []string{"_find", "_list", "_get", "_create", "_update", "_delete"}

// parseToolName splits a tool name like "work_order_create" into doctype "work_order" and operation "create".
func parseToolName(toolName string) (doctypeName, operation string, ok bool) {
	for _, op := range knownOps {
		if strings.HasSuffix(toolName, op) {
			return strings.TrimSuffix(toolName, op), op[1:], true // op[1:] strips the leading underscore
		}
	}
	return "", "", false
}

func sanitizeName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// ---------------------------------------------------------------------------
// System-level tools — doctype creation, validation, dry-run.
// These always create as Draft. Only a human can activate a draft.
// ---------------------------------------------------------------------------

func buildSystemFunctions() []map[string]any {
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
    computed: SUM(items.line_total)
    read_only: true
  - fieldname: tax_rate
    fieldtype: Percent
    label: Tax Rate
    default: "16"
  - fieldname: tax_amount
    fieldtype: Currency
    label: Tax Amount
    computed: subtotal * tax_rate / 100
    read_only: true
  - fieldname: grand_total
    fieldtype: Currency
    label: Grand Total
    computed: subtotal + tax_amount
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
    computed: quantity * unit_price
    read_only: true`

	return []map[string]any{
		analyticsToolDef(),
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
				"description": "Validate a DocType YAML definition WITHOUT saving. Always call this first before create_doctype_draft. Returns syntax errors with line numbers and 'did you mean?' suggestions for unknown keys.\n\nFIELD TYPES: Data, Text, Text Editor, Int, Float, Currency, Percent, Check, Date, Time, Datetime, Select (with options), Link (set options to target doctype name), Dynamic Link, Table (set options to child doctype name), Attach, Attach Image, Password, JSON, Section Break, Column Break, Heading.\n\nFIELD PROPERTIES: reqd (required), unique (must be unique across all records), in_list_view (show in table), in_standard_filter (show in filter sidebar), search_index (full-text searchable), read_only (non-editable), bold (highlight in forms), default (default value).\n\nLINKED FIELDS: Use linked_field: \"target.fieldname\" on a Link field to auto-populate data from the linked document (e.g., linked_field: \"product.selling_price\" auto-fills the price when a Product is selected).\n\nDEPENDS_ON: Use depends_on: \"fieldname\" to show/hide a field based on another field. Use mandatory_depends_on: \"fieldname\" to make the dependency required.\n\nCONSTRAINTS: Per-field validation rules as array of {type, value, message}:\n- min: maximum numeric value\n- max: maximum numeric value\n- min_length: minimum string length\n- max_length: maximum string length\n- regex: pattern to match\n- one_of: array of allowed values\n- not_one_of: array of disallowed values\nCOMPUTED FIELDS: Use expressions like 'quantity * unit_price', 'SUM(items.line_total)', 'ROUND(expr, 2)'. Computed fields should be read_only: true.\n\nTABLE (CHILD TABLE): Create the child doctype FIRST (with is_child_table: true), then the parent. The child doctype name goes in the Table field's 'options'.\n\nSIMPLE EXAMPLE:\n" + simpleExample + "\n\nCOMPLEX EXAMPLE (with Table, computed, Link, Select):\n" + complexExample + "\n\nCHILD TABLE EXAMPLE:\n" + childTableExample,
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
				"description": "Create a NEW DocType as DRAFT only. Does NOT create database tables — a human must review and activate. If the doctype has a Table field, create the child doctype FIRST (as a separate call), then the parent. Always call validate_doctype_yaml before this. Only call this AFTER the user confirms they want to create.\n\nFIELD TYPES: Data, Text, Text Editor, Int, Float, Currency, Percent, Check, Date, Time, Datetime, Select (with options using | prefix for multi-line), Link (options = target doctype name), Dynamic Link, Table (options = child doctype name), Attach, Attach Image, Password, JSON, Section Break, Column Break, Heading.\n\nFIELD PROPERTIES: reqd, unique, in_list_view, in_standard_filter, search_index, read_only, bold, default, linked_field (auto-populate from linked doc), depends_on, mandatory_depends_on.\n\nCONSTRAINTS: Array of {type, value, message}. Types: min, max, min_length, max_length, regex, one_of, not_one_of.\n\nCOMPUTED: 'quantity * unit_price', 'SUM(items.line_total)', 'ROUND(expr, N)'. Set read_only: true.\n\nFor child tables: set is_child_table: true. Create child FIRST, then parent. Do NOT include table columns (parent, parentfield, parenttype, idx) — the system adds them automatically.\n\nSIMPLE EXAMPLE:\n" + simpleExample + "\n\nCOMPLEX EXAMPLE (with Table, Link, Select, computed fields, submittable):\n" + complexExample + "\n\nCHILD TABLE EXAMPLE:\n" + childTableExample,
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
				"description": "Create a new JavaScript script that automates business logic. Scripts run on document events, custom API endpoints, workflow actions, or schedules.\n\nSCRIPT TYPES:\n- doc_event: Runs on document lifecycle. Requires doctype + event.\n- api_method: Custom API at /api/method/{method_path}.\n- workflow_action: Fires on workflow transitions.\n- scheduled: Runs on a cron schedule.\n- computed: Computes field values from other fields.\n- validate: Custom validation logic.\n\nEVENTS (for doc_event): before_insert, after_insert, before_save, after_save, before_delete, after_delete, before_submit, after_submit, before_cancel, after_cancel, validate\n\nAVAILABLE JS API (kora global): kora.log.info/warn/error(msg...), kora.getDoc(doctype,name), kora.getList(doctype,filters,orderBy,limit,offset), kora.saveDoc(doctype,doc), kora.createDoc(doctype,doc), kora.deleteDoc(doctype,name), kora.secrets.get(key), kora.http.fetch({method,url,headers,body}), kora.context.user/roles/site, kora.now()\n\nEXAMPLE: {name:\"validate_order\", script_type:\"doc_event\", doctype:\"Order\", event:\"before_save\", script:\"if (!event.doc.total || event.doc.total <= 0) { throw new Error('Total required'); } return { doc: event.doc };\"}",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string", "description": "Unique script name (lowercase, underscores)"},
						"script_type": map[string]any{"type": "string", "description": "Type: doc_event, api_method, workflow_action, scheduled, computed, validate"},
						"script":      map[string]any{"type": "string", "description": "JavaScript code (ES5.1+). Access event.doc and kora.* API."},
						"doctype":     map[string]any{"type": "string", "description": "Target doctype (required for doc_event, computed, validate)"},
						"event":       map[string]any{"type": "string", "description": "Event name (required for doc_event)"},
						"method_path": map[string]any{"type": "string", "description": "API path (required for api_method, e.g., 'send_invoice')"},
						"schedule":    map[string]any{"type": "string", "description": "Cron expression (required for scheduled, e.g., '0 9 * * *')"},
						"workflow_action": map[string]any{"type": "string", "description": "Action name (required for workflow_action)"},
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
				"description": "View the last 10 execution logs for a script. Shows timestamps, status, duration, and error messages. Use to debug failing scripts.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "Script name"},
					},
					"required": []string{"name"},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// executeToolCallsForAI runs tool calls and returns results in OpenAI tool message format.
func executeToolCallsForAI(tx *orm.TxManager, reg *doctype.Registry, toolCalls []any, owner, siteName string) []map[string]any {
	var results []map[string]any
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
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
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      fmt.Sprintf("Error: invalid arguments JSON: %v. Arguments received: %s", err, argsJSON),
			})
			continue
		}

		result := executeSingleTool(tx, reg, name, args, owner, siteName)
		results = append(results, map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"content":      result,
		})
	}
	return results
}

func executeSingleTool(tx *orm.TxManager, reg *doctype.Registry, toolName string, args map[string]any, owner, siteName string) string {
	// --- System tools (no doctype prefix) ---
	switch toolName {
	case "list_doctypes":
		return executeListDoctypes(reg)
	case "validate_doctype_yaml":
		yamlStr, _ := args["yaml"].(string)
		return executeValidateYAML(yamlStr)
	case "analytics_insights":
		doctypeName, _ := args["doctype"].(string)
		return executeAnalyticsInsights(tx, reg, doctypeName, siteName)
	case "create_doctype_draft":
		yamlStr, _ := args["yaml"].(string)
		return executeCreateDoctypeDraft(tx, reg, yamlStr, owner, siteName)
	case "script_create":
		return executeScriptCreate(tx, args, siteName, owner)
	case "script_list":
		return executeScriptList(tx, siteName)
	case "script_validate":
		return executeScriptValidate(tx, args)
	case "script_get":
		return executeScriptGet(tx, args, siteName)
	case "script_executions":
		return executeScriptExecutions(tx, args, siteName)
	}

	// Parse tool name using suffix matching (handles multi-word doctype names like "Work Order").
	doctypeName, operation, ok := parseToolName(toolName)
	if !ok {
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}

	// Find the doctype. Try exact sanitized-name match first, then case-insensitive.
	var dt *doctype.DocType
	for _, d := range reg.All() {
		if sanitizeName(d.Name) == doctypeName {
			dt = d
			break
		}
	}
	if dt == nil {
		for _, d := range reg.All() {
			if strings.EqualFold(sanitizeName(d.Name), doctypeName) {
				dt = d
				break
			}
		}
	}
	if dt == nil {
		return fmt.Sprintf("DocType %q not found", doctypeName)
	}

	switch operation {
	case "find":
		// Build JSON filter array from provided field values, using proper JSON encoding.
		var filtParts []string
		for k, v := range args {
			if v != nil && v != "" && k != "limit" && k != "offset" {
				vJSON, err := json.Marshal(fmt.Sprintf("%v", v))
				if err != nil {
					vJSON = []byte(fmt.Sprintf(`"%v"`, v))
				}
				filtParts = append(filtParts, fmt.Sprintf(`["%s","=",%s]`, k, vJSON))
			}
		}
		filter := "[" + strings.Join(filtParts, ",") + "]"
		docs, total, err := tx.GetList(dt, filter, "", 5, 0, "")
		if err != nil {
			return fmt.Sprintf("Error finding %s: %v", dt.Name, err)
		}
		if total == 0 {
			return fmt.Sprintf("No %s found matching the criteria.", dt.Name)
		}
		// Return count + up to 3 top matches so the model can detect duplicates.
		var summaries []string
		maxShow := 3
		if len(docs) < maxShow {
			maxShow = len(docs)
		}
		for i := 0; i < maxShow; i++ {
			summaries = append(summaries, fmt.Sprintf("%v", docs[i].Fields))
		}
		if total > maxShow {
			summaries = append(summaries, fmt.Sprintf("... and %d more", total-maxShow))
		}
		return fmt.Sprintf("Found %d matching %s: %s", total, dt.Name, strings.Join(summaries, "; "))
	case "list":
		limit := 20
		if v, ok := args["limit"].(float64); ok {
			limit = int(v)
		}
		docs, total, err := tx.GetList(dt, "", "", limit, 0, "")
		if err != nil {
			return fmt.Sprintf("Error listing %s: %v", dt.Name, err)
		}
		if total == 0 {
			return fmt.Sprintf("No %s found.", dt.Name)
		}
		// Build a markdown table for clean presentation.
		var cols []string
		var colLabels []string
		for _, f := range dt.DataFields() {
			if f.Fieldtype == "Table" || f.Fieldtype == "Section Break" || f.Fieldtype == "Column Break" || f.Fieldtype == "Heading" {
				continue
			}
			cols = append(cols, f.Fieldname)
			colLabels = append(colLabels, f.Label)
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("**%d %s found:**", total, dt.Name))
		lines = append(lines, "")
		// Header row.
		lines = append(lines, "| "+strings.Join(colLabels, " | ")+" |")
		// Separator.
		var seps []string
		for range cols {
			seps = append(seps, "---")
		}
		lines = append(lines, "| "+strings.Join(seps, " | ")+" |")
		// Data rows.
		for _, doc := range docs {
			var vals []string
			for _, col := range cols {
				v := doc.Get(col)
				vals = append(vals, formatCell(col, v))
			}
			lines = append(lines, "| "+strings.Join(vals, " | ")+" |")
		}
		lines = append(lines, "")
		return strings.Join(lines, "\n")
	case "get":
		name, _ := args["name"].(string)
		doc, err := tx.GetDoc(dt, name, "")
		if err != nil {
			return fmt.Sprintf("%s %q not found.", dt.Name, name)
		}
		return fmt.Sprintf("%s %q: %v", dt.Name, name, doc.Fields)
	case "create":
		// Validate field names — reject unknown fields with a helpful message.
		if unknown := unknownFields(args, dt); len(unknown) > 0 {
			slog.Warn("Rejecting unknown fields in tool call", "unknown", unknown, "valid", availableFieldNames(dt), "doctype", dt.Name)
			return fmt.Sprintf("Error: unknown fields: %s. Valid fields: %s",
				strings.Join(unknown, ", "), availableFieldNames(dt))
		}
		doc := doctype.NewDocument(dt.Name)
		for k, v := range args {
			doc.Set(k, v)
		}
		if err := tx.Insert(dt, doc, owner, "ai-assistant"); err != nil {
			return fmt.Sprintf("Error creating %s: %v", dt.Name, err)
		}
		return fmt.Sprintf("Created %s %q.", dt.Name, doc.Name)
	default:
		return fmt.Sprintf("Unknown operation: %s", operation)
	}
}

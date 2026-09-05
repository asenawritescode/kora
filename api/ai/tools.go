package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
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

func BuildToolCatalog(reg *doctype.Registry) ToolCatalog {
	var catalog []ToolDescriptor
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		catalog = append(catalog, buildDoctypeToolDescriptors(dt)...)
	}
	for _, raw := range buildSystemFunctions() {
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

func buildDoctypeToolDescriptors(dt *doctype.DocType) []ToolDescriptor {
	lower := sanitizeName(dt.Name)
	common := func(name, operation, description string, schema map[string]any) ToolDescriptor {
		return ToolDescriptor{
			ID:                      "tenant." + name,
			Source:                  "tenant",
			Name:                    name,
			Description:             description,
			InputSchema:             schema,
			SafetyLevel:             classifyToolSafety(name),
			ChannelAllowlist:        []string{"web", "whatsapp"},
			ArgumentContractVersion: "v2",
			Operation:               operation,
			Doctype:                 dt.Name,
			DoctypeLabel:            dt.Name,
			TitleField:              dt.TitleField,
			SearchFields:            splitCSV(dt.SearchFields),
			SortField:               defaultString(dt.SortField, "modified"),
			SortOrder:               defaultString(dt.SortOrder, "DESC"),
			FieldHints:              buildFieldHints(dt),
			SystemFields:            systemFieldHints(),
		}
	}
	create := common(lower+"_create", "create", "Create a new "+dt.Name+". Available fields: "+strings.Join(fieldNamesForDescription(dt), ", "), buildCreateSchema(dt))
	create.RequiresConfirmation = true
	create.RequiresRecentAuth = true
	update := common(lower+"_update", "update", "Update an existing "+dt.Name+". Requires the stable document name and writable fields.", buildUpdateSchema(dt))
	update.RequiresConfirmation = true
	update.RequiresRecentAuth = true
	return []ToolDescriptor{
		common(lower+"_find", "find", "Find "+dt.Name+" records using typed filters.", buildFindSchema()),
		common(lower+"_list", "list", "List "+dt.Name+" documents (recent first). Use find when filters are present.", buildListSchema()),
		common(lower+"_get", "get", "Get a single "+dt.Name+" by name.", buildGetSchema()),
		create,
		update,
	}
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
	if name == "script_create" || name == "update_doctype_draft" || name == "create_doctype_draft" {
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

func buildFindSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"filters": map[string]any{
				"type":        "array",
				"description": "Typed filters. Each item is {field, op, value}.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"field": map[string]any{"type": "string"},
						"op":    map[string]any{"type": "string", "enum": []string{"=", "!=", ">", ">=", "<", "<=", "like", "not like", "in", "not in", "between", "is", "is not"}},
						"value": map[string]any{},
					},
					"required": []string{"field", "op", "value"},
				},
			},
			"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Max results (default 5)"},
			"offset":   map[string]any{"type": "integer", "minimum": 0, "description": "Pagination offset"},
			"order_by": map[string]any{"type": "string", "description": "Sort expression such as modified DESC using a valid field."},
		},
	}
}

func buildListSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"limit":  map[string]any{"type": "integer", "description": "Max results (default 20)"},
		"offset": map[string]any{"type": "integer", "description": "Pagination offset"},
	}}
}

func buildGetSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"name": map[string]any{"type": "string", "description": "Document name"},
	}, "required": []string{"name"}}
}

func buildCreateSchema(dt *doctype.DocType) map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" || f.ReadOnly || f.Computed != "" {
			continue
		}
		props[f.Fieldname] = aiFieldSchema(&f)
		if f.Reqd {
			required = append(required, f.Fieldname)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func buildUpdateSchema(dt *doctype.DocType) map[string]any {
	props := map[string]any{
		"name": map[string]any{"type": "string", "description": "Stable document name to update"},
	}
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" || f.ReadOnly || f.Computed != "" {
			continue
		}
		props[f.Fieldname] = aiFieldSchema(&f)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             []string{"name"},
	}
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

func buildFieldHints(dt *doctype.DocType) []FieldHint {
	hints := make([]FieldHint, 0, len(dt.DataFields()))
	for _, f := range dt.DataFields() {
		schema := aiFieldSchema(&f)
		hint := FieldHint{
			Name:           f.Fieldname,
			Label:          f.Label,
			Fieldtype:      f.Fieldtype,
			Type:           fmt.Sprint(schema["type"]),
			Required:       f.Reqd,
			ReadOnly:       f.ReadOnly,
			Computed:       f.Computed != "",
			Writable:       f.Fieldtype != "Table" && !f.ReadOnly && f.Computed == "",
			StandardFilter: f.InStandardFilter,
			SearchIndex:    f.SearchIndex,
			InListView:     f.InListView,
			Unique:         f.Unique,
			Description:    f.Description,
			Constraints:    f.Constraints,
		}
		if format, ok := schema["format"].(string); ok {
			hint.Format = format
		}
		if f.Fieldtype == "Select" {
			hint.Options = splitOptions(f.Options)
		}
		if f.Fieldtype == "Link" || f.Fieldtype == "Dynamic Link" {
			hint.LinkTarget = f.Options
		}
		if f.Fieldtype == "Table" {
			hint.TableTarget = f.Options
		}
		hints = append(hints, hint)
	}
	return hints
}

func systemFieldHints() []SystemFieldHint {
	return []SystemFieldHint{
		{Name: "name", Fieldtype: "Data"},
		{Name: "owner", Fieldtype: "Data"},
		{Name: "creation", Fieldtype: "Datetime"},
		{Name: "modified", Fieldtype: "Datetime"},
		{Name: "modified_by", Fieldtype: "Data"},
		{Name: "doc_status", Fieldtype: "Int"},
	}
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func ExecuteTool(tx *orm.TxManager, reg *doctype.Registry, toolName string, args map[string]any, owner, siteName string) string {
	return executeSingleTool(tx, reg, toolName, args, owner, siteName, "", "")
}

func classifyToolSafety(name string) string {
	switch {
	case strings.HasSuffix(name, "_find"), strings.HasSuffix(name, "_list"), strings.HasSuffix(name, "_get"), name == "list_doctypes", strings.Contains(name, "analytics"):
		return "safe"
	case strings.HasSuffix(name, "_create"), strings.HasSuffix(name, "_update"):
		return "guarded"
	case strings.HasSuffix(name, "_delete"), name == "create_doctype_draft", name == "update_doctype_draft", name == "script_create":
		return "admin"
	default:
		return "guarded"
	}
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

	return []map[string]any{
		analyticsToolDef(),
		analyticsCatalogToolDef(),
		analyticsQueryToolDef(),
		analyticsReportsToolDef(),
		runReportToolDef(),
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
				"description": "Create a new JavaScript script that automates business logic. Scripts run on document events, custom API endpoints, workflow actions, or schedules.\n\nSCRIPT TYPES:\n- doc_event: Runs on document lifecycle. Requires doctype + event.\n- api_method: Custom API at /api/method/{method_path}.\n- workflow_action: Fires on workflow transitions.\n- scheduled: Runs on a cron schedule.\n- computed: Computes field values from other fields.\n- validate: Custom validation logic.\n\nEVENTS (for doc_event): before_insert, after_insert, before_save, after_save, before_delete, after_delete, before_submit, after_submit, before_cancel, after_cancel, validate\n\nAVAILABLE JS API (kora global): kora.log.info/warn/error(msg...), kora.getDoc(doctype,name), kora.getList(doctype,filters,orderBy,limit,offset), kora.saveDoc(doctype,doc), kora.createDoc(doctype,doc), kora.deleteDoc(doctype,name), kora.secrets.get(key), kora.http.fetch({method,url,headers,body}), kora.context.user/roles/site, kora.now()\n\nEXAMPLE: {name:\"validate_order\", script_type:\"doc_event\", doctype:\"Order\", event:\"before_save\", script:\"if (!event.doc.total || event.doc.total <= 0) { throw new Error('Total required'); } return { doc: event.doc };\"}",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":            map[string]any{"type": "string", "description": "Unique script name (lowercase, underscores)"},
						"script_type":     map[string]any{"type": "string", "description": "Type: doc_event, api_method, workflow_action, scheduled, computed, validate"},
						"script":          map[string]any{"type": "string", "description": "JavaScript code (ES5.1+). Access event.doc and kora.* API."},
						"doctype":         map[string]any{"type": "string", "description": "Target doctype (required for doc_event, computed, validate)"},
						"event":           map[string]any{"type": "string", "description": "Event name (required for doc_event)"},
						"method_path":     map[string]any{"type": "string", "description": "API path (required for api_method, e.g., 'send_invoice')"},
						"schedule":        map[string]any{"type": "string", "description": "Cron expression (required for scheduled, e.g., '0 9 * * *')"},
						"workflow_action": map[string]any{"type": "string", "description": "Action name (required for workflow_action)"},
						"description":     map[string]any{"type": "string", "description": "Human-readable description of what this script does"},
						"priority":        map[string]any{"type": "integer", "description": "Execution order (1-100, default 10)"},
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
				"description": "Create a new view as DRAFT. Always call validate_view first. Available component types: record_table, record_list, record_cards, record_form, record_detail, filter_bar, search_box, workflow_actions, split_view, kanban_board, approval_queue, calendar_view, dashboard_grid, metric_card, chart, scanner_input, product_grid, cart_panel, payment_panel, scanner_count, document_preview, confirmation_step, receipt_preview, drawer, category_tabs, tabs, line_item_builder, wizard, checklist, recent_records, public_form, workspace_dashboard, print_layout. Use JSON format: {\"name\":\"...\",\"route\":\"/...\",\"type\":\"workspace\",\"layout\":\"two_panel\",\"components\":[...]}",
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
	}
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// executeToolCallsForAI runs tool calls and returns results in OpenAI tool message format.
func executeToolCallsForAI(ctx context.Context, tx *orm.TxManager, reg *doctype.Registry, toolCalls []any, owner, siteName, runID, stepID, conversationID string) []map[string]any {
	var results []map[string]any
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			_ = RecordAudit(ctx, tx.DB, AuditEvent{
				Site:           siteName,
				RunID:          runID,
				StepID:         stepID,
				ConversationID: conversationID,
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
				Site:           siteName,
				RunID:          runID,
				StepID:         stepID,
				ConversationID: conversationID,
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
				Site:           siteName,
				RunID:          runID,
				StepID:         stepID,
				ConversationID: conversationID,
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
				"content":      fmt.Sprintf("Error: invalid arguments JSON: %v. Arguments received: %s", err, argsJSON),
			})
			continue
		}

		result := executeSingleTool(tx, reg, name, args, owner, siteName, runID, stepID)
		status := "completed"
		if isToolError(result) {
			status = "failed"
		}
		_ = RecordAudit(ctx, tx.DB, AuditEvent{
			Site:           siteName,
			RunID:          runID,
			StepID:         stepID,
			ConversationID: conversationID,
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

func executeSingleTool(tx *orm.TxManager, reg *doctype.Registry, toolName string, args map[string]any, owner, siteName, runID, stepID string) string {
	if err := requireRecentAuthForTool(tx.Context, toolName); err != nil {
		return err.Error()
	}
	// --- System tools (no doctype prefix) ---
	switch toolName {
	case "list_doctypes":
		return executeListDoctypes(reg)
	case "validate_doctype_yaml":
		yamlStr, _ := args["yaml"].(string)
		return executeValidateYAML(yamlStr)
	case "get_analytics_catalog":
		return executeAnalyticsCatalog(reg, tx, siteName)
	case "query_analytics":
		return executeAnalyticsQuery(reg, tx, siteName, args)
	case "list_analytics_reports":
		return executeAnalyticsReports(reg, tx, siteName)
	case "run_analytics_report":
		return executeAnalyticsReport(reg, tx, siteName, args)
	case "get_analytics_insights", "analytics_insights":
		doctypeName, _ := args["doctype"].(string)
		return executeAnalyticsInsights(tx, reg, doctypeName, siteName)
	case "create_doctype_draft":
		yamlStr, _ := args["yaml"].(string)
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return "Approval required for create_doctype_draft. A durable approval has been recorded and the tool was not executed."
		}
		result := executeCreateDoctypeDraft(tx, reg, yamlStr, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return result
	case "update_doctype_draft":
		yamlStr, _ := args["yaml"].(string)
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return "Approval required for update_doctype_draft. A durable approval has been recorded and the tool was not executed."
		}
		result := executeUpdateDoctypeDraft(tx, reg, yamlStr, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return result
	case "script_create":
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return "Approval required for script_create. A durable approval has been recorded and the tool was not executed."
		}
		result := executeScriptCreate(tx, args, siteName, owner)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return result
	case "script_list":
		return executeScriptList(tx, siteName)
	case "script_validate":
		return executeScriptValidate(tx, args)
	case "script_get":
		return executeScriptGet(tx, args, siteName)
	case "script_executions":
		return executeScriptExecutions(tx, args, siteName)
	case "list_views":
		return executeListViews(reg, siteName, tx)
	case "get_view":
		name, _ := args["name"].(string)
		return executeGetView(reg, name)
	case "validate_view":
		viewJSON := argsToJSON(args, "view")
		return executeValidateView(viewJSON, reg)
	case "create_view":
		viewJSON := argsToJSON(args, "view")
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return "Approval required for create_view. A durable approval has been recorded and the tool was not executed."
		}
		result := executeCreateViewDraft(tx, reg, viewJSON, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return result
	case "update_view":
		name, _ := args["name"].(string)
		viewJSON := argsToJSON(args, "view")
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return "Approval required for update_view. A durable approval has been recorded and the tool was not executed."
		}
		result := executeUpdateViewDraft(tx, reg, name, viewJSON, owner, siteName)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return result
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
		filter, limit, offset, orderBy, err := buildValidatedFindArgs(dt, args)
		if err != nil {
			return fmt.Sprintf("Error finding %s: %v", dt.Name, err)
		}
		docs, total, err := tx.GetList(dt, filter, orderBy, limit, offset, "")
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
			summaries = append(summaries, formatDocSummary(dt, docs[i], i+1))
		}
		if total > maxShow {
			summaries = append(summaries, fmt.Sprintf("... and %d more", total-maxShow))
		}
		return fmt.Sprintf("Found %d matching %s:\n%s", total, dt.Name, strings.Join(summaries, "\n"))
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
		var summaries []string
		for i, doc := range docs {
			summaries = append(summaries, formatDocSummary(dt, doc, i+1))
		}
		return fmt.Sprintf("%d %s found:\n%s", total, dt.Name, strings.Join(summaries, "\n"))
	case "get":
		name, _ := args["name"].(string)
		doc, err := tx.GetDoc(dt, name, "")
		if err != nil {
			return fmt.Sprintf("%s %q not found.", dt.Name, name)
		}
		return fmt.Sprintf("%s %q: %v", dt.Name, name, doc.Fields)
	case "create":
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return fmt.Sprintf("Approval required for %s_create. A durable approval has been recorded and the tool was not executed.", dt.Name)
		}
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
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return fmt.Sprintf("Created %s %q.", dt.Name, doc.Name)
	case "update":
		if ok, _ := HasGrantedApproval(tx.Context, tx.DB, siteName, runID, toolName, args); !ok {
			_ = EnsureApprovalPending(tx.Context, tx.DB, siteName, runID, owner, "agent", toolName, 0, args)
			if stepID != "" && runID != "" {
				_ = MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "pending_approval")
			}
			return fmt.Sprintf("Approval required for %s_update. A durable approval has been recorded and the tool was not executed.", dt.Name)
		}
		result := executeUpdateTool(tx, reg, dt, args, owner)
		_, _ = GrantApprovalForOperation(tx.Context, tx.DB, siteName, runID, toolName, args, owner)
		return result
	default:
		return fmt.Sprintf("Unknown operation: %s", operation)
	}
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
	sessionAt, ok := ctx.Value("session_created_at").(time.Time)
	if !ok || sessionAt.IsZero() {
		return fmt.Errorf("recent authentication required for %s", toolName)
	}
	if time.Since(sessionAt) > recentAuthWindow {
		return fmt.Errorf("recent authentication required for %s", toolName)
	}
	return nil
}

func toolDescriptorForName(toolName string) *ToolDescriptor {
	for _, dt := range buildSystemFunctions() {
		if descriptor, ok := descriptorFromRawFunction(dt); ok && descriptor.Name == toolName {
			return &descriptor
		}
	}
	return nil
}

func executeUpdateTool(tx *orm.TxManager, reg *doctype.Registry, dt *doctype.DocType, args map[string]any, owner string) string {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Sprintf("Error updating %s: name is required.", dt.Name)
	}
	if unknown := unknownFieldsExcept(args, dt, map[string]bool{"name": true}); len(unknown) > 0 {
		return fmt.Sprintf("Error updating %s: unknown fields: %s. Valid fields: %s", dt.Name, strings.Join(unknown, ", "), availableFieldNames(dt))
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
		if field.Fieldtype == "Table" || field.ReadOnly || field.Computed != "" {
			return fmt.Sprintf("Error updating %s: field %s is not writable.", dt.Name, key)
		}
		changes[key] = value
	}
	if len(changes) == 0 {
		return fmt.Sprintf("Error updating %s: provide at least one writable field.", dt.Name)
	}
	oldDoc, err := tx.GetDoc(dt, name, "")
	if err != nil {
		return fmt.Sprintf("%s %q not found.", dt.Name, name)
	}
	doc := doctype.NewDocument(dt.Name)
	doc.Name = name
	doc.IsNew = false
	for _, f := range dt.DataFields() {
		doc.Set(f.Fieldname, oldDoc.Get(f.Fieldname))
	}
	for key, value := range changes {
		doc.Set(key, value)
	}
	if err := tx.RunHooksForValidate(dt, doc, oldDoc); err != nil {
		return fmt.Sprintf("Error updating %s: %v", dt.Name, err)
	}
	if validationErrs := doctype.ValidateDocument(dt, doc, reg, oldDoc); validationErrs.HasErrors() {
		return fmt.Sprintf("Error updating %s: %v", dt.Name, validationErrs)
	}
	if err := tx.Save(dt, doc, "ai-assistant", "", oldDoc); err != nil {
		return fmt.Sprintf("Error updating %s: %v", dt.Name, err)
	}
	return fmt.Sprintf("Updated %s %q.", dt.Name, doc.Name)
}

func formatDocSummary(dt *doctype.DocType, doc *doctype.Document, index int) string {
	fields := map[string]any{}
	if doc != nil && doc.Fields != nil {
		fields = doc.Fields
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
		parts = append(parts, fmt.Sprintf("%s: %s", fieldDisplayLabel(f), formatDocValue(f.Fieldname, f.Fieldtype, value)))
	}
	return strings.Join(parts, " - ")
}

func primaryDisplayValue(dt *doctype.DocType, fields map[string]any) string {
	candidates := []string{dt.TitleField, "name"}
	for _, f := range dt.DataFields() {
		if f.InListView {
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
		if seen[f.Fieldname] || !isUserDisplayField(f) {
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
	limit := intArg(args["limit"], 5)
	if limit < 1 || limit > 100 {
		return "", 0, 0, "", fmt.Errorf("limit must be between 1 and 100")
	}
	offset := intArg(args["offset"], 0)
	if offset < 0 {
		return "", 0, 0, "", fmt.Errorf("offset must be greater than or equal to 0")
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

func parseToolFilters(raw any) ([]toolFilterArg, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("filters must be an array")
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
		if f.Fieldname == field && f.Fieldtype != "Table" {
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

func intArg(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return parsed
		}
	}
	return fallback
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

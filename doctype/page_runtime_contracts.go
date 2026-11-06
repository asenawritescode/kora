package doctype

// PageValueContract describes the serialized shape of a component property or
// action input. Reference identifies values that resolve against site config.
type PageValueContract struct {
	Type      string
	Required  bool
	Reference string
	Values    []string
}

// PageBindingContract describes a component binding and, for field bindings,
// the DocType field types the component can consume.
type PageBindingContract struct {
	Kind                     string
	Default                  string
	Required                 bool
	FieldTypes               []string
	AllowedValues            []string
	ResourceRole             string
	RequiredWhenResourceRole string
}

// PageResourceBindingContract declares a semantic input resource a component
// consumes. The manifest binds the role to a page resource ID.
type PageResourceBindingContract struct {
	Required      bool
	Query         string
	MustMatchData bool
}

// PageResourceRelationContract ties two declared resource roles together via
// a field whose Link target must match the target resource DocType.
type PageResourceRelationContract struct {
	SourceResource      string
	FieldBinding        string
	TargetResource      string
	RequiredForLinkType bool
}

// PageComponentContract is the Engine's machine-readable contract for a
// component rendered by the Kora UI.
type PageComponentContract struct {
	Name            string
	Version         int
	RequiresData    bool
	Props           map[string]PageValueContract
	Bindings        map[string]PageBindingContract
	Resources       map[string]PageResourceBindingContract
	Relations       []PageResourceRelationContract
	RequiredActions []string
}

// PageActionContract describes an action's execution side and complete
// top-level input schema. Nested record-field maps receive DocType-aware checks.
type PageActionContract struct {
	Name      string
	Execution string
	Inputs    map[string]PageValueContract
}

var commonPageProps = map[string]PageValueContract{
	"title":           {Type: "string"},
	"source_doctype":  {Type: "string", Reference: "doctype"},
	"bindings":        {Type: "object"},
	"desktop_columns": {Type: "string_array"},
	"mobile_columns":  {Type: "string_array"},
	"actions":         {Type: "array"},
	"filters":         {Type: "array"},
}

func componentContract(name string, requiresData bool, bindings map[string]PageBindingContract) PageComponentContract {
	props := make(map[string]PageValueContract, len(commonPageProps))
	for key, value := range commonPageProps {
		props[key] = value
	}
	return PageComponentContract{Name: name, Version: 1, RequiresData: requiresData, Props: props, Bindings: bindings}
}

func fieldBinding(defaultField string, required bool, fieldTypes ...string) PageBindingContract {
	if len(fieldTypes) == 0 {
		fieldTypes = []string{"*"}
	}
	return PageBindingContract{Kind: "field", Default: defaultField, Required: required, FieldTypes: fieldTypes}
}

func literalBinding(required bool) PageBindingContract {
	return PageBindingContract{Kind: "literal", Required: required}
}

func enumBinding(defaultValue string, values ...string) PageBindingContract {
	return PageBindingContract{Kind: "enum", Default: defaultValue, AllowedValues: values}
}

func fieldListBinding(required bool) PageBindingContract {
	return PageBindingContract{Kind: "field_list", Required: required}
}

var pageComponentContracts = map[string]PageComponentContract{
	"record_table":   componentContract("record_table", true, nil),
	"record_form":    componentContract("record_form", true, nil),
	"record_detail":  componentContract("record_detail", true, nil),
	"record_list":    componentContract("record_list", true, nil),
	"record_cards":   componentContract("record_cards", true, nil),
	"filter_bar":     componentContract("filter_bar", false, nil),
	"search_box":     componentContract("search_box", false, nil),
	"insights_panel": componentContract("insights_panel", true, nil),
	"split_view":     componentContract("split_view", false, nil),
	"kanban_board": componentContract("kanban_board", true, map[string]PageBindingContract{
		"status": fieldBinding("status", false, "Data", "Select", "Link"), "title": fieldBinding("name", false), "priority": fieldBinding("", false, "Data", "Select", "Int"),
	}),
	"approval_queue": componentContract("approval_queue", true, map[string]PageBindingContract{
		"title": fieldBinding("name", false),
	}),
	"calendar_view": componentContract("calendar_view", true, map[string]PageBindingContract{
		"date": fieldBinding("date", false, "Date", "Datetime"), "title": fieldBinding("name", false, "Data", "Link", "Select"),
	}),
	"dashboard_grid": componentContract("dashboard_grid", false, nil),
	"metric_card": componentContract("metric_card", true, map[string]PageBindingContract{
		"metric": enumBinding("count", "count", "sum"), "value_field": fieldBinding("", false, "Int", "Float", "Currency", "Percent"),
		"filter_field": fieldBinding("", false), "filter_value": literalBinding(false), "title": literalBinding(false), "trend": enumBinding("", "up", "down", "flat"),
	}),
	"chart": componentContract("chart", true, map[string]PageBindingContract{
		"chart_type": enumBinding("bar", "bar", "line", "pie"),
	}),
	"scanner_input": componentContract("scanner_input", false, map[string]PageBindingContract{
		"placeholder": literalBinding(false), "search_fields": fieldListBinding(false),
	}),
	"product_grid": func() PageComponentContract {
		contract := componentContract("product_grid", true, map[string]PageBindingContract{
			"title": fieldBinding("", true, "Data", "Link", "Select"), "price": fieldBinding("", true, "Int", "Float", "Currency", "Percent"),
			"badge": fieldBinding("", false, "Int", "Float", "Currency"), "image": fieldBinding("", false, "Data", "Attach", "Attach Image"),
			"sku": fieldBinding("", false, "Data", "Link", "Select"), "subtitle": fieldBinding("", false, "Data", "Link", "Select"),
		})
		contract.RequiredActions = []string{"local_cart_add"}
		return contract
	}(),
	"cart_panel": componentContract("cart_panel", false, nil),
	"payment_panel": func() PageComponentContract {
		contract := componentContract("payment_panel", false, map[string]PageBindingContract{
			"methods": literalBinding(false), "register": literalBinding(false), "customer": literalBinding(false), "invoice_date": literalBinding(false), "due_date": literalBinding(false), "customer_name": literalBinding(false),
		})
		contract.RequiredActions = []string{"create_transaction"}
		return contract
	}(),
	"scanner_count":    componentContract("scanner_count", true, nil),
	"document_preview": componentContract("document_preview", true, nil),
	"confirmation_step": componentContract("confirmation_step", false, map[string]PageBindingContract{
		"message": literalBinding(false), "confirm_label": literalBinding(false), "role_required": literalBinding(false),
	}),
	"receipt_preview": componentContract("receipt_preview", false, nil),
	"drawer": componentContract("drawer", false, map[string]PageBindingContract{
		"open": {Kind: "boolean", Default: "false"},
	}),
	"category_tabs": func() PageComponentContract {
		contract := componentContract("category_tabs", true, map[string]PageBindingContract{
			"group_field":          {Kind: "field", Required: true, FieldTypes: []string{"Data", "Select", "Link"}, ResourceRole: "products"},
			"category_label_field": {Kind: "field", FieldTypes: []string{"Data", "Select"}, ResourceRole: "categories", RequiredWhenResourceRole: "categories"},
		})
		contract.Resources = map[string]PageResourceBindingContract{
			"products":   {Query: "document.list", MustMatchData: true},
			"categories": {Query: "document.list"},
		}
		contract.Relations = []PageResourceRelationContract{{
			SourceResource: "products", FieldBinding: "group_field", TargetResource: "categories", RequiredForLinkType: true,
		}}
		return contract
	}(),
	"tabs":              componentContract("tabs", false, nil),
	"line_item_builder": componentContract("line_item_builder", false, nil),
	"wizard":            componentContract("wizard", false, nil),
	"checklist": componentContract("checklist", false, map[string]PageBindingContract{
		"items": {Kind: "json"},
	}),
	"recent_records": componentContract("recent_records", true, map[string]PageBindingContract{
		"limit": {Kind: "integer", Default: "5"}, "title": fieldBinding("name", false), "date": fieldBinding("modified", false, "Date", "Datetime"),
	}),
	"public_form": componentContract("public_form", true, map[string]PageBindingContract{
		"fields": fieldListBinding(false), "labels": {Kind: "json"}, "submit_endpoint": literalBinding(false), "route": literalBinding(false),
		"success_message": literalBinding(false), "success_detail": literalBinding(false), "description": literalBinding(false), "submit_label": literalBinding(false),
	}),
	"workspace_dashboard": componentContract("workspace_dashboard", false, map[string]PageBindingContract{
		"subtitle": literalBinding(false),
	}),
	"print_layout": componentContract("print_layout", false, nil),
}

// publishedComponent builds a version 2 contract whose resource roles accept
// only owner-published queries; field bindings resolve against the bound
// query's allowed fields instead of DocType fields.
func publishedComponent(name string, resources map[string]PageResourceBindingContract, bindings map[string]PageBindingContract) PageComponentContract {
	contract := componentContract(name, len(resources) > 0, bindings)
	contract.Version = 2
	contract.Resources = resources
	return contract
}

func publishedField(role string, required bool) PageBindingContract {
	return PageBindingContract{Kind: "field", Required: required, FieldTypes: []string{"*"}, ResourceRole: role}
}

var publishedItems = PageResourceBindingContract{Required: true, Query: publishedQueryResource}

// publishedPageComponentContracts are the version 2 register contracts. A view
// component opts in with version: 2; version 1 views keep the contracts above.
var publishedPageComponentContracts = map[string]PageComponentContract{
	"product_grid": publishedComponent("product_grid", map[string]PageResourceBindingContract{"items": publishedItems}, map[string]PageBindingContract{
		"title": publishedField("items", true), "price": publishedField("items", true), "badge": publishedField("items", false),
		"image": publishedField("items", false), "sku": publishedField("items", false), "subtitle": publishedField("items", false),
	}),
	"category_tabs": publishedComponent("category_tabs", map[string]PageResourceBindingContract{"items": publishedItems, "categories": {Query: publishedQueryResource}}, map[string]PageBindingContract{
		"group_field":          publishedField("items", true),
		"category_label_field": {Kind: "field", FieldTypes: []string{"*"}, ResourceRole: "categories", RequiredWhenResourceRole: "categories"},
	}),
	"cart_panel": publishedComponent("cart_panel", nil, nil),
	"scanner_input": func() PageComponentContract {
		contract := publishedComponent("scanner_input", map[string]PageResourceBindingContract{lookupRole: publishedItems}, map[string]PageBindingContract{
			"term_input": {Kind: "query_input", Required: true, ResourceRole: lookupRole},
			"title":      publishedField(lookupRole, true), "price": publishedField(lookupRole, true), "sku": publishedField(lookupRole, false),
		})
		// The lookup runs on demand when a code is scanned, so no initial rows are loaded.
		contract.RequiresData = false
		return contract
	}(),
	"tender_panel": publishedComponent("tender_panel", nil, map[string]PageBindingContract{
		"submit_operation_ref": {Kind: "operation", Required: true}, "methods": literalBinding(false), "quick_cash": literalBinding(false),
	}),
}

func input(typeName string, required bool, reference string) PageValueContract {
	return PageValueContract{Type: typeName, Required: required, Reference: reference}
}

var pageActionContracts = map[string]PageActionContract{
	"document.create": {Name: "document.create", Execution: "client", Inputs: map[string]PageValueContract{"target_doctype": input("string", true, "doctype"), "fields": input("object", false, "target_fields")}},
	"document.update": {Name: "document.update", Execution: "client", Inputs: map[string]PageValueContract{"target_doctype": input("string", true, "doctype"), "fields": input("object", false, "target_fields")}},
	"document.delete": {Name: "document.delete", Execution: "client", Inputs: map[string]PageValueContract{"target_doctype": input("string", true, "doctype")}},
	"document.submit": {Name: "document.submit", Execution: "client", Inputs: map[string]PageValueContract{"target_doctype": input("string", true, "doctype")}},
	"document.cancel": {Name: "document.cancel", Execution: "client", Inputs: map[string]PageValueContract{"target_doctype": input("string", true, "doctype")}},
	"delete_record":   {Name: "delete_record", Execution: "client", Inputs: map[string]PageValueContract{"target_doctype": input("string", true, "doctype")}},
	"create_record": {Name: "create_record", Execution: "server", Inputs: map[string]PageValueContract{
		"target_doctype": input("string", true, "doctype"), "fields": input("object", false, "target_fields"),
	}},
	"update_record": {Name: "update_record", Execution: "server", Inputs: map[string]PageValueContract{
		"target_doctype": input("string", true, "doctype"), "fields": input("object", false, "target_fields"),
	}},
	"navigate":          {Name: "navigate", Execution: "client", Inputs: map[string]PageValueContract{"route": input("string", true, "route"), "params": input("object", false, "")}},
	"local_cart_add":    {Name: "local_cart_add", Execution: "client", Inputs: map[string]PageValueContract{}},
	"local_cart_remove": {Name: "local_cart_remove", Execution: "client", Inputs: map[string]PageValueContract{}},
	"local_state_set":   {Name: "local_state_set", Execution: "client", Inputs: map[string]PageValueContract{"key": input("string", true, ""), "value": input("any", true, "")}},
	"create_transaction": {Name: "create_transaction", Execution: "server", Inputs: map[string]PageValueContract{
		"target_doctype": input("string", true, "doctype"), "child_table": input("string", false, "child_doctype"), "parent_field": input("string", false, "parent_field"),
		"line_source": input("string", false, ""), "movement_date": input("string", false, ""), "reference_field": input("string", false, "target_field"),
		"reference_prefix": input("string", false, ""), "idempotency_prefix": input("string", false, ""), "fields": input("object", false, "target_fields"), "line_defaults": input("object", false, "child_fields"),
		"line_fields": input("object", false, "child_fields"), "totals": input("object", false, "transaction_totals"), "related_quantity_updates": input("object", false, "quantity_updates"),
		"decrement_stock": input("boolean", false, ""), "requires_operation_status": input("string", false, ""), "allowed_context_values": input("object", false, ""), "payment_script": input("string", false, "script"),
	}},
	"create_quantity_effect": {Name: "create_quantity_effect", Execution: "server", Inputs: map[string]PageValueContract{
		"target_doctype": input("string", true, "doctype"), "related_doctype": input("string", true, "doctype"),
		"source_record_field": input("string", true, "target_field"), "delta_field": input("string", true, "target_numeric_field"),
		"reason_field": input("string", true, "target_field"), "related_quantity_field": input("string", true, "related_numeric_field"),
	}},
	"initiate_external_operation": {Name: "initiate_external_operation", Execution: "server", Inputs: map[string]PageValueContract{
		"script": input("string", false, ""), "operation_type": input("string", false, ""), "purpose": input("string", false, ""),
		"source_doctype": input("string", false, "doctype"), "provider": input("string", false, ""), "currency": input("string", false, ""),
	}},
	"validate_external_operation": {Name: "validate_external_operation", Execution: "server", Inputs: map[string]PageValueContract{"script": input("string", false, "")}},
}

// PageComponentContractFor returns the registered contract for a component.
func PageComponentContractFor(name string) (PageComponentContract, bool) {
	contract, ok := pageComponentContracts[name]
	return contract, ok
}

// PageComponentContractForVersion returns the contract a view component
// selects with its version; zero and one select the original contract.
func PageComponentContractForVersion(name string, version int) (PageComponentContract, bool) {
	if version <= 1 {
		return PageComponentContractFor(name)
	}
	contract, ok := publishedPageComponentContracts[name]
	return contract, ok && contract.Version == version
}

// PageActionContractFor returns the registered contract for an action command.
func PageActionContractFor(name string) (PageActionContract, bool) {
	contract, ok := pageActionContracts[name]
	return contract, ok
}

// PageComponentContracts and PageActionContracts expose contract copies for
// diagnostics and conformance checks.
func PageComponentContracts() []PageComponentContract {
	contracts := make([]PageComponentContract, 0, len(pageComponentContracts))
	for _, contract := range pageComponentContracts {
		contracts = append(contracts, contract)
	}
	return contracts
}

func PageActionContracts() []PageActionContract {
	contracts := make([]PageActionContract, 0, len(pageActionContracts))
	for _, contract := range pageActionContracts {
		contracts = append(contracts, contract)
	}
	return contracts
}

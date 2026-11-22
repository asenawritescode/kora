package ai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// The fixed record tools. One set serves every record type the person may read,
// so the catalog does not grow with the number of record types in the packs.
const (
	toolFindRecords    = "find_records"
	toolGetRecord      = "get_record"
	toolDescribeRecord = "describe_record_type"
	toolCreateRecord   = "create_record"
	toolUpdateRecord   = "update_record"
)

// recordToolOperation maps each fixed record tool to the record operation it runs.
var recordToolOperation = map[string]string{
	toolFindRecords:    "find",
	toolGetRecord:      "get",
	toolDescribeRecord: "describe",
	toolCreateRecord:   "create",
	toolUpdateRecord:   "update",
}

// recordToolPermission maps each record operation to the role-matrix operation it needs.
var recordToolPermission = map[string]string{
	"find": "read", "get": "read", "describe": "read", "create": "create", "update": "write",
}

// maxFindRecords caps the rows find_records returns to the model.
const maxFindRecords = 20

// CatalogOptions scopes the record tools to one person. The zero value lists every
// non-child record type, which is the service catalog. With a verified execution the
// record_type enum lists only the types that person may read.
type CatalogOptions struct {
	Execution     policy.Execution
	BusinessScope contract.BusinessScope
	// Operations offers the pack-action tools when set. Nil leaves them out of the catalog.
	Operations ModuleOperations
}

// readableRecordTypes returns the sorted names of the non-child record types in
// scope, module-owned types included.
func readableRecordTypes(reg *doctype.Registry, opts CatalogOptions) []string {
	var names []string
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		if opts.Execution.Valid() && !mayReadRecordType(reg, opts, dt) {
			continue
		}
		names = append(names, dt.Name)
	}
	sort.Strings(names)
	return names
}

func mayReadRecordType(reg *doctype.Registry, opts CatalogOptions, dt *doctype.DocType) bool {
	_, err := policy.DecideForBusinessScope(opts.Execution, reg, dt.Name, "read", opts.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned)
	return err == nil
}

// recordToolDescriptors returns the five fixed record tools, or none when no record type is in scope.
func recordToolDescriptors(types []string) []ToolDescriptor {
	if len(types) == 0 {
		return nil
	}
	typeProp := map[string]any{"type": "string", "enum": types, "description": "Record type name from the enum."}
	nameProp := map[string]any{"type": "string", "description": "Stable document name."}
	dataProp := map[string]any{"type": "object", "description": "Field values by fieldname. Call describe_record_type first to see fields."}
	findProps := map[string]any{
		"record_type": typeProp,
		"search":      map[string]any{"type": "string", "description": "Text to match in the record title or name."},
		"filters":     findFiltersSchema(),
		"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": maxFindRecords, "description": "Max rows (default 5)."},
	}
	return []ToolDescriptor{
		recordTool(toolConfirmWrite, "Run a write that a preview returned. Call it only in a later message, after the person says yes, with the confirm_token from that preview.", objectSchema(map[string]any{"confirm_token": map[string]any{"type": "string", "description": "confirm_token from the latest preview."}}, "confirm_token"), "confirm"),
		recordTool(toolFindRecords, "Find or list records of one type, recent first. Returns names, titles and summary fields.", objectSchema(findProps, "record_type"), recordToolOperation[toolFindRecords]),
		recordTool(toolGetRecord, "Get one record by name.", objectSchema(map[string]any{"record_type": typeProp, "name": nameProp}, "record_type", "name"), recordToolOperation[toolGetRecord]),
		recordTool(toolDescribeRecord, "Describe a record type: fields with type, required, default, options, link target, read-only and computed flags, child tables and pack commands. Call before a write.", objectSchema(map[string]any{"record_type": typeProp}, "record_type"), recordToolOperation[toolDescribeRecord]),
		recordTool(toolCreateRecord, "Create one record of a type. Pack-owned types need their pack command instead; the error names it.", objectSchema(map[string]any{"record_type": typeProp, "data": dataProp}, "record_type", "data"), recordToolOperation[toolCreateRecord]),
		recordTool(toolUpdateRecord, "Update fields of one existing record by its stable name.", objectSchema(map[string]any{"record_type": typeProp, "name": nameProp, "data": dataProp}, "record_type", "name", "data"), recordToolOperation[toolUpdateRecord]),
	}
}

func recordTool(name, description string, schema map[string]any, operation string) ToolDescriptor {
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
	}
}

// IsRecordTool reports whether name is one of the fixed record tools, whose record type is in the arguments.
func IsRecordTool(name string) bool {
	_, ok := recordToolOperation[name]
	return ok
}

// RecordToolTarget names the record type and role-matrix operation a fixed record tool call touches.
// It reports false for any other tool, and for a record type the registry does not know.
func RecordToolTarget(reg *doctype.Registry, toolName string, args map[string]any) (string, string, bool) {
	operation, ok := recordToolOperation[toolName]
	if !ok {
		return "", "", false
	}
	recordType, _ := args["record_type"].(string)
	dt := reg.Get(strings.TrimSpace(recordType))
	if dt == nil || dt.IsChildTable {
		return "", "", false
	}
	return dt.Name, recordToolPermission[operation], true
}

// recordTypeNamed returns the non-child record type with the exact name, or nil.
func recordTypeNamed(reg *doctype.Registry, name string) *doctype.DocType {
	dt := reg.Get(strings.TrimSpace(name))
	if dt == nil || dt.IsChildTable {
		return nil
	}
	return dt
}

// executeRecordTool runs one fixed record tool. The record type is checked against the
// role matrix before any operation runs, so module-owned types need no separate path.
func executeRecordTool(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation, operation string) string {
	recordType, _ := inv.Args["record_type"].(string)
	dt := recordTypeNamed(reg, recordType)
	if dt == nil {
		return (&aiToolError{Code: errNotFound, Message: fmt.Sprintf("Unknown record type %q.", recordType), Field: "record_type", Fix: "Use a record_type from the enum."}).text()
	}
	rowScope, denied := authorizeRecordOperation(tx, reg, dt, recordToolPermission[operation], inv.Execution)
	if denied != "" {
		return denied
	}
	switch operation {
	case "find":
		return findRecords(tx, dt, inv.Args, rowScope)
	case "get":
		return getRecord(tx, dt, inv.Args, rowScope)
	case "describe":
		return describeRecordType(reg, dt, inv.Commands)
	case "create":
		return executeRecordCreate(tx, reg, inv, dt, rowScope)
	case "update":
		return executeRecordUpdate(tx, reg, inv, dt, rowScope)
	}
	return (&aiToolError{Code: errInvalidValue, Message: "Unknown operation " + operation + "."}).text()
}

// executeRecordCreate creates one record, or previews it in chat.
func executeRecordCreate(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation, dt *doctype.DocType, rowScope policy.RowScope) string {
	if refusal := moduleCommandRefusal(dt, inv.Commands); refusal != "" {
		return refusal
	}
	fields, refusal := recordDataArg(inv.Args)
	if refusal != "" {
		return refusal
	}
	if unknown := unknownFields(fields, dt); len(unknown) > 0 {
		return unknownFieldError(dt, unknown).text()
	}
	w := recordWrite{Command: kernel.CommandRecordCreate, Doctype: dt.Name, Fields: fields}
	if inv.PreviewWrites {
		return previewRecordWrite(tx, reg, inv, w, rowScope)
	}
	result, err := executeAIRecordMutation(tx, reg, recordMutation{Command: w.Command, Doctype: w.Doctype, Fields: fields, IdempotencyKey: inv.IdempotencyKey, Execution: inv.Execution})
	if err != nil {
		return recordWriteErrorText(err)
	}
	return fmt.Sprintf("Created %s %q.", dt.Name, result.Name)
}

// executeRecordUpdate updates one record, or previews the change in chat.
func executeRecordUpdate(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation, dt *doctype.DocType, rowScope policy.RowScope) string {
	if refusal := moduleCommandRefusal(dt, inv.Commands); refusal != "" {
		return refusal
	}
	fields, refusal := recordDataArg(inv.Args)
	if refusal != "" {
		return refusal
	}
	fields["name"] = inv.Args["name"]
	name, changes, uerr := updateChanges(dt, fields)
	if uerr != nil {
		return uerr.text()
	}
	if inv.PreviewWrites {
		return previewRecordWrite(tx, reg, inv, recordWrite{Command: kernel.CommandRecordUpdate, Doctype: dt.Name, Name: name, Fields: changes}, rowScope)
	}
	return executeUpdateTool(tx, reg, dt, toolInvocation{Args: fields, IdempotencyKey: inv.IdempotencyKey, Execution: inv.Execution})
}

// authorizeRecordOperation applies the role matrix to one record type. It returns the row
// scope to read or write under, or the refusal text to return to the model.
func authorizeRecordOperation(tx *orm.TxManager, reg *doctype.Registry, dt *doctype.DocType, permissionOperation string, execution policy.Execution) (policy.RowScope, string) {
	decision, decisionErr := policy.DecideForBusinessScope(execution, reg, dt.Name, permissionOperation, tx.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned)
	if decisionErr != nil {
		return policy.RowScope{}, (&aiToolError{Code: errPermissionDenied, Message: fmt.Sprintf("You may not %s %s.", permissionOperation, dt.Name), Fix: "Ask a person with access to make this change."}).text()
	}
	rowScope := policy.RowScope{Owner: decision.OwnerOnly, AssignedFields: append([]string(nil), decision.AssignedFields...), Business: tx.BusinessScope}
	if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
		actor := execution.Actor()
		rowScope.UserID = actor.SubjectUserID
		if rowScope.UserID == "" {
			rowScope.UserID = actor.PrincipalID
		}
		if rowScope.UserID == "" {
			return policy.RowScope{}, (&aiToolError{Code: errPermissionDenied, Message: "Row-scoped access needs a signed-in subject."}).text()
		}
	}
	return rowScope, ""
}

// recordDataArg returns the data object of a write, or the refusal text when it is missing.
func recordDataArg(args map[string]any) (map[string]any, string) {
	data, ok := args["data"].(map[string]any)
	if !ok || len(data) == 0 {
		return nil, (&aiToolError{Code: errMissingFields, Message: "data must be an object with at least one field value.", Field: "data", Fix: "Call describe_record_type, then pass the fields in data."}).text()
	}
	fields := make(map[string]any, len(data))
	for key, value := range data {
		fields[key] = value
	}
	return fields, ""
}

// findRecords runs find and list in one: filters, an optional text search on the title,
// recent first, capped by limit.
func findRecords(tx *orm.TxManager, dt *doctype.DocType, args map[string]any, rowScope policy.RowScope) string {
	filters := []any{}
	if existing, ok := args["filters"].([]any); ok {
		filters = append(filters, existing...)
	}
	if search, _ := args["search"].(string); strings.TrimSpace(search) != "" {
		field := dt.TitleField
		if field == "" {
			field = "name"
		}
		filters = append(filters, map[string]any{"field": field, "op": "like", "value": "%" + strings.TrimSpace(search) + "%"})
	}
	findArgs := map[string]any{"filters": filters}
	if limit, ok := args["limit"]; ok {
		findArgs["limit"] = limit
	}
	filter, limit, offset, orderBy, err := buildValidatedFindArgs(dt, findArgs)
	if err != nil {
		return aiToolError{Code: errInvalidValue, Message: "Could not search " + dt.Name + ": " + err.Error(), Fix: "Use fields listed by describe_record_type and supported operators."}.text()
	}
	docs, total, err := tx.GetListWithScope(dt, filter, orderBy, limit, offset, rowScope, nil, "")
	if err != nil {
		return aiToolError{Code: errGuardFailed, Message: "Could not search " + dt.Name + "."}.text()
	}
	if total == 0 {
		return fmt.Sprintf("No %s found.", dt.Name)
	}
	summaries := make([]string, 0, len(docs))
	for i, doc := range docs {
		summaries = append(summaries, formatDocSummary(dt, doc, i+1))
	}
	header := fmt.Sprintf("Found %d %s, showing %d:", total, dt.Name, len(docs))
	return header + "\n" + strings.Join(summaries, "\n")
}

func getRecord(tx *orm.TxManager, dt *doctype.DocType, args map[string]any, rowScope policy.RowScope) string {
	name, _ := args["name"].(string)
	doc, err := tx.GetDocWithScope(dt, name, rowScope)
	if err != nil {
		return (&aiToolError{Code: errNotFound, Message: fmt.Sprintf("%s %q was not found.", dt.Name, name), Field: "name", Fix: "Find the record with find_records and use its name."}).text()
	}
	result, marshalErr := json.Marshal(map[string]any{
		"doctype":  dt.Name,
		"name":     doc.Name,
		"revision": doc.Revision,
		"fields":   aiReadableDocumentFields(dt, doc.Fields),
	})
	if marshalErr != nil {
		return aiToolError{Code: errInvalidValue, Message: fmt.Sprintf("%s %q could not be read.", dt.Name, name)}.text()
	}
	return string(result)
}

// describedField is one field of describe_record_type.
type describedField struct {
	Name       string           `json:"name"`
	Label      string           `json:"label,omitempty"`
	Fieldtype  string           `json:"fieldtype"`
	Type       string           `json:"type"`
	Required   bool             `json:"required,omitempty"`
	Writable   bool             `json:"writable"`
	Default    string           `json:"default,omitempty"`
	Options    []string         `json:"options,omitempty"`
	LinkTarget string           `json:"link_target,omitempty"`
	Children   []describedField `json:"child_fields,omitempty"`
}

// describedRecordType is the describe_record_type result.
type describedRecordType struct {
	RecordType string           `json:"record_type"`
	TitleField string           `json:"title_field,omitempty"`
	Fields     []describedField `json:"fields"`
	Commands   []string         `json:"pack_commands,omitempty"`
}

func describeRecordType(reg *doctype.Registry, dt *doctype.DocType, commands *kernel.CommandRegistry) string {
	out := describedRecordType{RecordType: dt.Name, TitleField: dt.TitleField, Fields: describeFields(reg, dt)}
	if dt.ModuleOwnershipMode == string(contract.MutationCommands) {
		out.Commands = domainCommandNames(commands, dt.Name)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return aiToolError{Code: errInvalidValue, Message: "Could not describe " + dt.Name + "."}.text()
	}
	return string(data)
}

// describeFields lists the readable fields of dt. A child table field carries the fields of its child type.
func describeFields(reg *doctype.Registry, dt *doctype.DocType) []describedField {
	var fields []describedField
	for _, f := range dt.DataFields() {
		if !aiReadableField(f) {
			continue
		}
		d := describedField{
			Name:      f.Fieldname,
			Label:     f.Label,
			Fieldtype: f.Fieldtype,
			Type:      fmt.Sprint(aiFieldSchema(&f)["type"]),
			Required:  f.Reqd,
			Writable:  f.Fieldtype != "Table" && !f.ReadOnly && f.Computed == "",
			Default:   f.Default,
		}
		if f.Fieldtype == "Select" {
			d.Options = splitOptions(f.Options)
		}
		if f.Fieldtype == "Link" || f.Fieldtype == "Dynamic Link" {
			d.LinkTarget = f.Options
		}
		if f.Fieldtype == "Table" {
			if child := reg.Get(f.Options); child != nil {
				d.Children = describeChildFields(child)
			}
		}
		fields = append(fields, d)
	}
	return fields
}

// describeChildFields lists a child table's own fields; child tables do not nest further.
func describeChildFields(child *doctype.DocType) []describedField {
	var fields []describedField
	for _, f := range child.DataFields() {
		if f.Fieldtype == "Table" || !aiReadableField(f) {
			continue
		}
		d := describedField{Name: f.Fieldname, Label: f.Label, Fieldtype: f.Fieldtype, Type: fmt.Sprint(aiFieldSchema(&f)["type"]), Required: f.Reqd, Writable: !f.ReadOnly && f.Computed == "", Default: f.Default}
		if f.Fieldtype == "Select" {
			d.Options = splitOptions(f.Options)
		}
		if f.Fieldtype == "Link" || f.Fieldtype == "Dynamic Link" {
			d.LinkTarget = f.Options
		}
		fields = append(fields, d)
	}
	return fields
}

// domainCommandNames returns the sorted full names of the owner commands that touch recordType.
func domainCommandNames(commands *kernel.CommandRegistry, recordType string) []string {
	var names []string
	for _, command := range commands.OwnerCommands() {
		for _, touched := range command.TouchedRecords() {
			if touched == recordType {
				names = append(names, command.FullName())
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// moduleCommandRefusal refuses a direct write to a record type that only its pack's commands may change.
// It returns the structured error naming those commands, or "" when a direct write is allowed.
func moduleCommandRefusal(dt *doctype.DocType, commands *kernel.CommandRegistry) string {
	if dt.ModuleOwnershipMode != string(contract.MutationCommands) {
		return ""
	}
	return (&aiToolError{
		Code:        errModuleCommandRequired,
		Message:     dt.Name + " is changed only by its pack commands.",
		Field:       "record_type",
		Fix:         "Use one of the listed pack commands.",
		ValidValues: limitValues(domainCommandNames(commands, dt.Name)),
	}).text()
}

// findFiltersSchema is the typed filter list of find_records.
func findFiltersSchema() map[string]any {
	return map[string]any{
		"type":     "array",
		"maxItems": 20,
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
	}
}

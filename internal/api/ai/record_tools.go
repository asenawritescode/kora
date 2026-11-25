package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/textmatch"
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
		"search":      map[string]any{"type": "string", "description": "Text to match in the record title or name, or in the name of a linked record."},
		"filters":     findFiltersSchema(),
		"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": maxFindRecords, "description": "Max rows (default 5)."},
	}
	return []ToolDescriptor{
		recordTool(toolConfirmWrite, "Run a write that a preview returned. Call it only in a later message, after the person says yes, with the write_id from that preview.", objectSchema(map[string]any{"write_id": map[string]any{"type": "string", "description": "write_id from the latest preview."}}, "write_id"), "confirm"),
		recordTool(toolFindRecords, "Find or list records of one type, recent first. Returns names, titles and summary fields. Link filters accept a code or a name; names are matched.", objectSchema(findProps, "record_type"), recordToolOperation[toolFindRecords]),
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
		return findRecords(tx, dt, inv.Args, rowScope, inv.Execution)
	case "get":
		return getRecord(tx, dt, inv.Args, rowScope, inv.Execution)
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

// findRecords runs find and list in one: filters, a text search on the title, recent first, capped by limit.
// Link filters and the search match names and titles first (see resolveTitleFilters).
func findRecords(tx *orm.TxManager, dt *doctype.DocType, args map[string]any, rowScope policy.RowScope, execution policy.Execution) string {
	filters := []any{}
	if existing, ok := args["filters"].([]any); ok {
		filters = append(filters, existing...)
	}
	search, _ := args["search"].(string)
	resolved, err := resolveTitleFilters(context.Background(), tx, dt, titleInput{filters: filters, search: strings.TrimSpace(search), rowScope: rowScope, execution: execution})
	if err != nil {
		return aiToolError{Code: errGuardFailed, Message: "Could not search " + dt.Name + "."}.text()
	}
	if resolved.none != "" {
		return strings.Join(append(resolved.notes, resolved.none), "\n")
	}
	findArgs := map[string]any{"filters": resolved.filters}
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
		return noRowsText(context.Background(), tx, dt, rowScope, resolved)
	}
	// Names are read before showLinkTitles, which replaces the title field's code with its title.
	names := readLinkNames(context.Background(), tx, dt, docs, execution)
	if err := showLinkTitles(tx, dt, docs); err != nil {
		return aiToolError{Code: errGuardFailed, Message: "Could not search " + dt.Name + "."}.text()
	}
	summaries := make([]string, 0, len(docs))
	for i, doc := range docs {
		summaries = append(summaries, formatDocSummary(dt, doc, i+1, names))
	}
	header := fmt.Sprintf("Found %d %s, showing %d:", total, dt.Name, len(docs))
	return strings.Join(append(resolved.notes, header+"\n"+strings.Join(summaries, "\n")), "\n")
}

// showLinkTitles sets each document's title field to its display title, so a Link title shows the linked
// record's title rather than its code. A document without the field keeps it absent.
func showLinkTitles(tx *orm.TxManager, dt *doctype.DocType, docs []*doctype.Document) error {
	if dt.TitleField == "" || len(docs) == 0 {
		return nil
	}
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		names = append(names, doc.Name)
	}
	found, err := tx.ResolveTitles(context.Background(), dt, names)
	if err != nil {
		return err
	}
	for _, doc := range docs {
		if _, ok := doc.Fields[dt.TitleField]; ok && found[doc.Name] != "" {
			doc.Fields[dt.TitleField] = found[doc.Name]
		}
	}
	return nil
}

// getRecord returns one record. Link fields keep their codes; link_titles adds the names of the codes the person
// may see named (see readLinkNames). A name that is not a code may be a title, which opens its record with a note.
func getRecord(tx *orm.TxManager, dt *doctype.DocType, args map[string]any, rowScope policy.RowScope, execution policy.Execution) string {
	name, _ := args["name"].(string)
	doc, note, err := openRecord(tx, dt, name, rowScope)
	if err != nil {
		return recordNotFound(tx, dt, name, rowScope)
	}
	payload := map[string]any{
		"doctype":  dt.Name,
		"name":     doc.Name,
		"revision": doc.Revision,
		"fields":   aiReadableDocumentFields(dt, doc.Fields),
	}
	if note != "" {
		payload["note"] = note
	}
	names := readLinkNames(context.Background(), tx, dt, []*doctype.Document{doc}, execution)
	if titles := names.fieldTitles(readableLinkFields(dt), doc); len(titles) > 0 {
		payload["link_titles"] = titles
	}
	result, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return aiToolError{Code: errInvalidValue, Message: fmt.Sprintf("%s %q could not be read.", dt.Name, name)}.text()
	}
	return string(result)
}

// openRecord returns the record a name opens. A name that is not a code of the type opens the record whose title
// it names, and the note says which code that is. The error is the code lookup's when no title matches either.
func openRecord(tx *orm.TxManager, dt *doctype.DocType, name string, scope policy.RowScope) (*doctype.Document, string, error) {
	doc, err := tx.GetDocWithScope(dt, name, scope)
	if err == nil {
		return doc, "", nil
	}
	typed := typedName(strings.TrimSpace(name))
	matches, matchErr := tx.MatchTitles(context.Background(), dt, string(typed), scope)
	if matchErr != nil {
		return nil, "", err
	}
	code, ok := typed.openTitle(matches)
	if !ok {
		return nil, "", err
	}
	doc, err = tx.GetDocWithScope(dt, code, scope)
	if err != nil {
		return nil, "", err
	}
	return doc, fmt.Sprintf("%s is %s.", typed, code), nil
}

// recordNotFound is the not-found error of get_record. When codes near the typed name exist, it suggests them.
func recordNotFound(tx *orm.TxManager, dt *doctype.DocType, name string, scope policy.RowScope) string {
	message := fmt.Sprintf("%s %q was not found.", dt.Name, name)
	if near := typedName(strings.TrimSpace(name)).nearCodes(recentRecordNames(tx, dt, scope)); len(near) > 0 {
		message += " Did you mean " + strings.Join(near, ", ") + "?"
	}
	return aiToolError{Code: errNotFound, Message: message, Field: "name", Fix: "Find the record with find_records and use its name."}.text()
}

// recentRecordNames returns the names of the most recent records the scope allows, for suggestions.
func recentRecordNames(tx *orm.TxManager, dt *doctype.DocType, scope policy.RowScope) []string {
	docs, _, err := tx.GetListWithScope(dt, "", "name DESC", maxRecentNames, 0, scope, nil, "")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		names = append(names, doc.Name)
	}
	return names
}

// Limits of get_record's suggestions: the codes within two edits of a typed name, at most three of them, read from
// the most recent fifty records.
const (
	maxNearCodeDistance = 2
	maxSuggestedCodes   = 3
	maxRecentNames      = 50
)

// typedName is a record name the person typed. It may be a code in another case or a title, such as "bill-001"
// for the bill with the code SB-0001 and the title "BILL-001".
type typedName string

// openTitle returns the code of the record the typed name names: the one code match, or the one match whose title
// equals the text ignoring case. It returns false when no single record is named.
func (n typedName) openTitle(matches orm.TitleMatches) (string, bool) {
	if matches.Kind == orm.TitleKindCode && len(matches.Matches) == 1 {
		return matches.Matches[0].Name, true
	}
	var equal []string
	for _, match := range matches.Matches {
		if strings.EqualFold(match.Title, string(n)) {
			equal = append(equal, match.Name)
		}
	}
	if len(equal) != 1 {
		return "", false
	}
	return equal[0], true
}

// nearCodes returns up to maxSuggestedCodes codes within maxNearCodeDistance edits of the typed name, ignoring case:
// the nearest first, then by code.
func (n typedName) nearCodes(codes []string) []string {
	type near struct {
		code string
		dist int
	}
	typed := strings.ToLower(string(n))
	var found []near
	for _, code := range codes {
		if dist := textmatch.Distance(strings.ToLower(code), typed); dist <= maxNearCodeDistance {
			found = append(found, near{code: code, dist: dist})
		}
	}
	slices.SortFunc(found, func(a, b near) int {
		if a.dist != b.dist {
			return a.dist - b.dist
		}
		return strings.Compare(a.code, b.code)
	})
	var out []string
	for i := 0; i < len(found) && i < maxSuggestedCodes; i++ {
		out = append(out, found[i].code)
	}
	return out
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

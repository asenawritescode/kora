package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// The pack-action tools. Pack commands (Sale, Shift, Till Session, Cash Movement…) are
// published operations, so the model finds them with list_actions and runs them with
// run_action. Both reach the tenant's operations through ModuleOperations.
const (
	toolListActions = "list_actions"
	toolRunAction   = "run_action"
)

// ModuleOperations is the tenant's published operations as the pack-action tools see them.
// internal/api supplies it per request, so this package does not import the HTTP layer.
// Catalog lists what the execution may run now. Run executes one operation with the same
// checks as the HTTP operation route.
type ModuleOperations interface {
	Catalog(execution policy.Execution, scope contract.BusinessScope) (contract.ApplicationCatalog, error)
	Run(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error)
	// Preview checks one action as Run would, and keeps nothing: its writes roll back.
	Preview(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error)
}

// OperationError is a refused pack action: a stable code, the message, and the field it
// names when known. Module implementations return it so the model sees the reason.
type OperationError struct {
	Code    string
	Message string
	Field   string
}

func (e OperationError) Error() string { return e.Code + ": " + e.Message }

// packActionDescriptors returns the two pack-action tools. They are offered only when the
// caller supplies ModuleOperations, so a surface without tenant operations never lists them.
func packActionDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
		recordTool(toolListActions, "List the pack actions you may run now, such as recording a sale or opening a till. Filter by record_type. Pass action to see the input fields it takes.",
			objectSchema(map[string]any{
				"record_type": map[string]any{"type": "string", "description": "Record type the action changes, such as Sale."},
				"action":      map[string]any{"type": "string", "description": "Action id from an earlier list_actions result."},
			}), "list"),
		recordTool(toolRunAction, "Run one pack action listed by list_actions, with its input. Confirm the action with the person first.",
			objectSchema(map[string]any{
				"action": map[string]any{"type": "string", "description": "Action id from list_actions."},
				"input":  map[string]any{"type": "object", "description": "Input fields of the action, as described by list_actions."},
			}, "action", "input"), "execute"),
	}
}

// packAction is one action in a list_actions result.
type packAction struct {
	Action     string `json:"action"`
	Name       string `json:"name"`
	Title      string `json:"title"`
	RecordType string `json:"record_type,omitempty"`
}

// executePackAction runs list_actions or run_action. Without ModuleOperations it refuses.
func executePackAction(tx *orm.TxManager, inv toolInvocation) string {
	if inv.Operations == nil {
		return aiToolError{Code: errPermissionDenied, Message: "Pack actions are not available in this context."}.text()
	}
	catalog, err := inv.Operations.Catalog(inv.Execution, tx.BusinessScope)
	if err != nil {
		return aiToolError{Code: errGuardFailed, Message: "Pack actions are unavailable."}.text()
	}
	if inv.Name == toolListActions {
		return listPackActions(catalog, inv)
	}
	return runPackAction(tx, catalog, inv)
}

// listPackActions returns the actions the person may run, filtered by record_type, or the
// input description of one action when action is given.
func listPackActions(catalog contract.ApplicationCatalog, inv toolInvocation) string {
	actions := runnablePackActions(catalog, inv.Commands)
	if ref, _ := inv.Args["action"].(string); strings.TrimSpace(ref) != "" {
		return describePackAction(catalog, actions, strings.TrimSpace(ref))
	}
	recordType, _ := inv.Args["record_type"].(string)
	matched := make([]packAction, 0, len(actions))
	for _, action := range actions {
		if recordType == "" || strings.EqualFold(action.RecordType, strings.TrimSpace(recordType)) {
			matched = append(matched, action)
		}
	}
	data, err := json.Marshal(map[string]any{"actions": matched})
	if err != nil {
		return aiToolError{Code: errInvalidValue, Message: "Pack actions could not be listed."}.text()
	}
	return string(data)
}

// runnablePackActions returns the available published operations of the catalog. The catalog
// already holds only the operations the execution may run in this scope, so availability is
// the one filter left. A pack command's record type comes from its command definition.
func runnablePackActions(catalog contract.ApplicationCatalog, commands *kernel.CommandRegistry) []packAction {
	actions := make([]packAction, 0, len(catalog.PublishedOperations))
	for _, op := range catalog.PublishedOperations {
		if op.Availability != contract.OperationAvailable {
			continue
		}
		title := op.Presentation.Title
		if title == "" {
			title = humanizeIdentifier(op.Operation)
		}
		actions = append(actions, packAction{Action: op.OperationRef, Name: op.Operation, Title: title, RecordType: commandRecordType(commands, op)})
	}
	return actions
}

// commandRecordType returns the record a published operation's command changes, or "" when
// no command definition matches it.
func commandRecordType(commands *kernel.CommandRegistry, op contract.ApplicationCatalogOperation) string {
	for _, cmd := range commands.OwnerCommands() {
		if cmd.ModuleOwnerID == op.OwnerModuleID && cmd.Capability == op.Capability && cmd.Operation == op.Operation {
			return cmd.Input.Record
		}
	}
	return ""
}

// describePackAction returns one action with its trimmed input schema, so the model fills
// only fields that exist and sees the required ones and their enums.
func describePackAction(catalog contract.ApplicationCatalog, actions []packAction, ref string) string {
	action, ok := findPackAction(actions, ref)
	if !ok {
		return actionRefusal("unknown_action", "No available action has id "+ref+". Call list_actions to see the actions.", "")
	}
	var op contract.ApplicationCatalogOperation
	for _, candidate := range catalog.PublishedOperations {
		if candidate.OperationRef == ref {
			op = candidate
		}
	}
	input := map[string]any{}
	for _, doc := range catalog.Schemas {
		if doc.Ref == op.InputSchema {
			var schema map[string]any
			if err := json.Unmarshal(doc.Schema, &schema); err == nil {
				input = trimSchema(schema)
			}
		}
	}
	data, err := json.Marshal(map[string]any{"action": action.Action, "title": action.Title, "record_type": action.RecordType, "input": input})
	if err != nil {
		return aiToolError{Code: errInvalidValue, Message: "Pack action could not be described."}.text()
	}
	return string(data)
}

// trimSchema keeps the parts of a JSON schema a caller fills in: type, enum, description,
// required, properties (recursively) and items. Other keywords are dropped.
func trimSchema(node map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"type", "enum", "description", "required"} {
		if value, ok := node[key]; ok {
			out[key] = value
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		trimmed := make(map[string]any, len(props))
		for name, raw := range props {
			if child, ok := raw.(map[string]any); ok {
				trimmed[name] = trimSchema(child)
			}
		}
		out["properties"] = trimmed
	}
	if items, ok := node["items"].(map[string]any); ok {
		out["items"] = trimSchema(items)
	}
	return out
}

// runPackAction runs one listed action. The action must be in the runnable list, the input
// must be an object, and the call uses the tool-call idempotency key, so a replay returns
// the first receipt instead of writing again.
func runPackAction(tx *orm.TxManager, catalog contract.ApplicationCatalog, inv toolInvocation) string {
	ref, _ := inv.Args["action"].(string)
	action, ok := findPackAction(runnablePackActions(catalog, inv.Commands), strings.TrimSpace(ref))
	if !ok {
		return actionRefusal("unknown_action", "No available action has id "+strings.TrimSpace(ref)+". Call list_actions to see the actions.", "")
	}
	input, refusal := packActionInput(inv.Args)
	if refusal != "" {
		return refusal
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return actionRefusal("validation.invalid_input", "Input must be an object of field values.", "")
	}
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if inv.PreviewWrites {
		return previewPackAction(tx, catalog, inv, action, input)
	}
	receipt, err := inv.Operations.Run(ctx, inv.Execution, tx.BusinessScope, action.Action, contract.ExecutePublishedOperation{
		Meta:        contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(inv.IdempotencyKey)},
		InputSchema: inputSchemaOf(catalog, action.Action),
		Input:       encoded,
	})
	if err != nil {
		return operationErrorText(err)
	}
	return receiptText(action.Action, receipt)
}

// packActionInput returns the input object of run_action. Absent input is an empty object; any
// other non-object value is refused.
func packActionInput(args map[string]any) (map[string]any, string) {
	raw, present := args["input"]
	if !present || raw == nil {
		return map[string]any{}, ""
	}
	object, isObject := raw.(map[string]any)
	if !isObject {
		return nil, actionRefusal("validation.invalid_input", "Input must be an object of field values.", "")
	}
	return object, ""
}

// inputSchemaOf returns the input schema reference of one published operation of the catalog.
func inputSchemaOf(catalog contract.ApplicationCatalog, ref string) contract.SchemaRef {
	var schemaRef contract.SchemaRef
	for _, op := range catalog.PublishedOperations {
		if op.OperationRef == ref {
			schemaRef = op.InputSchema
		}
	}
	return schemaRef
}

// operationErrorText returns the model-facing error of a refused run: the structured OperationError
// when the module gave one, a generic refusal otherwise.
func operationErrorText(err error) string {
	var refusal OperationError
	if errors.As(err, &refusal) {
		return actionRefusal(refusal.Code, refusal.Message, refusal.Field)
	}
	return actionRefusal("module.operation_failed", "Pack action could not be completed.", "")
}

// receiptText returns the model-facing result of a run. A receipt with an error is prefixed "Error:".
func receiptText(ref string, receipt contract.OperationReceipt) string {
	result := map[string]any{"action": ref, "status": receipt.Status, "execution_id": receipt.ExecutionID}
	if len(receipt.Output) > 0 {
		result["output"] = receipt.Output
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "Error: pack action result could not be encoded."
	}
	if receipt.Error != nil {
		return "Error: " + string(data)
	}
	return string(data)
}

// findPackAction returns the action with the given id from a list.
func findPackAction(actions []packAction, ref string) (packAction, bool) {
	for _, action := range actions {
		if action.Action == ref {
			return action, true
		}
	}
	return packAction{}, false
}

// actionRefusal is the structured error a pack action returns to the model.
// actionRefusal returns the structured error of a refused pack action. The module's own code
// maps to the shared error codes; an unknown code is a guard failure.
func actionRefusal(code, message, field string) string {
	return aiToolError{Code: refusalCode(code), Message: message, Field: field}.text()
}

func refusalCode(code string) string {
	switch {
	case code == "unknown_action" || code == "module.operation_not_found":
		return errNotFound
	case code == "permission.denied":
		return errPermissionDenied
	case strings.HasPrefix(code, "validation."):
		return errInvalidValue
	}
	return errGuardFailed
}

// humanizeIdentifier turns an operation name such as record_sale into a title such as Record sale.
func humanizeIdentifier(name string) string {
	spaced := strings.ReplaceAll(name, "_", " ")
	if spaced == "" {
		return spaced
	}
	return strings.ToUpper(spaced[:1]) + spaced[1:]
}

// objectSchema is a closed object schema with the given properties and required names.
func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

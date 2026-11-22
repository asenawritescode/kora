package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// toolConfirmWrite is the tool that runs a previewed record write.
const toolConfirmWrite = "confirm_write"

// recordWrite is one kernel record command, as it is stored between preview and confirm.
type recordWrite struct {
	Command string         `json:"command"`
	Doctype string         `json:"doctype"`
	Name    string         `json:"name,omitempty"`
	Fields  map[string]any `json:"fields"`
}

// previewRecordWrite validates a create or update without writing it, keeps the write as
// previewed, and returns the preview with its confirm token.
func previewRecordWrite(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation, w recordWrite, rowScope policy.RowScope) string {
	dt := reg.Get(w.Doctype)
	if dt == nil {
		return (&aiToolError{Code: errNotFound, Message: "Unknown record type " + w.Doctype + "."}).text()
	}
	result, err := executeAIRecordMutation(tx, reg, recordMutation{Command: w.Command, Doctype: w.Doctype, Name: w.Name, Fields: w.Fields, Execution: inv.Execution, Preview: true})
	if err != nil {
		return recordWriteErrorText(err)
	}
	preview, aerr := previewBody(tx, dt, w, result, rowScope)
	if aerr != nil {
		return aerr.text()
	}
	return storePreview(tx, inv, w, preview)
}

// storePreview keeps one previewed write as previewed and returns the reply with its confirm token.
func storePreview(tx *orm.TxManager, inv toolInvocation, stored any, preview map[string]any) string {
	if inv.RunID == "" || inv.ConversationID == "" {
		return (&aiToolError{Code: errConfirmInvalid, Message: "A preview needs a conversation turn."}).text()
	}
	argsJSON, err := json.Marshal(stored)
	if err != nil {
		return (&aiToolError{Code: errInvalidValue, Message: "The write could not be stored."}).text()
	}
	previewJSON, err := json.Marshal(preview)
	if err != nil {
		return (&aiToolError{Code: errInvalidValue, Message: "The preview could not be encoded."}).text()
	}
	token, err := storePendingWrite(tx.Context, tx.DB, pendingPreview{
		Site: inv.Site, ConversationID: inv.ConversationID, RunID: inv.RunID, ActorID: inv.Owner,
		ToolName: inv.Name, ArgsJSON: string(argsJSON), PreviewJSON: string(previewJSON),
	})
	if err != nil {
		return (&aiToolError{Code: errConfirmInvalid, Message: "The preview could not be saved."}).text()
	}
	out, err := json.Marshal(previewReply{
		Preview:      preview,
		ConfirmToken: token,
		ExpiresIn:    pendingTTLSeconds,
		Next:         "Show the person this preview. After they say yes, call confirm_write with confirm_token. Do not call it in this reply.",
	})
	if err != nil {
		return (&aiToolError{Code: errInvalidValue, Message: "The preview could not be encoded."}).text()
	}
	return string(out)
}

// packActionWrite is one run_action call as it is stored between preview and confirm.
type packActionWrite struct {
	Action string         `json:"action"`
	Input  map[string]any `json:"input"`
}

// previewPackAction checks a run_action through the operation core, under the preview option.
func previewPackAction(tx *orm.TxManager, catalog contract.ApplicationCatalog, inv toolInvocation, action packAction, input map[string]any) string {
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return actionRefusal("validation.invalid_input", "Input must be an object of field values.", "")
	}
	if _, err := inv.Operations.Preview(ctx, inv.Execution, tx.BusinessScope, action.Action, contract.ExecutePublishedOperation{
		Meta:        contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(inv.IdempotencyKey)},
		InputSchema: inputSchemaOf(catalog, action.Action),
		Input:       encoded,
	}); err != nil {
		return operationErrorText(err)
	}
	preview := map[string]any{"action": action.Action, "title": action.Title, "input": input}
	return storePreview(tx, inv, packActionWrite{Action: action.Action, Input: input}, preview)
}

// executeConfirmPackAction runs a previewed run_action once, through the operation core.
func executeConfirmPackAction(tx *orm.TxManager, inv toolInvocation, row pendingWrite) string {
	var stored packActionWrite
	if err := json.Unmarshal([]byte(row.ArgsJSON), &stored); err != nil {
		return failPendingWrite(tx, row.ID, actionRefusal("module.operation_failed", "The stored action could not be read.", ""))
	}
	if inv.Operations == nil {
		return failPendingWrite(tx, row.ID, aiToolError{Code: errPermissionDenied, Message: "Pack actions are not available in this context."}.text())
	}
	catalog, err := inv.Operations.Catalog(inv.Execution, tx.BusinessScope)
	if err != nil {
		return failPendingWrite(tx, row.ID, aiToolError{Code: errGuardFailed, Message: "Pack actions are unavailable."}.text())
	}
	encoded, err := json.Marshal(stored.Input)
	if err != nil {
		return failPendingWrite(tx, row.ID, actionRefusal("validation.invalid_input", "Input must be an object of field values.", ""))
	}
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	receipt, err := inv.Operations.Run(ctx, inv.Execution, tx.BusinessScope, stored.Action, contract.ExecutePublishedOperation{
		Meta:        contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(confirmIdempotencyKey(row.ID))},
		InputSchema: inputSchemaOf(catalog, stored.Action),
		Input:       encoded,
	})
	if err != nil {
		return failPendingWrite(tx, row.ID, operationErrorText(err))
	}
	outcome := receiptText(stored.Action, receipt)
	if receipt.Error != nil {
		return failPendingWrite(tx, row.ID, outcome)
	}
	// The operation receipt is committed with the action. If this store fails, a later confirm
	// reclaims the row after its lease and replays that receipt.
	_ = finishPendingWrite(ctx, tx.DB, row.ID, pendingDone, outcome)
	return outcome
}

// previewReply is the reply to a previewed write. Its fields keep this order on the wire.
type previewReply struct {
	Preview      map[string]any `json:"preview"`
	ConfirmToken string         `json:"confirm_token"`
	ExpiresIn    int            `json:"expires_in"`
	Next         string         `json:"next"`
}

// previewBody is what the person sees: the saved values of a create, or the changed fields of an
// update as old and new values.
func previewBody(tx *orm.TxManager, dt *doctype.DocType, w recordWrite, result *kernel.ResultData, rowScope policy.RowScope) (map[string]any, *aiToolError) {
	saved := aiReadableDocumentFields(dt, result.Document)
	if w.Command != kernel.CommandRecordUpdate {
		return map[string]any{"action": "create", "record_type": dt.Name, "fields": saved}, nil
	}
	old, err := tx.GetDocWithScope(dt, w.Name, rowScope)
	if err != nil {
		return nil, &aiToolError{Code: errNotFound, Message: dt.Name + " " + w.Name + " was not found.", Field: "name"}
	}
	before := aiReadableDocumentFields(dt, old.Fields)
	changes := map[string]any{}
	for key, value := range saved {
		if oldValue, ok := before[key]; !ok || !reflect.DeepEqual(oldValue, value) {
			changes[key] = map[string]any{"old": before[key], "new": value}
		}
	}
	return map[string]any{"action": "update", "record_type": dt.Name, "name": w.Name, "changes": changes}, nil
}

// executeConfirmWrite runs the write a preview stored. The stored command runs with the
// preview's idempotency key, so a replayed confirm returns the first outcome.
func executeConfirmWrite(tx *orm.TxManager, reg *doctype.Registry, inv toolInvocation) string {
	token, _ := inv.Args["confirm_token"].(string)
	if strings.TrimSpace(token) == "" {
		return (&aiToolError{Code: errConfirmInvalid, Message: "confirm_token is required.", Field: "confirm_token", Fix: "Pass the confirm_token from the preview."}).text()
	}
	caller := pendingCaller{Site: inv.Site, ConversationID: inv.ConversationID, RunID: inv.RunID, ActorID: inv.Owner}
	row, aerr := claimPendingWrite(tx.Context, tx.DB, strings.TrimSpace(token), caller)
	if aerr != nil {
		return aerr.text()
	}
	if row.State == pendingDone {
		return row.Outcome
	}
	var w recordWrite
	if err := json.Unmarshal([]byte(row.ArgsJSON), &w); err != nil {
		return failPendingWrite(tx, row.ID, "The stored write could not be read.")
	}
	if row.ToolName == toolRunAction {
		return executeConfirmPackAction(tx, inv, row)
	}
	result, err := executeAIRecordMutation(tx, reg, recordMutation{Command: w.Command, Doctype: w.Doctype, Name: w.Name, Fields: w.Fields, IdempotencyKey: confirmIdempotencyKey(row.ID), Execution: inv.Execution})
	if err != nil {
		return failPendingWrite(tx, row.ID, recordWriteErrorText(err))
	}
	outcome := confirmedOutcome(w, result)
	// The kernel receipt is committed with the write. If this store fails, a later confirm
	// reclaims the row after its lease and replays that receipt, so the outcome is not lost.
	_ = finishPendingWrite(tx.Context, tx.DB, row.ID, pendingDone, outcome)
	return outcome
}

// failPendingWrite stores a refused write as failed and returns its structured error text.
func failPendingWrite(tx *orm.TxManager, id, text string) string {
	// A stale finish is not expected here: the row was claimed by this call.
	_ = finishPendingWrite(tx.Context, tx.DB, id, pendingFailed, text)
	return text
}

func confirmedOutcome(w recordWrite, result *kernel.ResultData) string {
	if w.Command == kernel.CommandRecordUpdate {
		return fmt.Sprintf("Updated %s %q.", w.Doctype, result.Name)
	}
	return fmt.Sprintf("Created %s %q.", w.Doctype, result.Name)
}

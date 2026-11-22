package ai

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

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
	if inv.RunID == "" || inv.ConversationID == "" {
		return (&aiToolError{Code: errConfirmInvalid, Message: "A preview needs a conversation turn."}).text()
	}
	result, err := executeAIRecordMutation(tx, reg, recordMutation{Command: w.Command, Doctype: w.Doctype, Name: w.Name, Fields: w.Fields, Execution: inv.Execution, Preview: true})
	if err != nil {
		return recordWriteErrorText(err)
	}
	preview, aerr := previewBody(tx, dt, w, result, rowScope)
	if aerr != nil {
		return aerr.text()
	}
	argsJSON, err := json.Marshal(w)
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

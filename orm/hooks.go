package orm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/script"
)

// AsyncHookRequest is a deferred hook execution.
type AsyncHookRequest struct {
	DT       *doctype.DocType
	Event    script.Event
	Doc      *doctype.Document
	OldDoc   *doctype.Document
	Rec      script.ScriptRecord
	User     string
	UserRole string
	Site     string
}

// setupComputedHook sets the computed script hook before ComputeFields runs.
func (tx *TxManager) setupComputedHook() {
	if tx.ScriptRunner == nil || tx.ScriptStore == nil {
		doctype.SetComputedScriptHook(nil)
		return
	}
	doctype.SetComputedScriptHook(func(doctypeName, scriptName string, doc *doctype.Document) (any, error) {
		scripts, err := tx.ScriptStore.LoadActiveScripts(tx.SiteName, doctypeName, script.EventComputed)
		if err != nil {
			return nil, err
		}
		for _, rec := range scripts {
			if rec.Name == scriptName || rec.WorkflowAction == scriptName {
				req := script.ExecuteRequest{
					Script: rec.Script, ScriptType: rec.ScriptType, ScriptName: rec.Name,
					DocType: doctypeName, Event: script.EventComputed,
					Document: doc.Fields, User: tx.CurrentUser,
					UserRoles: []string{tx.CurrentUserRole}, Site: tx.SiteName,
					Provider: tx.ScriptProvider,
				}
				cctx := tx.Context
				if cctx == nil {
					cctx = context.Background()
				}
				result, err := tx.ScriptRunner.Execute(cctx, req)
				if err != nil {
					return nil, err
				}
				// The script's return value is the computed field value.
				if result != nil && result.Result != nil {
					return result.Result, nil
				}
				return nil, nil
			}
		}
		return nil, fmt.Errorf("computed script %q not found", scriptName)
	})
}

// RunHooksForValidate executes validate hooks from the API layer.
// This is a public entry point so API handlers can trigger validation scripts.
func (tx *TxManager) RunHooksForValidate(dt *doctype.DocType, doc *doctype.Document, oldDoc *doctype.Document) error {
	return tx.runHooks(dt, script.EventValidate, doc, oldDoc)
}

// runHooks executes all active scripts for a given doctype + event.
// For before_* hooks, the modified document is returned (scripts can modify it).
// For after_* hooks, errors are logged but not returned (best-effort).
func (tx *TxManager) runHooks(dt *doctype.DocType, event script.Event, doc *doctype.Document, oldDoc *doctype.Document) error {
	if tx.ScriptRunner == nil || tx.ScriptStore == nil {
		slog.Warn("runHooks: runner or store nil", "runner", tx.ScriptRunner != nil, "store", tx.ScriptStore != nil, "site", tx.SiteName, "doctype", dt.Name, "event", event)
		return nil
	}

	scripts, err := tx.ScriptStore.LoadActiveScripts(tx.SiteName, dt.Name, event)
	if err != nil {
		return fmt.Errorf("loading scripts: %w", err)
	}
	if len(scripts) == 0 {
		slog.Info("runHooks: no scripts matched", "site", tx.SiteName, "doctype", dt.Name, "event", event)
		return nil
	}
	slog.Info("runHooks: executing scripts", "site", tx.SiteName, "doctype", dt.Name, "event", event, "count", len(scripts))

	userRoles := []string{tx.CurrentUserRole}
	if tx.CurrentUserRole == "" {
		userRoles = []string{doctype.AdminRole}
	}

	var oldDocMap map[string]any
	if oldDoc != nil {
		oldDocMap = oldDoc.Fields
	}

	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}

	for _, rec := range scripts {
		// Route after_* events to the async queue if available.
		if script.IsAfterEvent(event) && tx.AsyncHookQueue != nil {
			select {
			case tx.AsyncHookQueue <- AsyncHookRequest{
				DT: dt, Event: event, Doc: doc, OldDoc: oldDoc, Rec: rec,
				User: tx.CurrentUser, UserRole: tx.CurrentUserRole, Site: tx.SiteName,
			}:
			default:
				slog.Warn("async hook queue full, dropping hook", "script", rec.Name, "event", event)
			}
			continue
		}

		req := script.ExecuteRequest{
			Script:      rec.Script,
			ScriptType:  rec.ScriptType,
			ScriptName:  rec.Name,
			DocType:     dt.Name,
			Event:       event,
			Document:    doc.Fields,
			OldDocument: oldDocMap,
			User:        tx.CurrentUser,
			UserRoles:   userRoles,
			Site:        tx.SiteName,
			Provider:    tx.ScriptProvider,
		}

		// Execute with panic recovery.
		var result *script.ExecuteResult
		var execErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					execErr = fmt.Errorf("script panic: %v", r)
					slog.Error("script execution panicked", "script", rec.Name, "event", event, "panic", r)
				}
			}()
			result, execErr = tx.ScriptRunner.Execute(ctx, req)
		}()

		durationMs := 0
		status := "success"
		var errMsg string

		if execErr != nil {
			status = "error"
			errMsg = execErr.Error()
			if script.IsBeforeEvent(event) {
				// Before hooks can reject — abort the operation.
				if result != nil {
					durationMs = int(result.Duration.Milliseconds())
				}
				tx.ScriptStore.LogExecution(tx.SiteName, rec, dt.Name, doc.Name, event, tx.CurrentUser, durationMs, status, errMsg)
				return fmt.Errorf("script %q (%s): %w", rec.Name, event, execErr)
			}
			// After hooks are best-effort — log and continue.
			slog.Warn("script after-hook failed", "script", rec.Name, "doctype", dt.Name, "event", event, "error", execErr)
		}

		if result != nil {
			durationMs = int(result.Duration.Milliseconds())
			if result.Modified && script.IsBeforeEvent(event) {
				// Apply modified document from before_* hook.
				doc.Fields = result.Document
				req.Document = result.Document // update for subsequent scripts
			}
		}

		// Log execution regardless of outcome.
		if tx.ScriptStore != nil {
			_ = tx.ScriptStore.LogExecution(tx.SiteName, rec, dt.Name, doc.Name, event, tx.CurrentUser, durationMs, status, errMsg)
		}
	}
	return nil
}

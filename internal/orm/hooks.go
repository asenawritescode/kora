package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

// hookEnqueueFailed counts async after_* hooks that could not be handed to the
// worker sink. Phase 1 requires async work to fail observably.
var hookEnqueueFailed atomic.Int64

const maxModuleValidationExtensionTime = 2 * time.Second

// HookEnqueueFailedCount returns the number of async hooks that could not be
// handed to the worker sink since process start.
func HookEnqueueFailedCount() int64 { return hookEnqueueFailed.Load() }

// RecordHookEnqueueFailure increments the process-level count when a supported
// asynchronous hook invocation could not be queued.
func RecordHookEnqueueFailure() int64 { return hookEnqueueFailed.Add(1) }

// AsyncHookRequest is a process-local deferred hook execution. The verified
// execution is deliberately not JSON-serializable; a future durable or remote
// worker must establish identity through an explicit service contract.
type AsyncHookRequest struct {
	Doctype   string // doctype name, re-hydrated via the registry
	Event     script.Event
	Doc       map[string]any // serializable current document (may include child rows)
	OldDoc    map[string]any // serializable previous document (nil for inserts)
	Rec       script.Record
	User      string
	UserRole  string
	UserRoles []string
	Site      string
	Execution policy.Execution `json:"-"`
	// DisableProvider preserves workflow-action scripts' synchronous execution
	// contract when they are submitted to the shared async worker.
	DisableProvider bool
	// SkipHookScripts prevents an async after-hook from recursively invoking
	// itself when its provider performs a nested document mutation.
	SkipHookScripts []string
}

// AsyncHookSink hands deferred after_* hooks to the background worker system.
type AsyncHookSink interface {
	Enqueue(context.Context, AsyncHookRequest) error
}

// computedScriptHook returns an operation-scoped evaluator for script-backed
// computed fields. It never mutates package-global state.
func (tx *TxManager) computedScriptHook() doctype.ComputedScriptHook {
	if tx.ScriptRunner == nil || tx.ScriptStore == nil {
		return nil
	}
	return func(doctypeName, scriptName string, doc *doctype.Document) (any, error) {
		scripts, err := tx.activeHookScripts(doctypeName)
		if err != nil {
			return nil, err
		}
		for _, rec := range scripts {
			if rec.Event != script.EventComputed {
				continue
			}
			if containsHookScript(tx.SkipHookScripts, rec.Name) {
				continue
			}
			if rec.Name == scriptName {
				provider := scopedHookProvider(tx, tx.ScriptProvider, doctypeName, script.EventComputed, rec.Name)
				userRoles := append([]string(nil), tx.CurrentUserRoles...)
				if len(userRoles) == 0 && tx.CurrentUserRole != "" {
					userRoles = []string{tx.CurrentUserRole}
				}
				req := script.ExecuteRequest{
					Script: rec.Script, ScriptType: rec.ScriptType, ScriptName: rec.Name,
					DocType: doctypeName, Event: script.EventComputed,
					Document: doc.ToMap(), User: tx.CurrentUser,
					UserRoles: userRoles, Site: tx.SiteName,
					Provider: provider,
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
	}
}

// RunHooksForValidate executes validate hooks from the API layer.
// This is a public entry point so API handlers can trigger validation scripts.
func (tx *TxManager) RunHooksForValidate(dt *doctype.DocType, doc *doctype.Document, oldDoc *doctype.Document) error {
	return tx.runHooks(dt, script.EventValidate, doc, oldDoc)
}

// CaptureAsyncAfterHooksTx freezes the selected lifecycle scripts, document
// values, and verified execution identity into the caller's business
// transaction. When durable storage is configured, callers must invoke this
// before committing their record mutation.
func (tx *TxManager) CaptureAsyncAfterHooksTx(ctx context.Context, dbTx *sql.Tx, dt *doctype.DocType, event script.Event, doc, oldDoc *doctype.Document) error {
	if tx == nil {
		return nil
	}
	extensions := tx.lifecycleExtensions(dt, event)
	if tx.AsyncHookInvocations == nil {
		if len(extensions) > 0 {
			return fmt.Errorf("module post-commit extensions require durable lifecycle-hook storage")
		}
		return nil
	}
	if ctx == nil || dbTx == nil || dt == nil || doc == nil || tx.ScriptStore == nil || !tx.Execution.Valid() || tx.AsyncHookTenantID == "" || tx.AsyncHookSourceOperation == "" || tx.AsyncHookConfigRevision == 0 {
		return fmt.Errorf("durable async hook capture requires transaction, script store and verified operation identity")
	}
	if !script.IsAfterEvent(event) {
		return fmt.Errorf("durable async hook capture does not accept event %q", event)
	}
	scripts, err := tx.activeHookScripts(dt.Name)
	if err != nil {
		return fmt.Errorf("load lifecycle scripts in source transaction: %w", err)
	}
	scripts, err = tx.selectLifecycleScripts(dt, event, scripts)
	if err != nil {
		return fmt.Errorf("resolve registered module post-commit hooks: %w", err)
	}
	current, err := json.Marshal(doc.ToMap())
	if err != nil {
		return fmt.Errorf("encode lifecycle document snapshot: %w", err)
	}
	var previous json.RawMessage
	if oldDoc != nil {
		previous, err = json.Marshal(oldDoc.ToMap())
		if err != nil {
			return fmt.Errorf("encode previous lifecycle document snapshot: %w", err)
		}
	}
	actor := tx.Execution.Actor()
	for _, record := range scripts {
		if record.Event != event || containsHookScript(tx.SkipHookScripts, record.Name) {
			continue
		}
		tx.AsyncHookSequence++
		if tx.AsyncHookSequence == 0 {
			return fmt.Errorf("too many async lifecycle invocations in one source operation")
		}
		invocation := script.AsyncHookInvocation{
			TenantID: tx.AsyncHookTenantID, Site: tx.SiteName,
			SourceOperation: tx.AsyncHookSourceOperation,
			ConfigRevision:  tx.AsyncHookConfigRevision, Sequence: tx.AsyncHookSequence,
			Doctype: dt.Name, Event: event, Record: record,
			Document:    append(json.RawMessage(nil), current...),
			OldDocument: append(json.RawMessage(nil), previous...),
			Actor:       actor, Grants: tx.Execution.Grants(),
		}
		if err := tx.AsyncHookInvocations.AppendAsyncHookTx(ctx, dbTx, invocation); err != nil {
			return fmt.Errorf("persist async lifecycle hook %q: %w", record.Name, err)
		}
	}
	// Do not reload after commit: a newly activated script must not receive an
	// old event whose source transaction committed before that activation.
	tx.SuppressAsyncHookEvents = true
	return nil
}

// runHooks executes all active scripts for a given doctype + event.
// For before_* hooks, the modified document is returned (scripts can modify it).
// For after_* hooks, errors are logged but not returned (best-effort).
func (tx *TxManager) runHooks(dt *doctype.DocType, event script.Event, doc *doctype.Document, oldDoc *doctype.Document) error {
	extensions := tx.lifecycleExtensions(dt, event)
	if len(extensions) > 0 && script.IsAfterEvent(event) && !tx.SuppressAsyncHookEvents {
		return fmt.Errorf("module post-commit extension handlers must be captured in the source transaction")
	}
	if tx.ScriptRunner == nil || tx.ScriptStore == nil {
		if len(extensions) > 0 && !(script.IsAfterEvent(event) && tx.SuppressAsyncHookEvents) {
			return fmt.Errorf("module lifecycle extensions require the tenant script runtime")
		}
		if tx.ScriptRunner != nil || tx.ScriptStore != nil {
			slog.Warn("runHooks: runner or store nil", "runner", tx.ScriptRunner != nil, "store", tx.ScriptStore != nil, "site", tx.SiteName, "doctype", dt.Name, "event", event)
		}
		return nil
	}

	scripts, err := tx.activeHookScripts(dt.Name)
	if err != nil {
		return fmt.Errorf("loading scripts: %w", err)
	}
	scripts, err = tx.selectLifecycleScripts(dt, event, scripts)
	if err != nil {
		return fmt.Errorf("resolve registered module lifecycle hooks: %w", err)
	}
	matching := make([]script.Record, 0, len(scripts))
	for _, record := range scripts {
		if record.Event == event {
			matching = append(matching, record)
		}
	}
	scripts = matching
	if len(scripts) == 0 {
		slog.Debug("runHooks: no scripts matched", "site", tx.SiteName, "doctype", dt.Name, "event", event)
		return nil
	}
	slog.Debug("runHooks: executing scripts", "site", tx.SiteName, "doctype", dt.Name, "event", event, "count", len(scripts))

	userRoles := append([]string(nil), tx.CurrentUserRoles...)
	if len(userRoles) == 0 && tx.CurrentUserRole != "" {
		userRoles = []string{tx.CurrentUserRole}
	}
	if len(userRoles) == 0 {
		userRoles = []string{doctype.AdminRole}
	}

	var oldDocMap map[string]any
	if oldDoc != nil {
		oldDocMap = oldDoc.ToMap()
	}

	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}

	for _, rec := range scripts {
		if containsHookScript(tx.SkipHookScripts, rec.Name) {
			continue
		}
		// Route after_* events to the async sink if available.
		if script.IsAfterEvent(event) && tx.SuppressAsyncHookEvents {
			continue
		}
		if script.IsAfterEvent(event) && tx.AsyncHookSink != nil {
			var docMap, oldDocMap map[string]any
			docMap = doc.ToMap()
			if oldDoc != nil {
				oldDocMap = oldDoc.ToMap()
			}
			hookRoles := append([]string(nil), userRoles...)
			if tx.Execution.Valid() {
				hookRoles = append([]string(nil), tx.Execution.Actor().Roles...)
			}
			if err := tx.AsyncHookSink.Enqueue(ctx, AsyncHookRequest{
				Doctype: dt.Name, Event: event, Doc: docMap, OldDoc: oldDocMap, Rec: rec,
				User: tx.CurrentUser, UserRole: tx.CurrentUserRole, UserRoles: hookRoles, Site: tx.SiteName,
				Execution:       tx.Execution,
				SkipHookScripts: append(append([]string(nil), tx.SkipHookScripts...), rec.Name),
			}); err != nil {
				hookEnqueueFailed.Add(1)
				slog.Warn("async hook enqueue failed", "script", rec.Name, "event", event, "failed_total", hookEnqueueFailed.Load(), "error", err)
			}
			continue
		}

		moduleExtension, isModuleExtension := tx.extensionForHandler(dt, event, rec.Name)
		isValidationExtension := isModuleExtension && moduleExtension.Registration.Kind == contract.ExtensionValidation
		if isValidationExtension {
			if tx.ModuleExtensionSchemas == nil {
				return fmt.Errorf("module validation extension %q has no schema registry", rec.Name)
			}
			input, marshalErr := json.Marshal(doc.ToMap())
			if marshalErr != nil {
				return fmt.Errorf("encode module validation extension input: %w", marshalErr)
			}
			if validateErr := tx.ModuleExtensionSchemas.Validate(moduleExtension.Point.InputSchema, input); validateErr != nil {
				return fmt.Errorf("module validation extension %q input rejected: %w", rec.Name, validateErr)
			}
			if rec.TimeoutMs <= 0 || time.Duration(rec.TimeoutMs)*time.Millisecond > maxModuleValidationExtensionTime {
				return fmt.Errorf("module validation extension %q timeout must be between 1ms and %s", rec.Name, maxModuleValidationExtensionTime)
			}
		}
		provider := scopedHookProvider(tx, tx.ScriptProvider, dt.Name, event, rec.Name)
		if isValidationExtension {
			provider = nil // Validators receive no document, secret, HTTP, or mutation bridge.
		}
		req := script.ExecuteRequest{
			Script:      rec.Script,
			ScriptType:  rec.ScriptType,
			ScriptName:  rec.Name,
			DocType:     dt.Name,
			Event:       event,
			Document:    doc.ToMap(),
			OldDocument: oldDocMap,
			User:        tx.CurrentUser,
			UserRoles:   userRoles,
			Site:        tx.SiteName,
			Provider:    provider,
		}
		if isValidationExtension {
			req.Timeout = time.Duration(rec.TimeoutMs) * time.Millisecond
			req.Deterministic = true
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
		if isValidationExtension && execErr == nil {
			if result == nil || result.Modified || result.Result == nil {
				execErr = fmt.Errorf("module validation extension must return a schema-valid result without modifying the document")
			} else if output, marshalErr := json.Marshal(result.Result); marshalErr != nil {
				execErr = fmt.Errorf("encode module validation extension result: %w", marshalErr)
			} else if validateErr := tx.ModuleExtensionSchemas.Validate(moduleExtension.Point.OutputSchema, output); validateErr != nil {
				execErr = fmt.Errorf("module validation extension result rejected: %w", validateErr)
			}
		}

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
				// Logging must not mask the rejection that aborts the operation.
				_ = tx.ScriptStore.LogExecution(tx.SiteName, rec, dt.Name, doc.Name, event, tx.CurrentUser, durationMs, status, errMsg)
				return fmt.Errorf("script %q (%s): %w", rec.Name, event, execErr)
			}
			// After hooks are best-effort — log and continue.
			slog.Warn("script after-hook failed", "script", rec.Name, "doctype", dt.Name, "event", event, "error", execErr)
		}

		if result != nil {
			durationMs = int(result.Duration.Milliseconds())
			if result.Modified && script.IsBeforeEvent(event) {
				// Apply modified document from before_* hook and restore typed child tables.
				doc.Fields = normalizeHookDocumentFields(dt, tx.Registry, result.Document)
				req.Document = doc.ToMap() // update for subsequent scripts
			}
		}

		// Log execution regardless of outcome.
		if tx.ScriptStore != nil {
			_ = tx.ScriptStore.LogExecution(tx.SiteName, rec, dt.Name, doc.Name, event, tx.CurrentUser, durationMs, status, errMsg)
		}
	}
	return nil
}

// LoadLifecycleHooksTx loads a command's lifecycle handlers from the command's
// transaction snapshot. Kernel uses this before validation and mutation so
// module extension handlers are present and their activation state cannot race
// with the business write.
func (tx *TxManager) LoadLifecycleHooksTx(ctx context.Context, dbTx *sql.Tx, dt *doctype.DocType) error {
	if tx == nil || tx.ScriptStore == nil || dbTx == nil || dt == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	scripts, err := tx.ScriptStore.LoadActiveScriptsTx(ctx, dbTx, tx.SiteName, dt.Name, "")
	if err != nil {
		return fmt.Errorf("loading lifecycle scripts in command transaction: %w", err)
	}
	if tx.HookScriptsByDoctype == nil {
		tx.HookScriptsByDoctype = make(map[string][]script.Record)
	}
	tx.HookScriptsByDoctype[dt.Name] = scripts
	return nil
}

func (tx *TxManager) lifecycleExtensions(dt *doctype.DocType, event script.Event) []contract.ResolvedModuleExtension {
	if tx == nil || dt == nil {
		return nil
	}
	var matching []contract.ResolvedModuleExtension
	for _, extension := range tx.ModuleExtensions {
		if extension.Point.Target.Name != dt.Name {
			continue
		}
		registration := extension.Registration
		switch registration.Kind {
		case contract.ExtensionValidation:
			if event == script.EventValidate {
				matching = append(matching, extension)
			}
		case contract.ExtensionPostCommit:
			if registration.Event == string(event) {
				matching = append(matching, extension)
			}
		}
	}
	return matching
}

func (tx *TxManager) extensionForHandler(dt *doctype.DocType, event script.Event, name string) (contract.ResolvedModuleExtension, bool) {
	for _, extension := range tx.lifecycleExtensions(dt, event) {
		if extension.Registration.HandlerRef.Name == name {
			return extension, true
		}
	}
	return contract.ResolvedModuleExtension{}, false
}

// selectLifecycleScripts keeps module-owned hook resources inert unless the
// current activation snapshot explicitly registers them at an owner point.
// Registered extensions follow the precomputed owner order; ordinary scripts
// keep the existing ScriptStore order ahead of extension handlers.
func (tx *TxManager) selectLifecycleScripts(dt *doctype.DocType, event script.Event, records []script.Record) ([]script.Record, error) {
	if tx == nil || dt == nil {
		return nil, fmt.Errorf("module lifecycle hook selection requires a document type")
	}
	ownedNames := make(map[string]bool)
	for _, module := range tx.ModuleDescriptors {
		for _, resource := range module.Resources {
			ownedNames[resource.Ref.Name] = true
		}
	}
	active := make(map[string]script.Record, len(records))
	for _, record := range records {
		if record.Event != event || record.DocType != "" && record.DocType != dt.Name {
			continue
		}
		if _, duplicate := active[record.Name]; duplicate {
			return nil, fmt.Errorf("module hook resource %q is ambiguous", record.Name)
		}
		active[record.Name] = record
	}
	var ordinary []script.Record
	for _, record := range records {
		if record.Event != event || record.DocType != "" && record.DocType != dt.Name || ownedNames[record.Name] {
			continue
		}
		ordinary = append(ordinary, record)
	}
	registered := tx.lifecycleExtensions(dt, event)
	result := append([]script.Record(nil), ordinary...)
	for _, extension := range registered {
		name := extension.Registration.HandlerRef.Name
		record, ok := active[name]
		if !ok || record.DocType != dt.Name || record.ScriptType != script.TypeDocEvent || !record.IsActive || record.Script == "" {
			return nil, fmt.Errorf("registered module extension handler %q is missing or inactive", name)
		}
		result = append(result, record)
	}
	return result, nil
}

func (tx *TxManager) activeHookScripts(doctypeName string) ([]script.Record, error) {
	if tx.HookScriptsByDoctype == nil {
		tx.HookScriptsByDoctype = make(map[string][]script.Record)
	}
	if scripts, loaded := tx.HookScriptsByDoctype[doctypeName]; loaded {
		return scripts, nil
	}
	var scripts []script.Record
	var err error
	if tx.Transaction != nil {
		ctx := tx.Context
		if ctx == nil {
			ctx = context.Background()
		}
		scripts, err = tx.ScriptStore.LoadActiveScriptsTx(ctx, tx.Transaction, tx.SiteName, doctypeName, "")
	} else {
		scripts, err = tx.ScriptStore.LoadActiveScripts(tx.SiteName, doctypeName, "")
	}
	if err != nil {
		return nil, fmt.Errorf("loading scripts: %w", err)
	}
	tx.HookScriptsByDoctype[doctypeName] = scripts
	return scripts, nil
}

func scopedHookProvider(tx *TxManager, provider script.KoraProvider, doctypeName string, event script.Event, scriptName string) script.KoraProvider {
	scoped, ok := provider.(script.LifecycleScopedProvider)
	if ok {
		provider = scoped.WithLifecycleHook(doctypeName, event, scriptName)
	}
	if txScoped, ok := provider.(script.MutationScopedProvider); ok && tx.ScriptMutationExecutor != nil {
		provider = txScoped.WithMutationExecutor(tx.ScriptMutationExecutor)
	}
	return provider
}

func containsHookScript(active []string, scriptName string) bool {
	for _, activeName := range active {
		if activeName == scriptName {
			return true
		}
	}
	return false
}

func normalizeHookDocumentFields(dt *doctype.DocType, registry *doctype.Registry, fields map[string]any) map[string]any {
	if dt == nil || fields == nil {
		return fields
	}
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	for _, field := range dt.TableFields() {
		value, ok := out[field.Fieldname]
		if !ok || value == nil {
			continue
		}
		childDT := registry.Get(field.Options)
		if childDT == nil {
			continue
		}
		if children, ok := hookValueToChildDocuments(value, childDT); ok {
			out[field.Fieldname] = children
		}
	}
	delete(out, "name")
	delete(out, "doc_status")
	delete(out, "owner")
	delete(out, "creation")
	delete(out, "modified")
	delete(out, "modified_by")
	return out
}

// DocumentFromMap reconstructs a *doctype.Document (including child tables) from a
// serialized map produced by Document.ToMap. It is used by async hook workers to
// re-hydrate an AsyncHookRequest DTO. Returns nil if m is nil.
func DocumentFromMap(reg *doctype.Registry, doctypeName string, m map[string]any) *doctype.Document {
	if m == nil {
		return nil
	}
	dt := reg.Get(doctypeName)
	doc := doctype.NewDocument(doctypeName)
	if name, ok := m["name"].(string); ok {
		doc.Name = name
		doc.IsNew = name == ""
	}
	if status, ok := m["doc_status"]; ok {
		doc.DocStatus = hookIntValue(status)
	}
	for key, value := range m {
		if key == "name" || key == "doc_status" || key == "owner" || key == "creation" || key == "modified" || key == "modified_by" {
			continue
		}
		// Rebuild child tables as typed []*doctype.Document where the doctype is known.
		if dt != nil {
			if field := findTableField(dt, key); field != nil {
				if childDT := reg.Get(field.Options); childDT != nil {
					if children, ok := hookValueToChildDocuments(value, childDT); ok {
						doc.Set(key, children)
						continue
					}
				}
			}
		}
		doc.Set(key, value)
	}
	return doc
}

// findTableField returns the Table-type field with the given fieldname, or nil.
func findTableField(dt *doctype.DocType, fieldname string) *doctype.Field {
	for i := range dt.Fields {
		if dt.Fields[i].Fieldname == fieldname && dt.Fields[i].Fieldtype == "Table" {
			return &dt.Fields[i]
		}
	}
	return nil
}

func hookValueToChildDocuments(value any, childDT *doctype.DocType) ([]*doctype.Document, bool) {
	switch rows := value.(type) {
	case []*doctype.Document:
		return rows, true
	case []map[string]any:
		children := make([]*doctype.Document, 0, len(rows))
		for _, row := range rows {
			children = append(children, hookMapToChildDocument(row, childDT))
		}
		return children, true
	case []any:
		children := make([]*doctype.Document, 0, len(rows))
		for _, item := range rows {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			children = append(children, hookMapToChildDocument(row, childDT))
		}
		return children, true
	default:
		return nil, false
	}
}

func hookMapToChildDocument(row map[string]any, childDT *doctype.DocType) *doctype.Document {
	child := doctype.NewDocument(childDT.Name)
	if name, ok := row["name"].(string); ok {
		child.Name = name
		child.IsNew = name == ""
	}
	if status, ok := row["doc_status"]; ok {
		child.DocStatus = hookIntValue(status)
	}
	for key, value := range row {
		if key == "name" || key == "doc_status" || key == "owner" || key == "creation" || key == "modified" || key == "modified_by" {
			continue
		}
		child.Set(key, value)
	}
	return child
}

func hookIntValue(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		f, _ := v.Float64()
		return int(f)
	case float64:
		return int(v)
	default:
		return 0
	}
}

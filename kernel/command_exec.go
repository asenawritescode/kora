package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
)

// authorizeOp dispatches authorization: built-in commands authorize against
// the payload's doctype; config-defined commands authorize EVERY touched
// record at per-step least privilege — create steps need "create", update
// steps need "write" on their target record (default-deny, spec §51/§108).
func authorizeOp(reg *doctype.Registry, op Operation, def CommandDefinition, dyn *CommandResource) *contract.Error {
	if dyn != nil && dyn.CrossEntity != nil {
		if op.Context.crossEntityOperation == nil {
			return contract.NewError(contract.CodePermissionDenied, "cross-entity command requires verified source and target scopes")
		}
		if authErr := policy.AuthorizeCrossEntityOperation(op.Context.Execution, *op.Context.crossEntityOperation, reg); authErr != nil {
			return authErr
		}
	}
	if dyn != nil && dynamicCommandTouchesCommandOnly(reg, dyn) {
		if !validDynamicModuleExecution(op.Context.ModuleExecution, op, reg, dyn) {
			return contract.NewError(contract.CodePermissionDenied, "protected resources require their owning module command")
		}
	}
	if mutationTouchesCommandOnly(reg, op) {
		execution := op.Context.ModuleExecution
		if execution == nil || execution.AuthorizedCommand == "" || execution.AuthorizedCommand == CommandRecordCreate || execution.AuthorizedCommand == CommandRecordUpdate || execution.AuthorizedCommand == CommandRecordMutateBundle || execution.AuthorizedCommand == CommandRecordDelete {
			return contract.NewError(contract.CodePermissionDenied, "resource is owned by a protected module and requires its domain command")
		}
		if !validModuleExecution(execution, op, reg) {
			return contract.NewError(contract.CodePermissionDenied, "module execution is not authorized for this domain command")
		}
	}
	if op.Command == CommandPublicFormSubmit {
		return authorizePublicForm(reg, op)
	}
	if op.Command == CommandRecordMutateBundle {
		if reg == nil {
			return ErrPermission
		}
		var payload RecordMutationBundlePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Doctype == "" || len(payload.Records) == 0 || len(payload.Records) > MaxMutationBundleRecords {
			return contract.NewError(contract.CodeValidationFailed, "record.mutate_bundle requires 1 to 50 records")
		}
		for _, record := range payload.Records {
			if record.Doctype == "" {
				return contract.NewError(contract.CodeValidationFailed, "bundle record doctype is required")
			}
			dt := reg.Get(record.Doctype)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", record.Doctype))
			}
			if dt.IsChildTable {
				return contract.NewError(contract.CodeValidationFailed, "child doctypes cannot be standalone bundle records")
			}
			permission := record.Operation
			if permission == "" {
				permission = "create"
			}
			if permission != "create" && permission != "update" && permission != "decrement" && permission != "adjust" {
				return contract.NewError(contract.CodeValidationFailed, "bundle operation must be create, update, decrement, or adjust")
			}
			if permission != "create" && record.Name == "" {
				return contract.NewError(contract.CodeValidationFailed, "bundle update requires a record name")
			}
			if dt.AppendOnly && permission != "create" {
				return contract.NewError(contract.CodeValidationFailed, "append-only records cannot be changed")
			}
			if permission != "create" {
				permission = "write"
			}
			if _, err := decideOperationResource(op, reg, dt, permission); err != nil {
				return err
			}
		}
		root := reg.Get(payload.Doctype)
		if root == nil || root.Name != reg.Get(payload.Records[0].Doctype).Name {
			return contract.NewError(contract.CodeValidationFailed, "bundle root doctype must match the first record")
		}
		return nil
	}
	if dyn == nil {
		return authorize(reg, op, def)
	}
	if reg == nil {
		return ErrPermission
	}
	for _, step := range dyn.Steps {
		stepOp, scopeErr := operationForCommandStep(op, dyn, step)
		if scopeErr != nil {
			return scopeErr
		}
		switch {
		case step.Create != nil:
			dt := reg.Get(step.Create.Record)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", step.Create.Record))
			}
			if _, err := decideOperationResource(stepOp, reg, dt, "create"); err != nil {
				return err
			}
		case step.Update != nil:
			dt := reg.Get(step.Update.Record)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", step.Update.Record))
			}
			if dt.AppendOnly {
				return contract.NewError(contract.CodeValidationFailed, "append-only records cannot be changed")
			}
			if _, err := decideOperationResource(stepOp, reg, dt, "write"); err != nil {
				return err
			}
		}
	}
	return nil
}

func dynamicCommandTouchesCommandOnly(reg *doctype.Registry, command *CommandResource) bool {
	if reg == nil || command == nil {
		return false
	}
	for _, name := range command.TouchedRecords() {
		dt := reg.Get(name)
		if dt != nil && dt.ModuleOwnershipMode == string(contract.MutationCommands) {
			return true
		}
	}
	return false
}

func validDynamicModuleExecution(execution *contract.ModuleExecution, op Operation, reg *doctype.Registry, command *CommandResource) bool {
	if execution == nil || !execution.Verified() || execution.TenantID != op.Context.TenantID || execution.Scope != op.Context.BusinessScope || execution.AuthorizedCommand != op.Command || execution.AllowedOperation != op.Command || command == nil || command.FullName() != op.Command || command.ModuleOwnerID == "" || command.Capability == "" || command.Operation == "" || command.InputSchema.ID == "" || command.InputSchema.Version == 0 || command.OutputSchema.ID == "" || command.OutputSchema.Version == 0 || reg == nil {
		return false
	}
	protected := false
	for _, name := range command.TouchedRecords() {
		dt := reg.Get(name)
		if dt == nil {
			return false
		}
		if dt.ModuleOwnershipMode == string(contract.MutationCommands) {
			protected = true
			if dt.ModuleOwnerID == "" || execution.OwnerModuleID != contract.ModuleID(dt.ModuleOwnerID) || command.ModuleOwnerID != execution.OwnerModuleID {
				return false
			}
		}
	}
	return protected
}

func validModuleExecution(execution *contract.ModuleExecution, op Operation, reg *doctype.Registry) bool {
	if execution == nil || !execution.Verified() || execution.TenantID != op.Context.TenantID || execution.Scope != op.Context.BusinessScope {
		return false
	}
	if execution.AuthorizedCommand == "" || execution.AuthorizedCommand != op.Command || execution.AllowedOperation != op.Command {
		return false
	}
	dt := reg.Get(payloadDoctype(op.Payload))
	if dt == nil {
		return false
	}
	return execution.OwnerModuleID == contract.ModuleID(dt.ModuleOwnerID)
}

func mutationTouchesCommandOnly(reg *doctype.Registry, op Operation) bool {
	if reg == nil {
		return false
	}
	check := func(name string) bool {
		dt := reg.Get(name)
		if dt == nil {
			return false
		}
		return dt.ModuleOwnershipMode == string(contract.MutationCommands)
	}
	switch op.Command {
	case CommandRecordCreate, CommandRecordUpdate, CommandRecordDelete, CommandWorkflowStatusTransition, CommandPublicFormSubmit:
		return check(payloadDoctype(op.Payload))
	case CommandRecordMutateBundle:
		var payload RecordMutationBundlePayload
		if json.Unmarshal(op.Payload, &payload) != nil {
			return false
		}
		for _, record := range payload.Records {
			if check(record.Doctype) {
				return true
			}
		}
	}
	return false
}

func authorizePublicForm(reg *doctype.Registry, op Operation) *contract.Error {
	if reg == nil || op.Context.Actor.PrincipalType != contract.PrincipalPublic {
		return ErrPermission
	}
	var payload RecordCreatePayload
	if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.PublicRoute == "" || payload.Doctype == "" {
		return contract.NewError(contract.CodeValidationFailed, "public form route and doctype are required")
	}
	if reg.Views == nil {
		return ErrPermission
	}
	view := reg.Views.GetByRoute(payload.PublicRoute)
	dt := reg.Get(payload.Doctype)
	if view == nil || view.PublicAccess == nil || !view.PublicAccess.Enabled || !view.PublicAccess.AllowMutations || view.SourceDocType != payload.Doctype || dt == nil || dt.PublicAccess == nil || !dt.PublicAccess.Enabled {
		return ErrPermission
	}
	if dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		return ErrPermission
	}
	return nil
}

type stepOutcome struct {
	Record  string `json:"record"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

// execDefinedCommand runs a config-defined command inside ONE SQL transaction:
// receipt claim, per-step writes (each validated against its target record
// schema), emitted events into the outbox, and the audit row — all commit or
// roll back together.
func (k *Kernel) execDefinedCommand(ctx context.Context, siteDB *sql.DB, dbTx *sql.Tx, txMgr *orm.TxManager, reg *doctype.Registry, op Operation, def CommandDefinition, opID string, cmd *CommandResource, afterCommit *func()) (json.RawMessage, *contract.Error) {
	ownsTransaction := dbTx == nil
	auditFailure := func(code contract.Code) {
		if ownsTransaction {
			k.writeFailureAudit(siteDB, op, def, opID, code)
		}
	}
	if ownsTransaction {
		var err error
		dbTx, err = siteDB.BeginTx(ctx, nil)
		if err != nil {
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "begin transaction: "+err.Error())
		}
		defer dbTx.Rollback()
	}
	priorMutationExecutor := txMgr.ScriptMutationExecutor
	priorPostCommitHooks := txMgr.PostCommitHooks
	priorTransaction := txMgr.Transaction
	priorUser, priorRole, priorRoles, priorContext := txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context
	priorBusinessScope := txMgr.BusinessScope
	priorSkipHooks := txMgr.SkipHookScripts
	txMgr.PostCommitHooks = nil
	txMgr.Transaction = dbTx
	txMgr.CurrentUser = op.Context.User
	txMgr.CurrentUserRole = op.Context.UserRole
	txMgr.CurrentUserRoles = append([]string(nil), op.Context.Roles...)
	txMgr.SkipHookScripts = append([]string(nil), op.Context.SkipHookScripts...)
	txMgr.Context = ctx
	txMgr.ScriptMutationExecutor = script.MutationExecutorFunc(func(nestedCtx context.Context, request script.MutationRequest) (json.RawMessage, error) {
		return k.executeScriptMutationInTx(nestedCtx, dbTx, txMgr, request)
	})
	defer func() {
		txMgr.ScriptMutationExecutor = priorMutationExecutor
		txMgr.PostCommitHooks = priorPostCommitHooks
		txMgr.Transaction = priorTransaction
		txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context = priorUser, priorRole, priorRoles, priorContext
		txMgr.BusinessScope = priorBusinessScope
		txMgr.SkipHookScripts = priorSkipHooks
	}()

	input, cerr := decodeInput(op.Payload)
	if cerr != nil {
		return nil, cerr
	}
	if cmd.ModuleOwnerID != "" {
		if k.CommandSchemas == nil || !k.CommandSchemas.Registered(cmd.InputSchema) || !k.CommandSchemas.Registered(cmd.OutputSchema) {
			return nil, contract.NewError(contract.CodeValidationFailed, "owner command input or output schema is not registered")
		}
		if err := k.CommandSchemas.Validate(cmd.InputSchema, op.Payload); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "owner command input failed its registered schema")
		}
	}

	user := op.Context.User
	if user == "" {
		user = "system"
	}
	txMgr.CurrentUser = user

	outcomes := make([]stepOutcome, 0, len(cmd.Steps))
	lastRecord, lastName := "", ""
	lastBusinessScope := op.Context.BusinessScope

	for i, step := range cmd.Steps {
		stepOp, scopeErr := operationForCommandStep(op, cmd, step)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if cmd.CrossEntity != nil {
			txMgr.BusinessScope = stepOp.Context.BusinessScope
		}
		lastBusinessScope = stepOp.Context.BusinessScope
		switch {
		case step.Create != nil:
			dt := reg.Get(step.Create.Record)
			if dt == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("command %q: record %q not found", def.Name, step.Create.Record))
			}
			doc := doctype.NewDocument(dt.Name)
			for field, ref := range step.Create.Values {
				v, rerr := resolveRef(ref, input)
				if rerr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
				}
				doc.Set(field, v)
			}
			if err := dt.BindBusinessScope(doc, txMgr.BusinessScope, true, nil); err != nil {
				return nil, contract.NewError(contract.CodePermissionDenied, err.Error())
			}
			setDefaultValues(dt, doc)
			if err := txMgr.RunHooksForValidate(dt, doc, nil); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if verrs := validateOperationDocumentWithResolvers(txMgr, dt, doc, reg, nil, op.Context.BusinessReferenceValidator, op.Context.HistoricalSnapshotValidator); verrs.HasErrors() {
				return nil, validationContractError(verrs)
			}
			if err := txMgr.PrepareInsert(dt, doc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if op.Context.IdempotencyKey != "" && def.IdempotentByKey && len(outcomes) == 0 {
				if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
					return nil, cerr
				}
			}
			owner := op.Context.Owner
			if owner == "" {
				owner = user
			}
			if err := txMgr.InsertInTx(dbTx, dt, doc, owner, user); err != nil {
				return nil, wrapORMError(err)
			}
			if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterInsert, doc, nil); hookErr != nil {
				return nil, hookErr
			}
			if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterSave, doc, nil); hookErr != nil {
				return nil, hookErr
			}
			doc.IsNew = false
			completeTx := *txMgr
			completeTx.ScriptMutationExecutor = nil
			completeTx.PostCommitHooks = nil
			completeTx.Transaction = nil
			completeTx.SkipHookScripts = nil
			txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteInsert(dt, doc) })
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: doc.Name, Created: true})
			lastRecord, lastName = dt.Name, doc.Name

		case step.Update != nil:
			dt := reg.Get(step.Update.Record)
			if dt == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("command %q: record %q not found", def.Name, step.Update.Record))
			}
			targetName, rerr := resolveStringRef(step.Update.Name, input)
			if rerr != nil || targetName == "" {
				return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
			}
			scope := scopeForOperation(reg, stepOp, dt.Name, "write")
			oldDoc, err := txMgr.GetDocWithScope(dt, targetName, scope)
			if err != nil {
				if err == sql.ErrNoRows || strings.Contains(err.Error(), orm.ErrNotFound.Error()) {
					return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("record %q not found", targetName))
				}
				return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
			}
			if ev := expectedVersionFrom(op); ev != "" {
				prior, perr := readRowVersion(dbTx, k.dialect(), dt.RawTableName(), targetName)
				if perr != nil {
					return nil, contract.NewError(contract.CodeNotFound, "document not found")
				}
				if prior != ev {
					auditFailure(ErrStaleVersion.Type)
					return nil, ErrStaleVersion
				}
			}
			doc := cloneDoc(oldDoc)
			for field, ref := range step.Update.Values {
				v, rerr := resolveRef(ref, input)
				if rerr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
				}
				doc.Set(field, v)
			}
			doc.IsNew = false
			if verrs := validateOperationDocumentWithResolvers(txMgr, dt, doc, reg, oldDoc, op.Context.BusinessReferenceValidator, op.Context.HistoricalSnapshotValidator); verrs.HasErrors() {
				return nil, validationContractError(verrs)
			}
			if err := txMgr.PrepareSave(dt, doc, oldDoc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if op.Context.IdempotencyKey != "" && def.IdempotentByKey && len(outcomes) == 0 {
				if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
					return nil, cerr
				}
			}
			if err := txMgr.SaveInTxWithScope(dbTx, dt, doc, user, scope, oldDoc); err != nil {
				return nil, wrapExpectedVersionORMError(err, op, k.dialect())
			}
			if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterSave, doc, oldDoc); hookErr != nil {
				return nil, hookErr
			}
			completeTx := *txMgr
			completeTx.ScriptMutationExecutor = nil
			completeTx.PostCommitHooks = nil
			completeTx.Transaction = nil
			completeTx.SkipHookScripts = nil
			txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteSave(dt, doc, oldDoc) })
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: targetName, Created: false})
			lastRecord, lastName = dt.Name, targetName
		}
	}

	raw, _ := json.Marshal(map[string]any{"command": def.Name, "steps": outcomes})
	if cmd.ModuleOwnerID != "" {
		if err := k.CommandSchemas.Validate(cmd.OutputSchema, raw); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "owner command output failed its registered schema")
		}
	}

	// Emit declared events through the outbox INSIDE the transaction so a
	// committed command's events are guaranteed durable (spec §19, §41).
	eventData, _ := json.Marshal(map[string]any{"command": def.Name, "steps": outcomes})
	for _, evtType := range cmd.Emits {
		env := contract.EventEnvelope{
			Type:          evtType,
			Source:        "kora.kernel",
			Site:          op.Context.Site,
			BusinessScope: lastBusinessScope,
			AggregateType: lastRecord,
			AggregateID:   lastName,
			CorrelationID: op.Context.CorrelationID,
			CausationID:   opID,
			Actor:         op.Context.Actor,
			Data:          eventData,
		}
		if k.Outbox == nil {
			break // no provider wired; skip silently only in tests — production wiring always sets Outbox
		}
		if err := k.Outbox.Append(ctx, dbTx, env); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "recording command event failed")
		}
	}

	row := k.buildAuditRow(op, def, opID, lastRecord, lastName, contract.StatusCompleted, "")
	row.Command = def.Name
	if _, aerr := k.writeAudit(dbTx, row); aerr != nil {
		return nil, contract.NewError(contract.CodeInternal, "audit write failed")
	}

	if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
		if cerr := k.storeReceiptResult(dbTx, op, contract.CommandResult{
			OperationID: opID, CorrelationID: op.Context.CorrelationID,
			Status: contract.StatusCompleted, Data: raw,
		}); cerr != nil {
			return nil, cerr
		}
	}

	postCommitHooks := append([]func(){}, txMgr.PostCommitHooks...)
	complete := func() {
		for _, hook := range postCommitHooks {
			hook()
		}
	}
	if ownsTransaction {
		if err := dbTx.Commit(); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "commit failed")
		}
		txMgr.ScriptMutationExecutor = nil
		txMgr.PostCommitHooks = nil
		txMgr.Transaction = nil
		complete()
	} else if afterCommit != nil {
		*afterCommit = complete
	}

	return raw, nil
}

// decodeInput extracts {"data": {...}} from the operation payload.
func decodeInput(raw json.RawMessage) (map[string]any, *contract.Error) {
	var wrapper struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "payload must be {\"data\":{...}}")
	}
	if wrapper.Data == nil {
		wrapper.Data = map[string]any{}
	}
	return wrapper.Data, nil
}

// resolveRef returns literal values as-is and resolves "$input.path" refs.
func resolveRef(ref string, input map[string]any) (any, error) {
	if !strings.HasPrefix(ref, "$input.") {
		return ref, nil
	}
	cur := any(input)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "$input."), ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference %q path invalid", ref)
		}
		cur, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("reference %q missing in input", ref)
		}
	}
	return cur, nil
}

func resolveStringRef(ref string, input map[string]any) (string, error) {
	v, err := resolveRef(ref, input)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("reference %q must resolve to a string", ref)
	}
	return s, nil
}

// readRowVersion reads the canonical version token for optimistic concurrency.
func readRowVersion(dbTx *sql.Tx, dialect db.Dialect, table, name string) (string, error) {
	var modified any
	query := fmt.Sprintf("SELECT modified FROM %s WHERE name = ?", dialect.QuoteIdent(table))
	if err := dbTx.QueryRow(db.Rebind(dialect, query), name).Scan(&modified); err != nil {
		return "", err
	}
	switch value := modified.(type) {
	case time.Time:
		return CanonicalVersion(value), nil
	case string:
		parsed, err := parseVersionTimestamp(value)
		if err != nil {
			return "", err
		}
		return CanonicalVersion(parsed), nil
	case []byte:
		parsed, err := parseVersionTimestamp(string(value))
		if err != nil {
			return "", err
		}
		return CanonicalVersion(parsed), nil
	default:
		return "", fmt.Errorf("unsupported modified timestamp type %T", modified)
	}
}

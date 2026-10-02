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
	"github.com/asenawritescode/kora/script"
)

// authorizeOp dispatches authorization: built-in commands authorize against
// the payload's doctype; config-defined commands authorize EVERY touched
// record at per-step least privilege — create steps need "create", update
// steps need "write" on their target record (default-deny, spec §51/§108).
func authorizeOp(reg *doctype.Registry, op Operation, def CommandDefinition, dyn *CommandResource) *contract.Error {
	if op.Command == CommandPublicFormSubmit {
		return authorizePublicForm(reg, op)
	}
	if op.Command == CommandRecordMutateBundle {
		if reg == nil {
			return ErrPermission
		}
		var payload RecordMutationBundlePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Doctype == "" || len(payload.Records) == 0 || len(payload.Records) > 10 {
			return contract.NewError(contract.CodeValidationFailed, "record.mutate_bundle requires 1 to 10 records")
		}
		roles := op.Context.Roles
		if len(roles) == 0 {
			roles = []string{doctype.AdminRole}
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
			if permission != "create" && permission != "update" {
				return contract.NewError(contract.CodeValidationFailed, "bundle operation must be create or update")
			}
			if permission == "update" && record.Name == "" {
				return contract.NewError(contract.CodeValidationFailed, "bundle update requires a record name")
			}
			if permission == "update" {
				permission = "write"
			}
			if allowed, _ := reg.CanUser(roles, record.Doctype, permission); !allowed {
				return ErrPermission
			}
		}
		if payload.Records[0].Doctype != payload.Doctype {
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
		switch {
		case step.Create != nil:
			if allowed, _ := reg.CanUser(op.Context.Roles, step.Create.Record, "create"); !allowed {
				return ErrPermission
			}
		case step.Update != nil:
			if allowed, _ := reg.CanUser(op.Context.Roles, step.Update.Record, "write"); !allowed {
				return ErrPermission
			}
		}
	}
	return nil
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
func (k *Kernel) execDefinedCommand(ctx context.Context, siteDB *sql.DB, txMgr *orm.TxManager, reg *doctype.Registry, op Operation, def CommandDefinition, opID string, cmd *CommandResource) (json.RawMessage, *contract.Error) {
	dbTx, err := siteDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "begin transaction: "+err.Error())
	}
	defer dbTx.Rollback()
	priorMutationExecutor := txMgr.ScriptMutationExecutor
	priorPostCommitHooks := txMgr.PostCommitHooks
	priorUser, priorRole, priorRoles, priorContext := txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context
	priorSkipHooks := txMgr.SkipHookScripts
	txMgr.PostCommitHooks = nil
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
		txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context = priorUser, priorRole, priorRoles, priorContext
		txMgr.SkipHookScripts = priorSkipHooks
	}()

	input, cerr := decodeInput(op.Payload)
	if cerr != nil {
		return nil, cerr
	}

	user := op.Context.User
	if user == "" {
		user = "system"
	}
	txMgr.CurrentUser = user

	outcomes := make([]stepOutcome, 0, len(cmd.Steps))
	lastRecord, lastName := "", ""

	for i, step := range cmd.Steps {
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
			setDefaultValues(dt, doc)
			if err := txMgr.RunHooksForValidate(dt, doc, nil); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if verrs := validateOperationDocument(txMgr, dt, doc, reg, nil); verrs.HasErrors() {
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
			doc.IsNew = false
			completeTx := *txMgr
			completeTx.ScriptMutationExecutor = nil
			completeTx.PostCommitHooks = nil
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
			owner := ownerForOperation(reg, op, dt.Name, "write")
			oldDoc, err := txMgr.GetDoc(dt, targetName, owner)
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
					k.writeFailureAudit(siteDB, op, def, opID, ErrStaleVersion.Type)
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
			if verrs := validateOperationDocument(txMgr, dt, doc, reg, oldDoc); verrs.HasErrors() {
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
			if err := txMgr.SaveInTx(dbTx, dt, doc, user, owner, oldDoc); err != nil {
				return nil, wrapExpectedVersionORMError(err, op, k.dialect())
			}
			completeTx := *txMgr
			completeTx.ScriptMutationExecutor = nil
			completeTx.PostCommitHooks = nil
			completeTx.SkipHookScripts = nil
			txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteSave(dt, doc, oldDoc) })
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: targetName, Created: false})
			lastRecord, lastName = dt.Name, targetName
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

	raw, _ := json.Marshal(map[string]any{"command": def.Name, "steps": outcomes})
	if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
		if cerr := k.storeReceiptResult(dbTx, op, contract.CommandResult{
			OperationID: opID, CorrelationID: op.Context.CorrelationID,
			Status: contract.StatusCompleted, Data: raw,
		}); cerr != nil {
			return nil, cerr
		}
	}

	if err := dbTx.Commit(); err != nil {
		return nil, contract.NewError(contract.CodeInternal, "commit failed")
	}
	postCommitHooks := append([]func(){}, txMgr.PostCommitHooks...)
	txMgr.ScriptMutationExecutor = nil
	txMgr.PostCommitHooks = nil
	for _, hook := range postCommitHooks {
		hook()
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

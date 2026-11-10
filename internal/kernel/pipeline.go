package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/workflow"
)

// payloadDoctype extracts the target doctype from either typed payload.
func payloadDoctype(raw json.RawMessage) string {
	var probe struct {
		Doctype string `json:"doctype"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Doctype
}

// executeInTx runs mutation + receipt claim + audit write inside ONE SQL
// transaction (the mutation itself writes its outbox rows via orm InTx
// variants). Any failure rolls back everything; a commit implies all of it.
func (k *Kernel) executeInTx(ctx context.Context, run txRun, op Operation, def CommandDefinition) (*ResultData, *contract.Error) {
	if op.Context.HistoricalSnapshotValidator == nil {
		op.Context.HistoricalSnapshotValidator = newHistoricalSnapshotValidator(op.Context.BusinessScope, k.CommandSchemas)
	}
	ownsTransaction := run.Tx == nil
	auditFailure := func(code contract.Code) {
		if ownsTransaction {
			k.writeFailureAudit(run.SiteDB, op, def, run.OpID, code)
		}
	}
	run, rollback, beginErr := run.begin(ctx)
	if beginErr != nil {
		return nil, beginErr
	}
	defer rollback()
	dbTx, txMgr, reg, opID := run.Tx, run.Mgr, run.Reg, run.OpID
	if admissionErr := enforceModuleLifecycleAdmissionTx(ctx, dbTx, op, admissionScope{Dialect: k.dialect(), Registry: reg}); admissionErr != nil {
		return nil, admissionErr
	}
	priorMutationExecutor := txMgr.ScriptMutationExecutor
	priorPostCommitHooks := txMgr.PostCommitHooks
	priorTransaction := txMgr.Transaction
	priorReferenceValidator, priorSnapshotValidator := txMgr.BusinessReferenceValidator, txMgr.HistoricalSnapshotValidator
	txMgr.BusinessReferenceValidator = op.Context.BusinessReferenceValidator
	txMgr.HistoricalSnapshotValidator = op.Context.HistoricalSnapshotValidator
	priorEventOrigin := txMgr.EventOrigin
	txMgr.Transaction = dbTx
	txMgr.EventOrigin = eventOrigin(op)
	txMgr.PostCommitHooks = nil
	txMgr.ScriptMutationExecutor = script.MutationExecutorFunc(func(nestedCtx context.Context, request script.MutationRequest) (json.RawMessage, error) {
		return k.executeScriptMutationInTx(nestedCtx, dbTx, txMgr, request)
	})
	defer func() {
		txMgr.ScriptMutationExecutor = priorMutationExecutor
		txMgr.PostCommitHooks = priorPostCommitHooks
		txMgr.Transaction = priorTransaction
		txMgr.EventOrigin = priorEventOrigin
		txMgr.BusinessReferenceValidator, txMgr.HistoricalSnapshotValidator = priorReferenceValidator, priorSnapshotValidator
	}()

	doctypeName := payloadDoctype(op.Payload)
	dt := reg.Get(doctypeName)
	if dt == nil {
		return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", doctypeName))
	}
	if err := txMgr.LoadLifecycleHooksTx(ctx, dbTx, dt); err != nil {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, err.Error())
	}

	user := op.Context.User
	if user == "" {
		user = "system"
	}
	owner := op.Context.Owner
	if owner == "" {
		owner = user
	}

	m := &mutation{k: k, run: run, op: op, def: def, dt: dt, ownsTx: ownsTransaction}

	var stepErr *contract.Error
	switch op.Command {
	case CommandRecordMutateBundle:
		var p RecordMutationBundlePayload
		if err := json.Unmarshal(op.Payload, &p); err != nil || len(p.Records) == 0 || len(p.Records) > MaxMutationBundleRecords || p.Doctype != dt.Name || p.Records[0].Doctype != dt.Name {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid record.mutate_bundle payload")
		}
		keySet := make(map[string]struct{}, len(p.Records))
		for _, item := range p.Records {
			if item.Key == "" || item.Doctype == "" {
				return nil, contract.NewError(contract.CodeValidationFailed, "bundle record key and doctype are required")
			}
			if _, exists := keySet[item.Key]; exists {
				return nil, contract.NewError(contract.CodeValidationFailed, "bundle record keys must be unique")
			}
			if reg.Get(item.Doctype) == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", item.Doctype))
			}
			keySet[item.Key] = struct{}{}
		}
		for _, item := range p.Records {
			recordDT := reg.Get(item.Doctype)
			if err := txMgr.LoadLifecycleHooksTx(ctx, dbTx, recordDT); err != nil {
				return nil, contract.NewError(contract.CodeDependencyUnavailable, err.Error())
			}
		}
		// Claim the idempotency key before allocating names or performing other
		// transactional writes. Concurrent retries must contend on the unique
		// receipt first; otherwise SQLite-compatible dialects can report a name
		// allocator lock error before the duplicate is recognized as a replay.
		if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
			if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
				return nil, cerr
			}
		}

		recordNames := make(map[string]string, len(p.Records))
		for _, item := range p.Records {
			operation := item.Operation
			if operation == "" {
				operation = "create"
			}
			if operation == "create" {
				recordDT := reg.Get(item.Doctype)
				name, _, allocationErr := orm.AllocateDocumentName(dbTx, k.dialect(), recordDT, orm.DocumentValues(item.Data), time.Now().UTC())
				if allocationErr != nil {
					var validation *doctype.ValidationError
					if errors.As(allocationErr, &validation) {
						return nil, contract.NewError(contract.CodeValidationFailed, validation.Message)
					}
					return nil, contract.NewError(contract.CodeDependencyUnavailable, "allocate bundle record name failed")
				}
				recordNames[item.Key] = name
			} else {
				name, resolveErr := resolveBundleName(item.Name, recordNames)
				if resolveErr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, resolveErr.Error())
				}
				recordNames[item.Key] = name
			}
		}
		bundleDocs := make([]*doctype.Document, len(p.Records))
		bundleTypes := make([]*doctype.DocType, len(p.Records))
		pendingLinks := make(map[string]*doctype.Document, len(p.Records))
		for i, item := range p.Records {
			recordDT := reg.Get(item.Doctype)
			operation := item.Operation
			if operation == "" {
				operation = "create"
			}
			if operation != "create" && operation != "update" && operation != "decrement" && operation != "adjust" {
				return nil, contract.NewError(contract.CodeValidationFailed, "bundle operation must be create, update, decrement, or adjust")
			}
			permission := "create"
			if operation != "create" {
				permission = "write"
			}
			if !op.Context.isEffectItem(item.Key) {
				if _, permissionErr := decideOperationResource(op, reg, recordDT, permission); permissionErr != nil {
					return nil, permissionErr
				}
			}
			if item.TransitionAction != "" {
				if operation != "update" {
					return nil, contract.NewError(contract.CodeValidationFailed, "transition actions require an update bundle item")
				}
				if _, permissionErr := decideOperationResource(op, reg, recordDT, "submit"); permissionErr != nil {
					return nil, permissionErr
				}
			}
			data, err := resolveCreateBundleData(item.Data, recordNames)
			if err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("bundle record %q: %v", item.Key, err))
			}
			if operation == "create" {
				if rejectErr := k.rejectWorkflowStateOnCreate(ctx, dbTx, op, recordDT, data); rejectErr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("bundle record %q: %v", item.Key, rejectErr))
				}
			}
			doc := doctype.NewDocument(recordDT.Name)
			doc.Name = recordNames[item.Key]
			var oldDoc *doctype.Document
			if operation != "create" {
				if rejectErr := k.rejectWorkflowStateMutation(ctx, dbTx, op, recordDT, data); rejectErr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, rejectErr.Error())
				}
				oldDoc, err = txMgr.GetDocWithScope(recordDT, doc.Name, scopeForOperation(reg, op, recordDT.Name, "write"))
				if err != nil {
					if errors.Is(err, orm.ErrNotFound) {
						return nil, contract.NewError(contract.CodeNotFound, "document not found")
					}
					return nil, contract.NewError(contract.CodeDependencyUnavailable, "load bundle document failed")
				}
				if item.TransitionAction != "" {
					if _, err = txMgr.GetDocWithScope(recordDT, doc.Name, scopeForOperation(reg, op, recordDT.Name, "submit")); err != nil {
						if errors.Is(err, orm.ErrNotFound) {
							return nil, contract.NewError(contract.CodeNotFound, "document not found")
						}
						return nil, contract.NewError(contract.CodeDependencyUnavailable, "load workflow document failed")
					}
				}
				if item.ExpectedRevision > 0 && oldDoc.Revision != item.ExpectedRevision {
					auditFailure(ErrStaleVersion.Type)
					return nil, ErrStaleVersion
				}
				if i == 0 {
					if expectedVersion := expectedVersionFrom(op); expectedVersion != "" {
						currentVersion, versionErr := readRowVersion(dbTx, k.dialect(), recordDT.RawTableName(), doc.Name)
						if versionErr != nil {
							return nil, contract.NewError(contract.CodeDependencyUnavailable, "read bundle record version failed")
						}
						if currentVersion != expectedVersion {
							auditFailure(ErrStaleVersion.Type)
							return nil, ErrStaleVersion
						}
					}
				}
				doc = cloneDoc(oldDoc)
			}
			if operation == "decrement" || operation == "adjust" {
				adjusted, err := adjustQuantity(recordDT, oldDoc, operation, data)
				if err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
				}
				data = adjusted
			}
			if err := applyData(recordDT, doc, data, reg, operation == "update" && !op.Context.AllowReadOnlyFields); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if err := recordDT.BindBusinessScope(doc, op.Context.BusinessScope, operation == "create", oldDoc); err != nil {
				return nil, contract.NewError(contract.CodePermissionDenied, err.Error())
			}
			if item.TransitionAction != "" {
				statusPolicy, unified, policyErr := k.workflowStatusPolicy(ctx, dbTx, op, recordDT.Name)
				if policyErr != nil {
					return nil, contract.NewError(contract.CodeDependencyUnavailable, "load workflow status policy failed")
				}
				if !unified || statusPolicy == nil {
					return nil, contract.NewError(contract.CodeValidationFailed, "no published status transition policy for table")
				}
				stateField := statusPolicy.StateField
				if recordDT.GetField(stateField) == nil || oldDoc == nil {
					return nil, contract.NewError(contract.CodeValidationFailed, "workflow state field is not defined on doctype")
				}
				currentState := fmt.Sprint(oldDoc.Get(stateField))
				if currentState == "<nil>" || currentState == "" {
					currentState = statusPolicy.InitialState
				}
				roles := append([]string(nil), op.Context.Roles...)
				if op.Context.UserRole != "" {
					found := false
					for _, role := range roles {
						found = found || role == op.Context.UserRole
					}
					if !found {
						roles = append([]string{op.Context.UserRole}, roles...)
					}
				}
				if len(roles) == 0 {
					return nil, contract.NewError(contract.CodePermissionDenied, "workflow transition requires an assigned role")
				}
				var newState string
				var newStatus int
				var transitionErr error
				newState, newStatus, transitionErr = workflow.ApplyStatusTransition(statusPolicy, currentState, item.TransitionAction, roles, doc)
				if transitionErr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, transitionErr.Error())
				}
				doc.Set(stateField, newState)
				doc.DocStatus = newStatus
			}
			if operation == "create" {
				setDefaultValues(recordDT, doc)
			} else {
				doc.IsNew = false
			}
			bundleDocs[i], bundleTypes[i] = doc, recordDT
			pendingLinks[recordDT.Name+"\x00"+doc.Name] = doc
			m.bundleSavedDocs = append(m.bundleSavedDocs, savedBundleDocument{dt: recordDT, doc: doc, oldDoc: oldDoc, workflowTransition: item.TransitionAction != ""})
		}
		resolver := func(target *doctype.DocType, name string) (*doctype.Document, error) {
			if target != nil {
				if pending, exists := pendingLinks[target.Name+"\x00"+name]; exists {
					return pending, nil
				}
			}
			return txMgr.GetDoc(target, name, "")
		}
		for i, doc := range bundleDocs {
			recordDT := bundleTypes[i]
			oldDoc := m.bundleSavedDocs[i].oldDoc
			if err := txMgr.RunHooksForValidate(recordDT, doc, oldDoc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if oldDoc == nil {
				if err := k.initializeWorkflowStateOnCreate(ctx, dbTx, op, recordDT, doc); err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
				}
			}
			if oldDoc != nil && !m.bundleSavedDocs[i].workflowTransition {
				if err := k.rejectWorkflowStateChange(ctx, dbTx, op, recordDT, docChange{oldDoc, doc}); err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
				}
			}
			if verrs := doctype.ValidateDocumentWithResolvers(recordDT, doc, reg, oldDoc, resolver, op.Context.BusinessReferenceValidator, op.Context.HistoricalSnapshotValidator); verrs.HasErrors() {
				return nil, validationContractError(verrs)
			}
			if oldDoc == nil {
				if err := txMgr.PrepareInsert(recordDT, doc); err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
				}
			} else if err := txMgr.PrepareSave(recordDT, doc, oldDoc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
		}
		related := make([]RelatedRecordResult, 0, len(p.Records)-1)
		for i, doc := range bundleDocs {
			recordDT := bundleTypes[i]
			oldDoc := m.bundleSavedDocs[i].oldDoc
			beforeHash := ""
			if oldDoc == nil {
				if err := txMgr.InsertInTx(dbTx, recordDT, doc, owner, user); err != nil {
					return nil, wrapORMError(err)
				}
				if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, recordDT, lifecycleEvent{script.EventAfterInsert, doc, nil}); hookErr != nil {
					return nil, hookErr
				}
				if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, recordDT, lifecycleEvent{script.EventAfterSave, doc, nil}); hookErr != nil {
					return nil, hookErr
				}
				m.insertedDocs = append(m.insertedDocs, doc)
			} else {
				beforeHash = CanonicalDocHash(oldDoc.Fields)
				if err := txMgr.SaveInTxWithScope(dbTx, recordDT, doc, user, scopeForOperation(reg, op, recordDT.Name, "write"), oldDoc); err != nil {
					return nil, wrapExpectedVersionORMError(err, op, k.dialect())
				}
				if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, recordDT, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
					return nil, hookErr
				}
				m.bundleSavedDocs[i].oldDoc = oldDoc
			}
			doc.IsNew = false
			afterHash := CanonicalDocHash(doc.Fields)
			row := k.buildAuditRow(op, def, opID, auditOutcome{Doctype: recordDT.Name, DocName: doc.Name, Status: contract.StatusCompleted})
			row.BeforeHash = beforeHash
			row.AfterHash = afterHash
			auditID, auditErr := k.writeAudit(dbTx, row)
			if auditErr != nil {
				return nil, contract.NewError(contract.CodeInternal, "audit write failed")
			}
			if historyErr := k.writeRecordHistory(dbTx, row, recordDT, oldDoc, doc); historyErr != nil {
				return nil, contract.NewError(contract.CodeInternal, "record history write failed")
			}
			if i == 0 {
				m.result = &ResultData{Doctype: recordDT.Name, Name: doc.Name, Created: oldDoc == nil, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
			} else {
				related = append(related, RelatedRecordResult{Key: p.Records[i].Key, Doctype: recordDT.Name, Name: doc.Name, Created: oldDoc == nil})
			}
		}
		m.result.Related = related

	case CommandRecordCreate, CommandPublicFormSubmit:
		var p RecordCreatePayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid record.create payload")
		}
		if op.Command == CommandPublicFormSubmit {
			var submitted map[string]any
			if err := json.Unmarshal(p.Data, &submitted); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, "public form data must be an object")
			}
			publicFields := dt.PublicFieldSet()
			filtered := make(map[string]any, len(submitted))
			for field, value := range submitted {
				if publicFields[field] {
					filtered[field] = value
				}
			}
			p.Data, _ = json.Marshal(filtered)
		}
		doc := doctype.NewDocument(dt.Name)
		if err := k.rejectWorkflowStateOnCreate(ctx, dbTx, op, dt, p.Data); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := applyData(dt, doc, p.Data, reg, false); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := dt.BindBusinessScope(doc, op.Context.BusinessScope, true, nil); err != nil {
			return nil, contract.NewError(contract.CodePermissionDenied, err.Error())
		}
		setDefaultValues(dt, doc)
		if err := txMgr.RunHooksForValidate(dt, doc, nil); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := k.initializeWorkflowStateOnCreate(ctx, dbTx, op, dt, doc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if verrs := validateOperationDocument(txMgr, dt, doc, reg, documentCheck{Old: nil, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}); verrs.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
			return nil, validationContractError(verrs)
		}
		if err := txMgr.PrepareInsert(dt, doc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
			if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
				return nil, cerr
			}
		}
		afterHash := CanonicalDocHash(doc.Fields)
		if err := txMgr.InsertInTx(dbTx, dt, doc, owner, user); err != nil {
			return nil, wrapORMError(err)
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterInsert, doc, nil}); hookErr != nil {
			return nil, hookErr
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, nil}); hookErr != nil {
			return nil, hookErr
		}
		doc.IsNew = false
		row := k.buildAuditRow(op, def, opID, auditOutcome{Doctype: dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
		row.AfterHash = afterHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, nil, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		m.result = &ResultData{Doctype: dt.Name, Name: doc.Name, Created: true, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
		m.insertedDocs = append(m.insertedDocs, doc)

	case CommandRecordUpdate:
		var p RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid record.update payload")
		}
		if p.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "record.update missing name")
		}
		if err := k.rejectWorkflowStateMutation(ctx, dbTx, op, dt, p.Data); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		scope := scopeForOperation(reg, op, dt.Name, "write")
		oldDoc, err := txMgr.GetDocWithScope(dt, p.Name, scope)
		if err != nil {
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
		}
		// Compare the caller's canonical modified-time token while the row is
		// protected by this transaction; Revision is a separate internal counter.
		if ev := expectedVersionFrom(op); ev != "" {
			prior, versionErr := readRowVersion(dbTx, k.dialect(), dt.RawTableName(), p.Name)
			if versionErr != nil {
				return nil, contract.NewError(contract.CodeDependencyUnavailable, "read document version failed")
			}
			if prior != ev {
				auditFailure(ErrStaleVersion.Type)
				return nil, ErrStaleVersion
			}
		}
		doc := cloneDoc(oldDoc)
		if err := applyData(dt, doc, p.Data, reg, !op.Context.AllowReadOnlyFields); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := dt.BindBusinessScope(doc, op.Context.BusinessScope, false, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodePermissionDenied, err.Error())
		}
		doc.IsNew = false
		if err := txMgr.RunHooksForValidate(dt, doc, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := k.rejectWorkflowStateChange(ctx, dbTx, op, dt, docChange{oldDoc, doc}); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if verrs := validateOperationDocument(txMgr, dt, doc, reg, documentCheck{Old: oldDoc, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}); verrs.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
			return nil, validationContractError(verrs)
		}
		if err := txMgr.PrepareSave(dt, doc, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
			if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
				return nil, cerr
			}
		}
		beforeHash := CanonicalDocHash(oldDoc.Fields)
		afterHash := CanonicalDocHash(doc.Fields)
		if err := txMgr.SaveInTxWithScope(dbTx, dt, doc, user, scope, oldDoc); err != nil {
			return nil, wrapExpectedVersionORMError(err, op, k.dialect())
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, def, opID, auditOutcome{Doctype: dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
		row.BeforeHash = beforeHash
		row.AfterHash = afterHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		m.result = &ResultData{Doctype: dt.Name, Name: doc.Name, Created: false, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
		m.savedDoc, m.savedOldDoc = doc, oldDoc

	case CommandRecordDelete:
		stepErr = m.remove(ctx)

	case CommandWorkflowStatusTransition:
		var p WorkflowStatusTransitionPayload
		if err := json.Unmarshal(op.Payload, &p); err != nil || p.Name == "" || p.Action == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "workflow.status_transition requires name and action")
		}
		if op.Context.ExpectedConfigRevision > 0 {
			if err := configstore.FenceConfigRevisionTx(ctx, dbTx, k.dialect(), op.Context.ExpectedConfigRevision); err != nil {
				if errors.Is(err, configstore.ErrConfigRevisionConflict) {
					auditFailure(ErrStaleVersion.Type)
					return nil, contract.NewError(contract.CodeConflict, "tenant configuration changed while the workflow transition was being prepared")
				}
				return nil, contract.NewError(contract.CodeDependencyUnavailable, "lock workflow configuration revision failed")
			}
		}
		statusPolicy, hasStatusPolicy, statusPolicyErr := k.workflowStatusPolicy(ctx, dbTx, op, dt.Name)
		if statusPolicyErr != nil {
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load workflow status policy failed")
		}
		if !hasStatusPolicy || statusPolicy == nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "no published status transition policy for table")
		}
		stateField := statusPolicy.StateField
		if dt.GetField(stateField) == nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "workflow state field is not defined on doctype")
		}
		scope := scopeForOperation(reg, op, dt.Name, "submit")
		oldDoc, err := txMgr.GetDocWithScope(dt, p.Name, scope)
		if err != nil {
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
		}
		if p.ExpectedRevision > 0 && oldDoc.Revision != p.ExpectedRevision {
			auditFailure(ErrStaleVersion.Type)
			return nil, ErrStaleVersion
		}
		if ev := expectedVersionFrom(op); ev != "" {
			prior, versionErr := readRowVersion(dbTx, k.dialect(), dt.RawTableName(), p.Name)
			if versionErr != nil {
				return nil, contract.NewError(contract.CodeDependencyUnavailable, "read document version failed")
			}
			if prior != ev {
				auditFailure(ErrStaleVersion.Type)
				return nil, ErrStaleVersion
			}
		}
		currentState := fmt.Sprint(oldDoc.Get(stateField))
		if currentState == "<nil>" || currentState == "" {
			currentState = statusPolicy.InitialState
		}
		roles := append([]string(nil), op.Context.Roles...)
		if op.Context.UserRole != "" {
			found := false
			for _, role := range roles {
				found = found || role == op.Context.UserRole
			}
			if !found {
				roles = append([]string{op.Context.UserRole}, roles...)
			}
		}
		if len(roles) == 0 {
			return nil, contract.NewError(contract.CodePermissionDenied, "workflow transition requires an assigned role")
		}
		var newState string
		var newStatus int
		var transitionErr error
		newState, newStatus, transitionErr = workflow.ApplyStatusTransition(statusPolicy, currentState, p.Action, roles, oldDoc)
		if transitionErr != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, transitionErr.Error())
		}
		doc := cloneDoc(oldDoc)
		doc.Set(stateField, newState)
		doc.DocStatus = newStatus
		if err := txMgr.PrepareSave(dt, doc, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
			if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
				return nil, cerr
			}
		}
		beforeHash := CanonicalDocHash(oldDoc.Fields)
		afterHash := CanonicalDocHash(doc.Fields)
		if err := txMgr.SaveInTxWithScope(dbTx, dt, doc, user, scope, oldDoc); err != nil {
			return nil, wrapORMError(err)
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, def, opID, auditOutcome{Doctype: dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
		row.BeforeHash = beforeHash
		row.AfterHash = afterHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		m.result = &ResultData{Doctype: dt.Name, Name: doc.Name, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
		m.savedDoc, m.savedOldDoc = doc, oldDoc

	default:
		return nil, contract.NewError(contract.CodeValidationFailed, "unknown command "+op.Command)
	}
	if stepErr != nil {
		return nil, stepErr
	}
	return m.finish() //nolint:contextcheck // the context travels in txMgr.Context
}

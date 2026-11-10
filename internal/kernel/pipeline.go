package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
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

	m := &mutation{k: k, run: run, op: op, def: def, dt: dt, user: user, owner: owner, ownsTx: ownsTransaction, auditFailure: auditFailure}

	var stepErr *contract.Error
	switch op.Command {
	case CommandRecordMutateBundle:
		b, cerr := m.openBundle(ctx)
		if cerr != nil {
			return nil, cerr
		}
		p, recordNames := b.payload, b.names
		bundleDocs := make([]*doctype.Document, len(p.Records))
		bundleTypes := make([]*doctype.DocType, len(p.Records))
		pendingLinks := make(map[string]*doctype.Document, len(p.Records))
		for i, item := range p.Records {
			recordDT := reg.Get(item.Doctype)
			operation, itemErr := m.authorizeItem(item, recordDT)
			if itemErr != nil {
				return nil, itemErr
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
				oldDoc, itemErr = m.loadItem(item, i, recordDT, doc.Name) //nolint:contextcheck // the context travels in txMgr.Context
				if itemErr != nil {
					return nil, itemErr
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
		stepErr = m.create(ctx)

	case CommandRecordUpdate:
		stepErr = m.update(ctx)

	case CommandRecordDelete:
		stepErr = m.remove(ctx)

	case CommandWorkflowStatusTransition:
		stepErr = m.transition(ctx)

	default:
		return nil, contract.NewError(contract.CodeValidationFailed, "unknown command "+op.Command)
	}
	if stepErr != nil {
		return nil, stepErr
	}
	return m.finish() //nolint:contextcheck // the context travels in txMgr.Context
}

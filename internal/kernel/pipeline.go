package kernel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/workflow"
)

var createBundleReference = regexp.MustCompile(`\$records\.([A-Za-z0-9_-]+)\.name`)

type savedBundleDocument struct {
	dt                 *doctype.DocType
	doc                *doctype.Document
	oldDoc             *doctype.Document
	workflowTransition bool
}

// Typed sentinel errors. Adapters map these to wire errors via contract.Code;
// they must never be detected by string matching.
var (
	ErrUnauthenticated = contract.NewError(contract.CodeUnauthenticated, "operation context has no authenticated actor")
	ErrNoSite          = contract.NewError(contract.CodeValidationFailed, "operation context missing tenant site")
	ErrPermission      = contract.NewError(contract.CodePermissionDenied, "not permitted")
	ErrStaleVersion    = contract.NewError(contract.CodeConflict, "document was modified by another operation")
	ErrKeyReused       = contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key already used with a different payload")
)

// validateContext enforces fail-closed identity: no site or no authenticated
// principal means no execution. A mismatch between the executing site and the
// actor's home site is a cross-tenant attempt and is rejected.
func (k *Kernel) validateContext(op Operation) *contract.Error {
	if op.Context.Site == "" {
		return ErrNoSite
	}
	if !op.Context.Actor.Authenticated() {
		return ErrUnauthenticated
	}
	if op.Context.Actor.Site != "" && op.Context.Actor.Site != op.Context.Site {
		return ErrPermission
	}
	return nil
}

// authorize evaluates the site registry's permission matrix identically for
// every source. AI/MCP actors carry resolved human subjects; the gate never
// special-cases them (authorization parity, KERNEL-004 / SEC-003).
func authorize(reg *doctype.Registry, op Operation, def CommandDefinition) *contract.Error {
	if reg == nil {
		return ErrPermission
	}
	doctypeName := payloadDoctype(op.Payload)
	if doctypeName == "" {
		return contract.NewError(contract.CodeValidationFailed, "payload missing doctype")
	}
	if resolved := reg.Get(doctypeName); resolved != nil {
		doctypeName = resolved.Name
		if resolved.AppendOnly && op.Command != CommandRecordCreate && op.Command != CommandPublicFormSubmit && op.Command != CommandRecordMutateBundle {
			return contract.NewError(contract.CodeValidationFailed, "append-only records cannot be changed")
		}
	}
	permission := def.AuthorizesWith
	var bundle RecordMutationBundlePayload
	if op.Command == CommandRecordMutateBundle && json.Unmarshal(op.Payload, &bundle) == nil && len(bundle.Records) > 0 {
		permission = bundle.Records[0].Operation
		if permission == "" {
			permission = "create"
		}
		if permission == "update" && bundle.Records[0].TransitionAction != "" {
			if _, err := decideOperationResource(op, reg, reg.Get(doctypeName), "submit"); err != nil {
				return err
			}
		}
	}
	_, err := decideOperationResource(op, reg, reg.Get(doctypeName), permission)
	return err
}

func decideOperationResource(op Operation, reg *doctype.Registry, dt *doctype.DocType, operation string) (policy.Decision, *contract.Error) {
	if dt == nil {
		return policy.Decision{}, contract.NewError(contract.CodeNotFound, "resource not found")
	}
	return policy.DecideForBusinessScope(op.Context.Execution, reg, dt.Name, operation, op.Context.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned)
}

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

func captureLifecycleHooks(ctx context.Context, txMgr *orm.TxManager, tx *sql.Tx, dt *doctype.DocType, event script.Event, doc, oldDoc *doctype.Document) *contract.Error {
	if err := txMgr.CaptureAsyncAfterHooksTx(ctx, tx, dt, event, doc, oldDoc); err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "persisting async lifecycle hooks failed: "+err.Error())
	}
	return nil
}

// executeInTx runs mutation + receipt claim + audit write inside ONE SQL
// transaction (the mutation itself writes its outbox rows via orm InTx
// variants). Any failure rolls back everything; a commit implies all of it.
func (k *Kernel) executeInTx(ctx context.Context, siteDB *sql.DB, dbTx *sql.Tx, txMgr *orm.TxManager, reg *doctype.Registry, op Operation, def CommandDefinition, opID string, afterCommit *func()) (*ResultData, *contract.Error) {
	if op.Context.HistoricalSnapshotValidator == nil {
		op.Context.HistoricalSnapshotValidator = newHistoricalSnapshotValidator(op.Context.BusinessScope, k.CommandSchemas)
	}
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
	if admissionErr := enforceModuleLifecycleAdmissionTx(ctx, dbTx, k.dialect(), op, reg, nil); admissionErr != nil {
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

	var result *ResultData
	var deletedDoc *doctype.Document
	var insertedDocs []*doctype.Document
	var savedDoc, savedOldDoc *doctype.Document
	var bundleSavedDocs []savedBundleDocument

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
				if err := k.rejectWorkflowStateOnCreate(ctx, dbTx, op, recordDT, reg, data); err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("bundle record %q: %v", item.Key, err))
				}
			}
			doc := doctype.NewDocument(recordDT.Name)
			doc.Name = recordNames[item.Key]
			var oldDoc *doctype.Document
			if operation != "create" {
				if err := k.rejectWorkflowStateMutation(ctx, dbTx, op, recordDT, reg, data); err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
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
			bundleSavedDocs = append(bundleSavedDocs, savedBundleDocument{dt: recordDT, doc: doc, oldDoc: oldDoc, workflowTransition: item.TransitionAction != ""})
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
			oldDoc := bundleSavedDocs[i].oldDoc
			if err := txMgr.RunHooksForValidate(recordDT, doc, oldDoc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if oldDoc == nil {
				if err := k.initializeWorkflowStateOnCreate(ctx, dbTx, op, recordDT, doc); err != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
				}
			}
			if oldDoc != nil && !bundleSavedDocs[i].workflowTransition {
				if err := k.rejectWorkflowStateChange(ctx, dbTx, op, recordDT, reg, oldDoc, doc); err != nil {
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
			oldDoc := bundleSavedDocs[i].oldDoc
			beforeHash := ""
			if oldDoc == nil {
				if err := txMgr.InsertInTx(dbTx, recordDT, doc, owner, user); err != nil {
					return nil, wrapORMError(err)
				}
				if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, recordDT, script.EventAfterInsert, doc, nil); hookErr != nil {
					return nil, hookErr
				}
				if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, recordDT, script.EventAfterSave, doc, nil); hookErr != nil {
					return nil, hookErr
				}
				insertedDocs = append(insertedDocs, doc)
			} else {
				beforeHash = CanonicalDocHash(oldDoc.Fields)
				if err := txMgr.SaveInTxWithScope(dbTx, recordDT, doc, user, scopeForOperation(reg, op, recordDT.Name, "write"), oldDoc); err != nil {
					return nil, wrapExpectedVersionORMError(err, op, k.dialect())
				}
				if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, recordDT, script.EventAfterSave, doc, oldDoc); hookErr != nil {
					return nil, hookErr
				}
				bundleSavedDocs[i].oldDoc = oldDoc
			}
			doc.IsNew = false
			afterHash := CanonicalDocHash(doc.Fields)
			row := k.buildAuditRow(op, def, opID, recordDT.Name, doc.Name, contract.StatusCompleted, "")
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
				result = &ResultData{Doctype: recordDT.Name, Name: doc.Name, Created: oldDoc == nil, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
			} else {
				related = append(related, RelatedRecordResult{Key: p.Records[i].Key, Doctype: recordDT.Name, Name: doc.Name, Created: oldDoc == nil})
			}
		}
		result.Related = related

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
		if err := k.rejectWorkflowStateOnCreate(ctx, dbTx, op, dt, reg, p.Data); err != nil {
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
		if verrs := validateOperationDocumentWithResolvers(txMgr, dt, doc, reg, nil, op.Context.BusinessReferenceValidator, op.Context.HistoricalSnapshotValidator); verrs.HasErrors() {
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
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterInsert, doc, nil); hookErr != nil {
			return nil, hookErr
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterSave, doc, nil); hookErr != nil {
			return nil, hookErr
		}
		doc.IsNew = false
		row := k.buildAuditRow(op, def, opID, dt.Name, doc.Name, contract.StatusCompleted, "")
		row.AfterHash = afterHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, nil, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		result = &ResultData{Doctype: dt.Name, Name: doc.Name, Created: true, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
		insertedDocs = append(insertedDocs, doc)

	case CommandRecordUpdate:
		var p RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid record.update payload")
		}
		if p.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "record.update missing name")
		}
		if err := k.rejectWorkflowStateMutation(ctx, dbTx, op, dt, reg, p.Data); err != nil {
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
		if err := k.rejectWorkflowStateChange(ctx, dbTx, op, dt, reg, oldDoc, doc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if verrs := validateOperationDocumentWithResolvers(txMgr, dt, doc, reg, oldDoc, op.Context.BusinessReferenceValidator, op.Context.HistoricalSnapshotValidator); verrs.HasErrors() {
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
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterSave, doc, oldDoc); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, def, opID, dt.Name, doc.Name, contract.StatusCompleted, "")
		row.BeforeHash = beforeHash
		row.AfterHash = afterHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		result = &ResultData{Doctype: dt.Name, Name: doc.Name, Created: false, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
		savedDoc, savedOldDoc = doc, oldDoc

	case CommandRecordDelete:
		var p RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid record.delete payload")
		}
		if p.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "record.delete missing name")
		}
		scope := scopeForOperation(reg, op, dt.Name, "delete")
		oldDoc, err := txMgr.GetDocWithScope(dt, p.Name, scope)
		if err != nil {
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
		}
		if err := txMgr.PrepareDelete(dt, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
			if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
				return nil, cerr
			}
		}
		beforeHash := CanonicalDocHash(oldDoc.Fields)
		if err := txMgr.DeleteInTxWithScope(dbTx, dt, p.Name, scope, oldDoc); err != nil {
			return nil, wrapORMError(err)
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterDelete, oldDoc, nil); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, def, opID, dt.Name, oldDoc.Name, contract.StatusCompleted, "")
		row.BeforeHash = beforeHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, nil); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		deletedDoc = oldDoc
		result = &ResultData{Doctype: dt.Name, Name: oldDoc.Name, Deleted: true, AuditID: auditID, Operation: opID}

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
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, script.EventAfterSave, doc, oldDoc); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, def, opID, dt.Name, doc.Name, contract.StatusCompleted, "")
		row.BeforeHash = beforeHash
		row.AfterHash = afterHash
		auditID, aerr := k.writeAudit(dbTx, row)
		if aerr != nil {
			return nil, contract.NewError(contract.CodeInternal, "audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record history write failed")
		}
		result = &ResultData{Doctype: dt.Name, Name: doc.Name, Document: doc.ToMap(), AuditID: auditID, Operation: opID}
		savedDoc, savedOldDoc = doc, oldDoc

	default:
		return nil, contract.NewError(contract.CodeValidationFailed, "unknown command "+op.Command)
	}
	if result != nil && op.Context.IdempotencyKey != "" && def.IdempotentByKey {
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, contract.NewError(contract.CodeInternal, "encoding command result failed")
		}
		if cerr := k.storeReceiptResult(dbTx, op, contract.CommandResult{
			OperationID: opID, CorrelationID: op.Context.CorrelationID,
			Status: contract.StatusCompleted, Data: data,
		}); cerr != nil {
			return nil, cerr
		}
	}

	nestedPostCommitHooks := append([]func(){}, txMgr.PostCommitHooks...)
	completionManager := *txMgr
	completionManager.ScriptMutationExecutor = nil
	completionManager.PostCommitHooks = nil
	completionManager.Transaction = nil
	complete := func() {
		for _, hook := range nestedPostCommitHooks {
			hook()
		}
		if deletedDoc != nil {
			completionManager.CompleteDelete(dt, deletedDoc)
		}
		for _, doc := range insertedDocs {
			completionManager.CompleteInsert(reg.Get(doc.DocType), doc)
		}
		for _, saved := range bundleSavedDocs {
			if saved.oldDoc != nil {
				completionManager.CompleteSave(saved.dt, saved.doc, saved.oldDoc)
			}
		}
		if savedDoc != nil {
			completionManager.CompleteSave(dt, savedDoc, savedOldDoc)
		}
	}
	if ownsTransaction {
		if err := dbTx.Commit(); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "commit failed")
		}
		txMgr.ScriptMutationExecutor = nil
		txMgr.PostCommitHooks = nil
		complete()
	} else if afterCommit != nil {
		*afterCommit = complete
	}
	return result, nil
}

func validateOperationDocumentWithResolvers(txMgr *orm.TxManager, dt *doctype.DocType, doc *doctype.Document, reg *doctype.Registry, oldDoc *doctype.Document, referenceResolver contract.BusinessReferenceValidator, snapshotValidator contract.HistoricalSnapshotValidator) doctype.ValidationErrors {
	resolver := func(target *doctype.DocType, name string) (*doctype.Document, error) {
		return txMgr.GetDoc(target, name, "")
	}
	return doctype.ValidateDocumentWithResolvers(dt, doc, reg, oldDoc, resolver, referenceResolver, snapshotValidator)
}

func resolveCreateBundleData(raw json.RawMessage, priorNames map[string]string) (json.RawMessage, error) {
	var values map[string]any
	if len(raw) == 0 {
		values = map[string]any{}
	} else if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("data must be a JSON object")
	}
	resolved, err := resolveBundleValue(values, priorNames)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("encode resolved data: %w", err)
	}
	return encoded, nil
}

func resolveBundleName(value string, names map[string]string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("bundle update name is required")
	}
	resolved, err := resolveBundleValue(value, names)
	if err != nil {
		return "", err
	}
	name, ok := resolved.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("bundle update name is invalid")
	}
	return name, nil
}

func resolveBundleValue(value any, priorNames map[string]string) (any, error) {
	switch current := value.(type) {
	case string:
		var resolutionErr error
		resolved := createBundleReference.ReplaceAllStringFunc(current, func(token string) string {
			if resolutionErr != nil {
				return token
			}
			parts := createBundleReference.FindStringSubmatch(token)
			if len(parts) != 2 {
				resolutionErr = fmt.Errorf("invalid record reference %q", token)
				return token
			}
			name, ok := priorNames[parts[1]]
			if !ok {
				resolutionErr = fmt.Errorf("record reference %q must point to an earlier bundle item", token)
				return token
			}
			return name
		})
		return resolved, resolutionErr
	case map[string]any:
		resolved := make(map[string]any, len(current))
		for key, nested := range current {
			value, err := resolveBundleValue(nested, priorNames)
			if err != nil {
				return nil, err
			}
			resolved[key] = value
		}
		return resolved, nil
	case []any:
		resolved := make([]any, len(current))
		for index, nested := range current {
			value, err := resolveBundleValue(nested, priorNames)
			if err != nil {
				return nil, err
			}
			resolved[index] = value
		}
		return resolved, nil
	default:
		return value, nil
	}
}

func setDefaultValues(dt *doctype.DocType, doc *doctype.Document) {
	doctype.ApplyDefaultValues(dt, doc)
}

func scopeForOperation(reg *doctype.Registry, op Operation, doctypeName, permission string) policy.RowScope {
	dt := reg.Get(doctypeName)
	decision, err := decideOperationResource(op, reg, dt, permission)
	if err != nil {
		return policy.RowScope{Business: op.Context.BusinessScope}
	}
	actor := op.Context.Execution.Actor()
	userID := actor.SubjectUserID
	if userID == "" {
		userID = actor.PrincipalID
	}
	return policy.RowScope{UserID: userID, Owner: decision.OwnerOnly, AssignedFields: append([]string(nil), decision.AssignedFields...), Business: op.Context.BusinessScope}
}

// cloneDoc copies a loaded document so mutation attempts never alias the
// previously persisted state.
func cloneDoc(d *doctype.Document) *doctype.Document {
	c := doctype.NewDocument(d.DocType)
	for k, v := range d.Fields {
		c.Fields[k] = v
	}
	c.Name = d.Name
	c.DocStatus = d.DocStatus
	c.Revision = d.Revision
	c.IsNew = false
	return c
}

// applyData merges JSON field data into a document, rejecting unknown keys —
// unsupported configuration is never silently discarded.
func applyData(dt *doctype.DocType, doc *doctype.Document, raw json.RawMessage, reg *doctype.Registry, skipReadOnly bool) error {
	var fields map[string]any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("data is not a JSON object")
	}
	for key, val := range fields {
		f := dt.GetField(key)
		if f == nil {
			return fmt.Errorf("unknown field %q on doctype %q", key, dt.Name)
		}
		if skipReadOnly && f.ReadOnly {
			continue
		}
		if f.Fieldtype == "Table" {
			rows, ok := val.([]any)
			if !ok {
				return fmt.Errorf("field %q must be an array of child rows", key)
			}
			childDT := reg.Get(f.Options)
			if childDT == nil {
				return fmt.Errorf("child doctype %q not found", f.Options)
			}
			children := make([]*doctype.Document, 0, len(rows))
			for index, value := range rows {
				row, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("field %q row %d must be an object", key, index)
				}
				child := doctype.NewDocument(f.Options)
				for childField, childValue := range row {
					child.Set(childField, childValue)
				}
				children = append(children, child)
			}
			doc.Set(key, children)
			continue
		}
		doc.Set(key, val)
	}
	return nil
}

func (k *Kernel) workflowStatusPolicy(ctx context.Context, tx *sql.Tx, op Operation, table string) (*workflow.StatusPolicy, bool, error) {
	if k == nil {
		return nil, false, nil
	}
	repository := k.WorkflowStatusPolicies
	if repository == nil && k.TxManager != nil {
		repository = k.TxManager.WorkflowStatusPolicies
	}
	if repository == nil {
		return nil, false, nil
	}
	if _, err := contract.ParseTenantID(string(op.Context.TenantID)); err != nil {
		return nil, false, contract.NewError(contract.CodeUnauthenticated, "workflow policy tenant is unavailable")
	}
	return repository.GetStatusPolicyInTx(ctx, tx, op.Context.TenantID, table)
}

func (k *Kernel) rejectWorkflowStateMutation(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, reg *doctype.Registry, raw json.RawMessage) error {
	if dt == nil || len(raw) == 0 {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	if _, supplied := fields[statusPolicy.StateField]; supplied {
		return fmt.Errorf("workflow-managed status field %q can only be changed through a status action", statusPolicy.StateField)
	}
	return nil
}

func (k *Kernel) rejectWorkflowStateOnCreate(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, reg *doctype.Registry, raw json.RawMessage) error {
	if dt == nil || len(raw) == 0 {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if statusPolicy.InitialState == "" {
		return fmt.Errorf("workflow-managed status field %q has no configured initial value", statusPolicy.StateField)
	}
	field := dt.GetField(statusPolicy.StateField)
	if field == nil {
		return fmt.Errorf("workflow status field %q is not configured on DocType %s", statusPolicy.StateField, dt.Name)
	}
	initial := statusPolicy.InitialState
	if defaultState := strings.TrimSpace(field.Default); defaultState != "" && defaultState != initial {
		return fmt.Errorf("DocType default for workflow status field %q must match initial value %q", statusPolicy.StateField, initial)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	value, supplied := fields[statusPolicy.StateField]
	if !supplied {
		return nil
	}
	var state string
	if err := json.Unmarshal(value, &state); err != nil || state != initial {
		return fmt.Errorf("new records must start in initial workflow status %q; later values require a status action", initial)
	}
	return nil
}

func (k *Kernel) rejectWorkflowStateChange(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, reg *doctype.Registry, before, after *doctype.Document) error {
	if dt == nil || before == nil || after == nil {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if fmt.Sprint(before.Get(statusPolicy.StateField)) != fmt.Sprint(after.Get(statusPolicy.StateField)) {
		return fmt.Errorf("workflow-managed status field %q can only be changed through a status action", statusPolicy.StateField)
	}
	return nil
}

func (k *Kernel) initializeWorkflowStateOnCreate(ctx context.Context, tx *sql.Tx, op Operation, dt *doctype.DocType, doc *doctype.Document) error {
	if dt == nil || doc == nil {
		return nil
	}
	statusPolicy, found, err := k.workflowStatusPolicy(ctx, tx, op, dt.Name)
	if err != nil || !found {
		return err
	}
	if statusPolicy.InitialState == "" {
		return fmt.Errorf("workflow status field %q has no configured initial value", statusPolicy.StateField)
	}
	if dt.GetField(statusPolicy.StateField) == nil {
		return fmt.Errorf("workflow status field %q is not configured on DocType %s", statusPolicy.StateField, dt.Name)
	}
	stateName := strings.TrimSpace(fmt.Sprint(doc.Get(statusPolicy.StateField)))
	if stateName == "<nil>" || stateName == "" {
		doc.Set(statusPolicy.StateField, statusPolicy.InitialState)
	} else if stateName != statusPolicy.InitialState {
		return fmt.Errorf("new records must start in initial workflow status %q; later values require a status action", statusPolicy.InitialState)
	}
	return nil
}

// eventOrigin carries the operation's authenticated cause onto the change
// events its writes append.
func eventOrigin(op Operation) *orm.EventOrigin {
	return &orm.EventOrigin{Actor: op.Context.Actor, CorrelationID: op.Context.CorrelationID, CausationID: op.Context.CausationID, CausationDepth: op.Context.CausationDepth}
}

func validationContractError(verrs doctype.ValidationErrors) *contract.Error {
	raw, _ := json.Marshal(verrs)
	e := contract.NewError(contract.CodeValidationFailed, "validation failed")
	e.Details = raw
	return e
}

// wrapORMError converts ORM sentinel errors into typed contract errors.
func wrapORMError(err error) *contract.Error {
	if errors.Is(err, orm.ErrConflict) {
		return ErrStaleVersion
	}
	switch {
	case errors.Is(err, orm.ErrDuplicate):
		return contract.NewError(contract.CodeConflict, sanitize(err.Error()))
	case errors.Is(err, orm.ErrValidation):
		return contract.NewError(contract.CodeValidationFailed, "record validation failed")
	case errors.Is(err, orm.ErrNotFound):
		return contract.NewError(contract.CodeNotFound, "document not found or access denied")
	default:
		return contract.NewError(contract.CodeDependencyUnavailable, "record persistence failed")
	}
}

func wrapExpectedVersionORMError(err error, op Operation, dialect db.Dialect) *contract.Error {
	if expectedVersionFrom(op) != "" && dialect.IsWriteConflict(err) {
		return ErrStaleVersion
	}
	return wrapORMError(err)
}

// sanitize strips SQL driver internals from user-facing messages while keeping
// the meaningful suffix.
func sanitize(msg string) string {
	if i := strings.LastIndex(msg, ": "); i > 0 && strings.Count(msg, ": ") > 1 {
		return msg[i+2:]
	}
	return msg
}

func (k *Kernel) rejected(opID string, op Operation, e *contract.Error) (contract.CommandResult, *contract.Error) {
	return contract.CommandResult{
		OperationID:   opID,
		CorrelationID: op.Context.CorrelationID,
		Status:        statusFor(e.Type),
		Error:         e,
	}, e
}

func statusFor(c contract.Code) contract.Status {
	switch c {
	case contract.CodeConflict:
		return contract.StatusConflict
	case contract.CodePermissionDenied, contract.CodeUnauthenticated,
		contract.CodeValidationFailed, contract.CodeNotFound, contract.CodeIdempotencyKeyReused:
		return contract.StatusRejected
	default:
		return contract.StatusFailed
	}
}

// expectedVersionFrom reads the optimistic-concurrency token from the
// envelope-level convention: Context.CausationID prefixed "expected:".
// A dedicated envelope field lands with SPEC-003's canonical envelope work;
// absence of the token preserves backward compatibility (no guard).
func expectedVersionFrom(op Operation) string {
	if strings.TrimSpace(op.Context.ExpectedVersion) != "" {
		return strings.TrimSpace(op.Context.ExpectedVersion)
	}
	if strings.HasPrefix(op.Context.CausationID, "expected:") {
		return strings.TrimPrefix(op.Context.CausationID, "expected:")
	}
	return ""
}

// CanonicalVersion renders a document version token (the `modified`
// timestamp) in a stable string form so callers can echo it back as
// expected_version regardless of driver time parsing.
func CanonicalVersion(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		if parsed, err := parseVersionTimestamp(t); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return t
	case []byte:
		return CanonicalVersion(string(t))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseVersionTimestamp(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid version timestamp %q", value)
}

// CanonicalDocHash renders a document's field map as sorted-key JSON and
// returns the sha256 hex digest — the audit before/after state token
// (KERNEL-009). Hash input is deterministic: sorted keys, JSON-encoded values.
func CanonicalDocHash(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(fields[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func payloadHash(raw json.RawMessage) string {
	canonical, err := canonicalJSON(raw)
	if err == nil {
		raw = canonical
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func rawPayloadHash(raw json.RawMessage) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}

func resultHash(r contract.CommandResult) string {
	raw, _ := json.Marshal(struct {
		Status contract.Status `json:"status"`
		Data   json.RawMessage `json:"data,omitempty"`
		Error  *contract.Error `json:"error,omitempty"`
	}{r.Status, r.Data, r.Error})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

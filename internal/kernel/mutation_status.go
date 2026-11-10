package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/workflow"
)

// transition moves a record to the next workflow state through a published
// status action.
func (m *mutation) transition(ctx context.Context) *contract.Error {
	var p WorkflowStatusTransitionPayload
	if err := json.Unmarshal(m.op.Payload, &p); err != nil || p.Name == "" || p.Action == "" {
		return contract.NewError(contract.CodeValidationFailed, "workflow.status_transition requires name and action")
	}
	if cerr := m.fenceConfigRevision(ctx); cerr != nil {
		return cerr
	}
	statusPolicy, cerr := m.statusPolicy(ctx)
	if cerr != nil {
		return cerr
	}
	scope := scopeForOperation(m.run.Reg, m.op, m.dt.Name, "submit")
	oldDoc, cerr := m.load(p.Name, scope) //nolint:contextcheck // the context travels in txMgr.Context
	if cerr != nil {
		return cerr
	}
	if p.ExpectedRevision > 0 && oldDoc.Revision != p.ExpectedRevision {
		m.auditFailure(ErrStaleVersion.Type)
		return ErrStaleVersion
	}
	if versionErr := m.checkVersion(p.Name); versionErr != nil {
		return versionErr
	}
	doc, cerr := m.applyAction(statusPolicy, oldDoc, p.Action) //nolint:contextcheck // the context travels in txMgr.Context
	if cerr != nil {
		return cerr
	}
	return m.saveTransition(ctx, doc, oldDoc, scope)
}

// fenceConfigRevision locks the tenant configuration revision the transition
// was prepared against.
func (m *mutation) fenceConfigRevision(ctx context.Context) *contract.Error {
	if m.op.Context.ExpectedConfigRevision <= 0 {
		return nil
	}
	err := configstore.FenceConfigRevisionTx(ctx, m.run.Tx, m.k.dialect(), m.op.Context.ExpectedConfigRevision)
	if err == nil {
		return nil
	}
	if errors.Is(err, configstore.ErrConfigRevisionConflict) {
		m.auditFailure(ErrStaleVersion.Type)
		return contract.NewError(contract.CodeConflict, "tenant configuration changed while the workflow transition was being prepared")
	}
	return contract.NewError(contract.CodeDependencyUnavailable, "lock workflow configuration revision failed")
}

// statusPolicy returns the published status policy of the record's table.
func (m *mutation) statusPolicy(ctx context.Context) (*workflow.StatusPolicy, *contract.Error) {
	statusPolicy, found, err := m.k.workflowStatusPolicy(ctx, m.run.Tx, m.op, m.dt.Name)
	if err != nil {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "load workflow status policy failed")
	}
	if !found || statusPolicy == nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "no published status transition policy for table")
	}
	if m.dt.GetField(statusPolicy.StateField) == nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow state field is not defined on doctype")
	}
	return statusPolicy, nil
}

// actingRoles lists the roles the transition is judged against: the caller's
// roles, with the primary role first when it is not among them.
func actingRoles(op Operation) []string {
	roles := append([]string(nil), op.Context.Roles...)
	if op.Context.UserRole == "" {
		return roles
	}
	for _, role := range roles {
		if role == op.Context.UserRole {
			return roles
		}
	}
	return append([]string{op.Context.UserRole}, roles...)
}

// applyAction returns a copy of the record in the state the action leads to.
func (m *mutation) applyAction(statusPolicy *workflow.StatusPolicy, oldDoc *doctype.Document, action string) (*doctype.Document, *contract.Error) {
	stateField := statusPolicy.StateField
	currentState := fmt.Sprint(oldDoc.Get(stateField))
	if currentState == "<nil>" || currentState == "" {
		currentState = statusPolicy.InitialState
	}
	roles := actingRoles(m.op)
	if len(roles) == 0 {
		return nil, contract.NewError(contract.CodePermissionDenied, "workflow transition requires an assigned role")
	}
	newState, newStatus, err := workflow.ApplyStatusTransition(statusPolicy, currentState, action, roles, oldDoc)
	if err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	doc := cloneDoc(oldDoc)
	doc.Set(stateField, newState)
	doc.DocStatus = newStatus
	return doc, nil
}

// saveTransition persists the transitioned record with its audit and history
// rows.
func (m *mutation) saveTransition(ctx context.Context, doc, oldDoc *doctype.Document, scope policy.RowScope) *contract.Error {
	if err := m.run.Mgr.PrepareSave(m.dt, doc, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if claimErr := m.claim(); claimErr != nil {
		return claimErr
	}
	beforeHash := CanonicalDocHash(oldDoc.Fields)
	afterHash := CanonicalDocHash(doc.Fields)
	if err := m.run.Mgr.SaveInTxWithScope(m.run.Tx, m.dt, doc, m.user, scope, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapORMError(err)
	}
	if hookErr := captureLifecycleHooks(ctx, m.run.Mgr, m.run.Tx, m.dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
		return hookErr
	}
	row := m.k.buildAuditRow(m.op, m.def, m.run.OpID, auditOutcome{Doctype: m.dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
	row.BeforeHash = beforeHash
	row.AfterHash = afterHash
	auditID, cerr := m.record(row, oldDoc, doc)
	if cerr != nil {
		return cerr
	}
	m.result = &ResultData{Doctype: m.dt.Name, Name: doc.Name, Document: doc.ToMap(), AuditID: auditID, Operation: m.run.OpID}
	m.savedDoc, m.savedOldDoc = doc, oldDoc
	return nil
}

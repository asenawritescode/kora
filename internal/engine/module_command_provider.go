package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/plugin"
)

// newKernelCommandProviderFactory adapts the generation's existing
// CommandRegistry to the capability provider seam. The registry remains the
// source of command definitions; the capability metadata on each definition
// links one local operation to that command.
func newKernelCommandProviderFactory(f kernelCommandProviderFactory) (plugin.ProviderFactory, error) {
	if f.database == nil || f.registry == nil || f.commands == nil || f.engine == nil || f.owner == "" || f.capability == "" || f.validateSchema == nil {
		return nil, fmt.Errorf("kernel command provider dependencies are required")
	}
	if _, err := contract.ParseTenantID(string(f.tenant)); err != nil {
		return nil, fmt.Errorf("kernel command provider tenant: %w", err)
	}
	return &f, nil
}

type kernelCommandProviderFactory struct {
	database       *sql.DB
	registry       *doctype.Registry
	commands       *kernel.CommandRegistry
	engine         *kernel.Kernel
	tenant         contract.TenantID
	owner          contract.ModuleID
	capability     contract.CapabilityName
	validateSchema func(contract.SchemaRef, []byte) error
}

func (f *kernelCommandProviderFactory) Capability() contract.CapabilityName { return f.capability }

func (f *kernelCommandProviderFactory) Open(ctx contract.OperationContext) (contract.CapabilityHandle, error) {
	if ctx.Scope.TenantID != f.tenant || ctx.Scope.Validate() != nil || ctx.Site != string(f.tenant) {
		return nil, fmt.Errorf("kernel command provider scope does not match its tenant")
	}
	return &kernelCommandProviderHandle{factory: f, scope: ctx.Scope, opened: time.Now().UTC()}, nil
}

func (f *kernelCommandProviderFactory) OpenReferenceResolver(ctx context.Context, operation contract.OperationContext) (contract.BusinessReferenceProviderHandle, error) {
	if ctx == nil || operation.Scope.TenantID != f.tenant || operation.Scope.Validate() != nil || operation.Site != string(f.tenant) {
		return nil, fmt.Errorf("kernel command reference provider scope does not match its tenant")
	}
	return &kernelCommandProviderHandle{factory: f, scope: operation.Scope, opened: time.Now().UTC()}, nil
}

type kernelCommandProviderHandle struct {
	factory *kernelCommandProviderFactory
	scope   contract.BusinessScope
	opened  time.Time
	revoked bool
}

func kernelCommandPayload(input []byte) json.RawMessage {
	payload := append([]byte(`{"data":`), input...)
	return append(payload, '}')
}

func (h *kernelCommandProviderHandle) Name() contract.CapabilityName { return h.factory.capability }
func (*kernelCommandProviderHandle) Scope() contract.GrantScope      { return contract.GrantScopeOperation }
func (h *kernelCommandProviderHandle) Revoke()                       { h.revoked = true }
func (h *kernelCommandProviderHandle) Evidence() contract.CleanupEvidence {
	evidence := contract.CleanupEvidence{Owner: string(h.factory.owner), ScopeID: string(h.scope.TenantID), AcquiredAt: h.opened}
	if h.revoked {
		evidence.ReleasedAt = time.Now().UTC()
	}
	return evidence
}

// Resolve maps the provider's stable public resource identity to its owned
// local row under the exact selected tenant or legal-entity scope. Resource
// names and identity fields are module metadata; no companion DocType is
// installed for the consumer.
func (h *kernelCommandProviderHandle) Resolve(ctx context.Context, scope contract.ScopeContext, ref contract.BusinessRef) (contract.ResolvedBusinessRef, error) {
	if ctx == nil || h == nil || h.factory == nil || h.revoked || scope.Scope != h.scope || scope.ProviderID == "" || ref.Scope != h.scope || ref.ProviderID != scope.ProviderID {
		return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodePermissionDenied, "internal business reference scope is invalid")
	}
	var target *doctype.DocType
	for _, candidate := range h.factory.registry.All() {
		if candidate != nil && candidate.BusinessResourceType == ref.ResourceType && candidate.ModuleOwnerID == string(h.factory.owner) && candidate.ModuleAccess == contract.ModulePublished && candidate.BusinessIdentityField != "" {
			if target != nil {
				return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeConflict, "published resource identity is ambiguous")
			}
			target = candidate
		}
	}
	if target == nil || target.GetField(target.BusinessIdentityField) == nil || !target.GetField(target.BusinessIdentityField).Unique || contract.ValidateResourceBusinessScope(h.scope, target.BusinessScopePolicy) != nil {
		return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeNotFound, "published business reference resource is unavailable")
	}
	dialect := h.factory.engine.Dialect
	query := "SELECT name FROM " + dialect.QuoteIdent(target.RawTableName()) + " WHERE " + dialect.QuoteIdent(target.BusinessIdentityField) + "=?"
	args := []any{ref.ID}
	if target.BusinessScopePolicy == contract.ScopeEntityOwned {
		if target.BusinessScopeField == "" || target.GetField(target.BusinessScopeField) == nil {
			return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodePermissionDenied, "published resource has no entity scope field")
		}
		query += " AND " + dialect.QuoteIdent(target.BusinessScopeField) + "=?"
		args = append(args, h.scope.LegalEntityID)
	}
	rows, err := h.factory.database.QueryContext(ctx, db.Rebind(dialect, query), args...)
	if err != nil {
		return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeDependencyUnavailable, "published reference lookup failed")
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var rowName string
		if err := rows.Scan(&rowName); err != nil {
			return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeDependencyUnavailable, "published reference lookup failed")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeDependencyUnavailable, "published reference lookup failed")
	}
	if count == 0 {
		return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeNotFound, "published business reference was not found in this scope")
	}
	if count > 1 {
		return contract.ResolvedBusinessRef{}, contract.NewError(contract.CodeConflict, "published business identity is not unique")
	}
	return contract.ResolvedBusinessRef{Canonical: ref}, nil
}

func (h *kernelCommandProviderHandle) InvokeCapability(ctx context.Context, invocation plugin.CapabilityInvocation) (plugin.CapabilityInvocationResult, error) {
	if h == nil || h.factory == nil || h.revoked || ctx == nil || !invocation.Execution.Valid() || invocation.Execution.Tenant() != h.scope.TenantID || invocation.Context.Scope != h.scope || invocation.ModuleExecution.Scope != h.scope || !invocation.ModuleExecution.Verified() || invocation.ModuleExecution.CallerModuleID == "" || invocation.ModuleExecution.OwnerModuleID != h.factory.owner || invocation.ModuleExecution.BindingRevision == 0 || invocation.ModuleExecution.AllowedOperation != invocation.Operation.Name || invocation.Operation.Effect != contract.EffectLocal {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodePermissionDenied, "internal capability invocation identity is invalid")
	}
	command, ok := h.factory.commands.LookupCapability(h.factory.capability, invocation.Operation.Name)
	if !ok || command.ModuleOwnerID != h.factory.owner || command.Capability != h.factory.capability || command.Operation != invocation.Operation.Name || command.InputSchema != invocation.Operation.Input || command.OutputSchema != invocation.Operation.Output {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodePermissionDenied, "local capability operation has no matching owner command contract")
	}
	if err := h.factory.validateSchema(command.InputSchema, invocation.Payload); err != nil {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodeValidationFailed, "owner command input failed its registered schema")
	}
	moduleExecution, err := contract.NewCommandModuleExecution(h.scope, invocation.ModuleExecution.CallerModuleID, h.factory.owner, invocation.ModuleExecution.BindingRevision, command.FullName(), command.FullName())
	if err != nil {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodePermissionDenied, "owner command provenance could not be minted")
	}
	actor := invocation.Execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	primaryRole := ""
	if len(actor.Roles) > 0 {
		primaryRole = actor.Roles[0]
	}
	operation := kernel.Operation{Command: command.FullName(), Payload: kernelCommandPayload(invocation.Payload), Context: kernel.OperationContext{
		ModuleExecution: &moduleExecution, BusinessScope: h.scope, BusinessReferenceValidator: invocation.BusinessReferenceValidator, Site: actor.Site, TenantID: invocation.Execution.Tenant(),
		Execution: invocation.Execution, Actor: actor, User: user, Owner: user, UserRole: primaryRole, Roles: append([]string(nil), actor.Roles...),
		Source: kernel.SourceWorker, ExpectedVersion: string(invocation.Meta.ExpectedRevision), IdempotencyKey: string(invocation.Meta.IdempotencyKey),
	}}
	tx, err := h.factory.database.BeginTx(ctx, nil)
	if err != nil {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodeDependencyUnavailable, "begin local capability transaction failed")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result, afterCommit, commandErr := h.factory.engine.ExecuteInTx(ctx, h.factory.database, tx, h.factory.registry, operation)
	if commandErr != nil {
		return plugin.CapabilityInvocationResult{}, commandErr
	}
	if err := h.factory.validateSchema(command.OutputSchema, result.Data); err != nil {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodeValidationFailed, "owner command output failed its registered schema")
	}
	if err := tx.Commit(); err != nil {
		return plugin.CapabilityInvocationResult{}, contract.NewError(contract.CodeOutcomeUnknown, "local capability transaction commit outcome is unknown")
	}
	committed = true
	if afterCommit != nil {
		afterCommit()
	}
	return plugin.CapabilityInvocationResult{ExecutionID: contract.OperationID(result.OperationID), Output: append(json.RawMessage(nil), result.Data...)}, nil
}

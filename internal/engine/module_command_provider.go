package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/plugin"
)

// newKernelCommandProviderFactory adapts the generation's existing
// CommandRegistry to the capability provider seam. The registry remains the
// source of command definitions; the capability metadata on each definition
// links one local operation to that command.
func newKernelCommandProviderFactory(database *sql.DB, registry *doctype.Registry, commandRegistry *kernel.CommandRegistry, commandKernel *kernel.Kernel, tenant contract.TenantID, owner contract.ModuleID, capability contract.CapabilityName, validateSchema func(contract.SchemaRef, []byte) error) (plugin.ProviderFactory, error) {
	if database == nil || registry == nil || commandRegistry == nil || commandKernel == nil || owner == "" || capability == "" || validateSchema == nil {
		return nil, fmt.Errorf("kernel command provider dependencies are required")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("kernel command provider tenant: %w", err)
	}
	return &kernelCommandProviderFactory{database: database, registry: registry, commands: commandRegistry, engine: commandKernel, tenant: tenant, owner: owner, capability: capability, validateSchema: validateSchema}, nil
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

type kernelCommandProviderHandle struct {
	factory *kernelCommandProviderFactory
	scope   contract.BusinessScope
	opened  time.Time
	revoked bool
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
	operation := kernel.Operation{Command: command.FullName(), Payload: append(json.RawMessage(nil), invocation.Payload...), Context: kernel.OperationContext{
		ModuleExecution: &moduleExecution, BusinessScope: h.scope, Site: actor.Site, TenantID: invocation.Execution.Tenant(),
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

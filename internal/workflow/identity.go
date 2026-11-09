package workflow

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
)

// ExecutionIdentityProvider is owned by runtime assembly. It must return a
// verified service identity for the claimed tenant; workflow JSON and the
// original human starter are never identity sources for action execution.
type ExecutionIdentityProvider interface {
	ForWorkflow(context.Context, contract.TenantID, contract.InstanceID) (policy.Execution, error)
	RequireActionGrants(context.Context, contract.TenantID, []policy.Grant) error
}

// TenantServiceIdentityProvider binds one runtime generation to a configured
// workflow service identity. Construct one per tenant runtime after the
// assembly layer has validated the service principal and its narrow grants.
type TenantServiceIdentityProvider struct {
	tenant    contract.TenantID
	execution policy.Execution
}

func NewTenantServiceIdentityProvider(tenant contract.TenantID, actor contract.ActorContext, grants []policy.Grant) (*TenantServiceIdentityProvider, error) {
	parsedTenant, err := contract.ParseTenantID(string(tenant))
	if err != nil {
		return nil, fmt.Errorf("workflow service identity tenant: %w", err)
	}
	if actor.PrincipalType != contract.PrincipalService || actor.SubjectUserID != "" || actor.DelegationID != "" {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow execution identity must be a non-delegated service principal")
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(parsedTenant, actor, grants)
	if err != nil {
		return nil, fmt.Errorf("workflow service execution identity: %w", err)
	}
	return &TenantServiceIdentityProvider{tenant: parsedTenant, execution: execution}, nil
}

func (p *TenantServiceIdentityProvider) ForWorkflow(ctx context.Context, tenant contract.TenantID, instance contract.InstanceID) (policy.Execution, error) {
	if p == nil || !p.execution.Valid() {
		return policy.Execution{}, contract.NewError(contract.CodeUnauthenticated, "workflow service identity is unavailable")
	}
	if ctx == nil {
		return policy.Execution{}, fmt.Errorf("workflow identity lookup requires context")
	}
	if err := ctx.Err(); err != nil {
		return policy.Execution{}, err
	}
	if tenant != p.tenant {
		return policy.Execution{}, contract.NewError(contract.CodePermissionDenied, "workflow execution identity tenant mismatch")
	}
	if _, err := contract.ParseInstanceID(string(instance)); err != nil {
		return policy.Execution{}, contract.NewError(contract.CodeValidationFailed, "workflow execution instance identity is invalid")
	}
	return p.execution, nil
}

// RequireActionGrants checks the immutable tenant service identity at runtime
// assembly. Action registrations are trusted assembly data; definitions cannot
// add grants to this set.
func (p *TenantServiceIdentityProvider) RequireActionGrants(ctx context.Context, tenant contract.TenantID, grants []policy.Grant) error {
	if p == nil || !p.execution.Valid() || ctx == nil {
		return contract.NewError(contract.CodeUnauthenticated, "workflow execution identity is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant != p.tenant || p.execution.Tenant() != tenant {
		return contract.NewError(contract.CodePermissionDenied, "workflow execution identity tenant mismatch")
	}
	for _, grant := range grants {
		if !p.execution.HasGrant(grant) {
			return contract.NewError(contract.CodePermissionDenied, "workflow service identity lacks a registered action grant")
		}
	}
	return nil
}

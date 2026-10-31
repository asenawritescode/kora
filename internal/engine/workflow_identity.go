package engine

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
)

type scopedWorkflowIdentity struct {
	delegate workflow.ExecutionIdentityProvider
	tenant   contract.TenantID
	site     string
	grants   []policy.Grant
}

func newScopedWorkflowIdentity(delegate workflow.ExecutionIdentityProvider, tenant contract.TenantID, site string, grants []policy.Grant) (workflow.ExecutionIdentityProvider, error) {
	if delegate == nil || site == "" {
		return nil, fmt.Errorf("compatibility workflow identity and tenant route are required")
	}
	parsedTenant, err := contract.ParseTenantID(string(tenant))
	if err != nil {
		return nil, err
	}
	grantCopy := append([]policy.Grant(nil), grants...)
	for _, grant := range grantCopy {
		if !grant.Valid() {
			return nil, contract.NewError(contract.CodeValidationFailed, "workflow service grant is invalid")
		}
	}
	return &scopedWorkflowIdentity{delegate: delegate, tenant: parsedTenant, site: site, grants: grantCopy}, nil
}

func (p *scopedWorkflowIdentity) ForWorkflow(ctx context.Context, tenant contract.TenantID, instance contract.InstanceID) (policy.Execution, error) {
	if p == nil || ctx == nil || tenant != p.tenant {
		return policy.Execution{}, contract.NewError(contract.CodePermissionDenied, "workflow identity tenant binding is invalid")
	}
	if _, err := contract.ParseInstanceID(string(instance)); err != nil {
		return policy.Execution{}, contract.NewError(contract.CodeValidationFailed, "workflow instance identity is invalid")
	}
	delegated, err := p.delegate.ForWorkflow(ctx, tenant, instance)
	if err != nil {
		return policy.Execution{}, err
	}
	actor := delegated.Actor()
	if !delegated.Valid() || delegated.Tenant() != p.tenant || actor.PrincipalType != contract.PrincipalService || actor.PrincipalID == "" || actor.SubjectUserID != "" || actor.DelegationID != "" || len(actor.Roles) != 0 || actor.Site != p.site {
		return policy.Execution{}, contract.NewError(contract.CodeUnauthenticated, "workflow identity provider returned an invalid service actor")
	}
	return policy.NewVerifiedExecutionWithGrants(p.tenant, actor, p.grants)
}

func (p *scopedWorkflowIdentity) RequireActionGrants(ctx context.Context, tenant contract.TenantID, required []policy.Grant) error {
	if p == nil || ctx == nil || tenant != p.tenant {
		return contract.NewError(contract.CodePermissionDenied, "workflow identity tenant binding is invalid")
	}
	for _, grant := range required {
		if !grant.Valid() || !hasWorkflowGrant(p.grants, grant) {
			return contract.NewError(contract.CodePermissionDenied, "workflow service identity lacks a registered action grant")
		}
	}
	if err := p.delegate.RequireActionGrants(ctx, tenant, required); err != nil {
		return err
	}
	_, err := p.ForWorkflow(ctx, tenant, contract.InstanceID(contract.NewID()))
	return err
}

func hasWorkflowGrant(grants []policy.Grant, required policy.Grant) bool {
	for _, grant := range grants {
		if grant.Resource == required.Resource && grant.Operation == required.Operation && grant.OwnerOnly == required.OwnerOnly {
			return true
		}
	}
	return false
}

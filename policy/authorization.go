package policy

import "github.com/asenawritescode/kora/contract"

// RolePolicy is the transport-neutral view of the tenant's role permission
// matrix. Persistence and DocType packages implement it at the boundary.
type RolePolicy interface {
	CanUser(roles []string, resource, operation string) (allowed bool, ownerOnly bool)
}

type Decision struct {
	Allowed        bool
	OwnerOnly      bool
	AssignedFields []string
}

// RowScope describes the record-level visibility granted to an actor. Owner
// and assignment scopes are combined with OR; an empty scope is unrestricted.
type RowScope struct {
	UserID         string
	Owner          bool
	AssignedFields []string
	Business       contract.BusinessScope
}

// DecideForBusinessScope is the authorization gate for entity-owned resources.
// Entity operations require a narrow grant bound to that exact tenant/entity;
// tenant roles and unscoped service grants do not carry over.
func DecideForBusinessScope(execution Execution, roles RolePolicy, resource, operation string, scope contract.BusinessScope, entityOwned bool) (Decision, *contract.Error) {
	if private, ok := roles.(interface{ IsPrivateModuleResource(string) bool }); ok && private.IsPrivateModuleResource(resource) && isReadOperation(operation) {
		return Decision{}, contract.NewError(contract.CodePermissionDenied, "private module resources require an owner-controlled query surface")
	}
	if !entityOwned {
		return Decide(execution, roles, resource, operation)
	}
	if !execution.Valid() || scope.Kind != contract.ScopeKindLegalEntity || scope.Validate() != nil || scope.TenantID != execution.Tenant() {
		return Decision{}, contract.NewError(contract.CodePermissionDenied, "verified legal entity scope is required")
	}
	grant, ok := execution.GrantForScope(resource, operation, scope)
	if ok {
		return Decision{Allowed: true, OwnerOnly: grant.OwnerOnly}, nil
	}
	if scoped, ok := roles.(BusinessScopedRolePolicy); ok {
		allowed, ownerOnly, fields := scoped.CanUserInBusinessScope(execution.Actor().Roles, resource, operation, scope)
		if allowed {
			return Decision{Allowed: true, OwnerOnly: ownerOnly, AssignedFields: append([]string(nil), fields...)}, nil
		}
	}
	return Decision{}, contract.NewError(contract.CodePermissionDenied, "not permitted for legal entity")
}

// AuthorizeCrossEntityOperation requires the same registered command grant
// independently on both legal entities. Tenant roles and tenant-wide grants
// never imply either side of the operation.
func AuthorizeCrossEntityOperation(execution Execution, operation contract.CrossEntityOperation, rolePolicies ...RolePolicy) *contract.Error {
	if !execution.Valid() || operation.Validate() != nil || execution.Tenant() != operation.TenantID {
		return contract.NewError(contract.CodePermissionDenied, "explicit cross-entity operation is invalid")
	}
	if len(rolePolicies) > 1 {
		return contract.NewError(contract.CodeValidationFailed, "cross-entity authorization accepts at most one role policy")
	}
	source := contract.BusinessScope{TenantID: operation.TenantID, Kind: contract.ScopeKindLegalEntity, LegalEntityID: operation.SourceEntityID}
	target := contract.BusinessScope{TenantID: operation.TenantID, Kind: contract.ScopeKindLegalEntity, LegalEntityID: operation.TargetEntityID}
	if !hasCrossEntityGrant(execution, rolePolicy(rolePolicies), operation, source) {
		return contract.NewError(contract.CodePermissionDenied, "source legal entity grant is required")
	}
	if !hasCrossEntityGrant(execution, rolePolicy(rolePolicies), operation, target) {
		return contract.NewError(contract.CodePermissionDenied, "target legal entity grant is required")
	}
	return nil
}

func rolePolicy(policies []RolePolicy) RolePolicy {
	if len(policies) == 0 {
		return nil
	}
	return policies[0]
}

func hasCrossEntityGrant(execution Execution, roles RolePolicy, operation contract.CrossEntityOperation, scope contract.BusinessScope) bool {
	if _, ok := execution.GrantForScope(operation.Resource, operation.Operation, scope); ok {
		return true
	}
	_, err := DecideForBusinessScope(execution, roles, operation.Resource, operation.Operation, scope, true)
	return err == nil
}

// ScopedRolePolicy exposes the full row scope while RolePolicy retains its
// legacy interface for callers that only need an allow/deny decision.
type ScopedRolePolicy interface {
	RowScopeFor(roles []string, resource, operation string) (owner bool, assignedFields []string)
}

// BusinessScopedRolePolicy can grant access only when a permission itself is
// explicitly bound to the requested legal entity. Tenant role membership is
// never enough for entity-owned records.
type BusinessScopedRolePolicy interface {
	CanUserInBusinessScope(roles []string, resource, operation string, scope contract.BusinessScope) (allowed bool, ownerOnly bool, assignedFields []string)
}

// Decide evaluates one resource operation from verified identity and the
// current tenant role policy. Caller-populated actor fields are never inputs.
func Decide(execution Execution, roles RolePolicy, resource, operation string) (Decision, *contract.Error) {
	if private, ok := roles.(interface{ IsPrivateModuleResource(string) bool }); ok && private.IsPrivateModuleResource(resource) && isReadOperation(operation) {
		return Decision{}, contract.NewError(contract.CodePermissionDenied, "private module resources require an owner-controlled query surface")
	}
	if !execution.Valid() {
		return Decision{}, contract.NewError(contract.CodeUnauthenticated, "verified execution identity is required")
	}
	if grant, ok := execution.GrantFor(resource, operation); ok {
		return Decision{Allowed: true, OwnerOnly: grant.OwnerOnly}, nil
	}
	actor := execution.Actor()
	if len(actor.Roles) == 0 || roles == nil {
		return Decision{}, contract.NewError(contract.CodePermissionDenied, "not permitted")
	}
	allowed, ownerOnly := roles.CanUser(actor.Roles, resource, operation)
	if !allowed {
		return Decision{}, contract.NewError(contract.CodePermissionDenied, "not permitted")
	}
	decision := Decision{Allowed: true, OwnerOnly: ownerOnly}
	if scoped, ok := roles.(ScopedRolePolicy); ok {
		decision.OwnerOnly, decision.AssignedFields = scoped.RowScopeFor(actor.Roles, resource, operation)
	}
	return decision, nil
}

func isReadOperation(operation string) bool {
	switch operation {
	case "read", "report", "export":
		return true
	default:
		return false
	}
}

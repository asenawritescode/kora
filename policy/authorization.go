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
}

// ScopedRolePolicy exposes the full row scope while RolePolicy retains its
// legacy interface for callers that only need an allow/deny decision.
type ScopedRolePolicy interface {
	RowScopeFor(roles []string, resource, operation string) (owner bool, assignedFields []string)
}

// Decide evaluates one resource operation from verified identity and the
// current tenant role policy. Caller-populated actor fields are never inputs.
func Decide(execution Execution, roles RolePolicy, resource, operation string) (Decision, *contract.Error) {
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

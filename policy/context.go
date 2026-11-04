// Package policy contains transport-neutral identity and authorization
// contracts. It does not decide business-domain transitions.
package policy

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/asenawritescode/kora/contract"
)

// Execution is a validated, tenant-bound identity. Its private fields prevent
// construction by decoding caller-controlled JSON. Authentication adapters
// create values only after validating a session or scoped credential.
type Execution struct {
	tenant contract.TenantID
	actor  contract.ActorContext
	grants []Grant
}

// Grant is a narrow, verified operation grant issued by a credential
// authentication adapter. It intentionally has no owner-scope or wildcard
// fields; those require a domain policy decision.
type Grant struct {
	Resource  string
	Operation string
	OwnerOnly bool
	Scope     contract.BusinessScope
}

// Valid reports whether a grant names one exact resource and supported
// operation. Grants intentionally have no wildcard or inheritance semantics.
func (g Grant) Valid() bool {
	if !validResource(g.Resource) || !validOperation(g.Operation) {
		return false
	}
	if g.Scope.Kind == "" && g.Scope.TenantID == "" && g.Scope.LegalEntityID == "" {
		return true
	}
	return g.Scope.Validate() == nil
}

// NewVerifiedExecution is for trusted authentication adapters after they have
// validated the credential and current tenant membership.
func NewVerifiedExecution(tenant contract.TenantID, actor contract.ActorContext) (Execution, error) {
	return NewVerifiedExecutionWithGrants(tenant, actor, nil)
}

// NewVerifiedExecutionWithGrants binds an authenticated actor and a copied,
// validated set of delegated resource-operation grants.
func NewVerifiedExecutionWithGrants(tenant contract.TenantID, actor contract.ActorContext, grants []Grant) (Execution, error) {
	parsedTenant, err := contract.ParseTenantID(string(tenant))
	if err != nil {
		return Execution{}, fmt.Errorf("execution tenant: %w", err)
	}
	if !actor.Authenticated() {
		return Execution{}, contract.NewError(contract.CodeUnauthenticated, "execution requires an authenticated actor")
	}
	if actor.Site == "" {
		return Execution{}, contract.NewError(contract.CodePermissionDenied, "actor tenant route is required")
	}
	actor.Roles = append([]string(nil), actor.Roles...)
	actor.Scopes = append([]string(nil), actor.Scopes...)
	grantCopy := make([]Grant, 0, len(grants))
	seen := make(map[Grant]struct{}, len(grants))
	for _, grant := range grants {
		if !grant.Valid() || grant.Scope.TenantID != "" && grant.Scope.TenantID != parsedTenant {
			return Execution{}, contract.NewError(contract.CodeValidationFailed, "execution grant is invalid")
		}
		if _, exists := seen[grant]; exists {
			continue
		}
		seen[grant] = struct{}{}
		grantCopy = append(grantCopy, grant)
	}
	return Execution{tenant: parsedTenant, actor: actor, grants: grantCopy}, nil
}

func (e Execution) Tenant() contract.TenantID { return e.tenant }

func (e Execution) Actor() contract.ActorContext {
	actor := e.actor
	actor.Roles = append([]string(nil), actor.Roles...)
	actor.Scopes = append([]string(nil), actor.Scopes...)
	return actor
}

// Grants returns a copy of the verified credential grants. Durable workers
// may persist these only as part of a source transaction created from this
// validated execution, then reconstruct the same bounded execution context.
func (e Execution) Grants() []Grant {
	return append([]Grant(nil), e.grants...)
}

// HasGrant reports whether the credential contains this exact grant,
// including its business scope and owner restriction.
func (e Execution) HasGrant(required Grant) bool {
	if !e.Valid() || !required.Valid() {
		return false
	}
	for _, grant := range e.grants {
		if grant == required {
			return true
		}
	}
	return false
}

func (e Execution) Valid() bool {
	return e.tenant != "" && e.actor.Authenticated() && e.actor.Site != ""
}

// Allows reports whether a verified delegated grant covers this exact
// resource and operation. Grants are not inherited by similarly named types.
func (e Execution) Allows(resource, operation string) bool {
	if !e.Valid() || resource == "" || !validOperation(operation) {
		return false
	}
	for _, grant := range e.grants {
		if grant.Scope.Kind == "" && grant.Scope.TenantID == "" && grant.Scope.LegalEntityID == "" && grant.Resource == resource && grant.Operation == operation {
			return true
		}
	}
	return false
}

// GrantFor returns the exact verified grant for a resource operation. When
// duplicate grants differ by owner scope, the restrictive scope wins.
func (e Execution) GrantFor(resource, operation string) (Grant, bool) {
	if !e.Valid() || resource == "" || !validOperation(operation) {
		return Grant{}, false
	}
	var found Grant
	allowed := false
	for _, grant := range e.grants {
		if grant.Scope.Kind != "" || grant.Scope.TenantID != "" || grant.Scope.LegalEntityID != "" || grant.Resource != resource || grant.Operation != operation {
			continue
		}
		if !allowed || grant.OwnerOnly {
			found = grant
		}
		allowed = true
	}
	return found, allowed
}

// GrantForScope returns only a grant issued for this exact legal-entity scope.
// An unscoped tenant grant or tenant role never implies entity access.
func (e Execution) GrantForScope(resource, operation string, scope contract.BusinessScope) (Grant, bool) {
	if !e.Valid() || scope.Validate() != nil || scope.TenantID != e.Tenant() || resource == "" || !validOperation(operation) {
		return Grant{}, false
	}
	var found Grant
	allowed := false
	for _, grant := range e.grants {
		if grant.Resource != resource || grant.Operation != operation || grant.Scope != scope {
			continue
		}
		if !allowed || grant.OwnerOnly {
			found = grant
		}
		allowed = true
	}
	return found, allowed
}

func validOperation(operation string) bool {
	switch operation {
	case "read", "create", "write", "delete", "submit", "cancel", "amend", "export", "import", "report":
		return true
	default:
		return false
	}
}

func validResource(resource string) bool {
	if resource == "" || len(resource) > 160 || strings.TrimSpace(resource) != resource {
		return false
	}
	for _, r := range resource {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type ReadPurpose string

const (
	ReadResource       ReadPurpose = "resource"
	ReadEvent          ReadPurpose = "event"
	ReadExport         ReadPurpose = "export"
	ReadRequestSubject ReadPurpose = "request_subject"
)

type ReadDecision struct {
	Allowed           bool
	VisibleFields     []string
	CanReadHistorical bool
	PolicyRevision    contract.Revision
}

type Reader interface {
	DecideRead(context.Context, Execution, contract.SubjectRef, ReadPurpose) (ReadDecision, error)
}

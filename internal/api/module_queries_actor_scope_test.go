package api

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestAuthorizePublishedQueryActorScopeForOwnerFieldQueries is the #407 gate
// matrix: an owner_field query always gets the verified actor; only an
// if_owner grant sets OwnerOnly; queries without owner_field are unchanged.
func TestAuthorizePublishedQueryActorScopeForOwnerFieldQueries(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	ownerQuery := contract.PublishedQuery{OwnerModuleID: "pos.core", Name: "my_session", ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"name", "cashier"}, OwnerField: "cashier"}
	plainQuery := contract.PublishedQuery{OwnerModuleID: "pos.core", Name: "open_till", ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"name", "cashier"}}
	registry := doctype.NewRegistry()
	for _, p := range []doctype.Permission{
		{Doctype: "module.pos.core.query.my_session", Role: "Cashier", Read: true, IfOwner: true},
		{Doctype: "module.pos.core.query.my_session", Role: "Manager", Read: true},
		{Doctype: "module.pos.core.query.open_till", Role: "Cashier", Read: true},
		{Doctype: "module.pos.core.query.open_till", Role: "Owner", Read: true},
	} {
		registry.Permissions.SetPermission(&p)
	}
	tests := []struct {
		name, role, subject, principal string
		query                          contract.PublishedQuery
		wantDenied, wantScope          bool
		wantOwnerOnly                  bool
		wantSubject                    string
	}{
		{"cashier if_owner owner query", "Cashier", "cashier-1", "p-cashier", ownerQuery, false, true, true, "cashier-1"},
		{"manager plain owner query", "Manager", "manager-1", "p-manager", ownerQuery, false, true, false, "manager-1"},
		{"administrator owner query", doctype.AdminRole, "admin-1", "p-admin", ownerQuery, false, true, false, "admin-1"},
		{"administrator falls back to principal", doctype.AdminRole, "", "p-admin", ownerQuery, false, true, false, "p-admin"},
		{"owner no grant on owner query", "Owner", "owner-1", "p-owner", ownerQuery, true, false, false, ""},
		{"no grant owner query", "Nobody", "nobody-1", "p-nobody", ownerQuery, true, false, false, ""},
		{"cashier plain non-owner query", "Cashier", "cashier-1", "p-cashier", plainQuery, false, false, false, ""},
		{"owner plain non-owner query", "Owner", "owner-1", "p-owner", plainQuery, false, false, false, ""},
		{"administrator non-owner query", doctype.AdminRole, "admin-1", "p-admin", plainQuery, false, false, false, ""},
		{"no grant non-owner query", "Nobody", "nobody-1", "p-nobody", plainQuery, true, false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: tc.principal, SubjectUserID: tc.subject, PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{tc.role}, AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			actor, authErr := authorizePublishedQueryActorScope(execution, registry, tc.query, scope)
			if (authErr != nil) != tc.wantDenied {
				t.Fatalf("denied = %v (%v), want %v", authErr != nil, authErr, tc.wantDenied)
			}
			if (actor != nil) != tc.wantScope {
				t.Fatalf("scope = %#v, want present %v", actor, tc.wantScope)
			}
			if actor == nil {
				return
			}
			if actor.OwnerOnly != tc.wantOwnerOnly || actor.SubjectID != tc.wantSubject || actor.OwnerField != tc.query.OwnerField || actor.BusinessScope != scope || len(actor.AssignedFields) != 0 {
				t.Fatalf("scope = %#v", actor)
			}
		})
	}
}

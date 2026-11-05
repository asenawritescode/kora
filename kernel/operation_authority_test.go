package kernel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func TestPublishedOperationAuthorizesStepsByOperationGrant(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Unowned"})
	reg.Register(&doctype.DocType{Name: "OwnCrud", ModuleOwnerID: "pos.core", ModuleOwnershipMode: string(contract.MutationCRUD)})
	reg.Register(&doctype.DocType{Name: "OwnCmd", ModuleOwnerID: "pos.core", ModuleOwnershipMode: string(contract.MutationCommands)})
	reg.Register(&doctype.DocType{Name: "OtherCrud", BusinessScopePolicy: contract.ScopeEntityOwned, ModuleOwnerID: "inv.core", ModuleOwnershipMode: string(contract.MutationCRUD)})
	reg.Register(&doctype.DocType{Name: "OtherCmd", ModuleOwnerID: "inv.core", ModuleOwnershipMode: string(contract.MutationCommands)})
	opGrant := policy.Grant{Resource: "module.pos.core.operation.sell", Operation: "write", Scope: scope}

	cmd := func(steps ...CommandStep) *CommandResource {
		return &CommandResource{
			Name: "sell", Namespace: "pos", Version: 1, ModuleOwnerID: "pos.core", Capability: "pos.sale", Operation: "sell",
			InputSchema: contract.SchemaRef{ID: "in", Version: 1}, OutputSchema: contract.SchemaRef{ID: "out", Version: 1}, Steps: steps,
		}
	}
	create := func(r string) CommandStep {
		return CommandStep{Create: &StepCreate{Record: r, Values: map[string]string{"x": "1"}}}
	}
	update := func(r string) CommandStep {
		return CommandStep{Update: &StepUpdate{Record: r, Name: "$input.n", Values: map[string]string{"x": "1"}}}
	}

	tests := []struct {
		name     string
		steps    []CommandStep
		grants   []policy.Grant
		verified bool
		wantErr  bool
	}{
		{"op grant runs unowned create and own crud update", []CommandStep{create("Unowned"), update("OwnCrud"), create("OwnCmd")}, []policy.Grant{opGrant}, true, false},
		{"direct route checks every step", []CommandStep{create("Unowned"), update("OwnCrud")}, []policy.Grant{opGrant}, false, true},
		{"no operation grant", []CommandStep{create("Unowned")}, nil, true, true},
		{"other module commands_only stays denied", []CommandStep{create("OwnCmd"), create("OtherCmd")}, []policy.Grant{opGrant}, true, true},
		{"other module crud needs actor grant", []CommandStep{create("OtherCrud")}, []policy.Grant{opGrant}, true, true},
		{"other module crud allowed with actor grant", []CommandStep{create("OtherCrud")}, []policy.Grant{opGrant, {Resource: "OtherCrud", Operation: "create", Scope: scope}}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actor, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{
				PrincipalID: "cashier", PrincipalType: contract.PrincipalService, Site: "tenant-a", AuthenticatedAt: time.Now().UTC(),
			}, tc.grants)
			if err != nil {
				t.Fatal(err)
			}
			c := cmd(tc.steps...)
			op := Operation{Command: c.FullName(), Payload: json.RawMessage(`{"n":"a"}`), Context: OperationContext{TenantID: "tenant-a", Site: "tenant-a", BusinessScope: scope, Execution: actor}}
			if tc.verified {
				me, err := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, c.FullName(), c.FullName())
				if err != nil {
					t.Fatal(err)
				}
				op.Context.ModuleExecution = &me
			}
			authErr := authorizeOp(reg, op, CommandDefinition{Name: c.FullName()}, c)
			if (authErr != nil) != tc.wantErr {
				t.Fatalf("authorizeOp err=%v wantErr=%v", authErr, tc.wantErr)
			}
		})
	}
}

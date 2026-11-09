package kernel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

func TestRegisteredCrossEntityCommandRequiresBothScopedGrants(t *testing.T) {
	source := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	target := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	command, err := ParseCommandResource([]byte(`
name: transfer.apply
namespace: finance
version: 1
input:
  record: Transfer
cross_entity:
  resource: Intercompany Transfer
  operation: write
  target_entity_field: input.target_entity_id
transaction:
  - create:
      record: Transfer
      business_scope: source
      values:
        title: $input.title
  - create:
      record: Transfer
      business_scope: target
      values:
        title: $input.title
`))
	if err != nil {
		t.Fatal("parse registered cross-entity command:", err)
	}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Transfer", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "legal_entity_id", Fieldtype: "Data"}}, BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id"})

	makeOperation := func(grants []policy.Grant, sourceScope contract.BusinessScope) Operation {
		actor := contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC()}
		execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", actor, grants)
		if err != nil {
			t.Fatalf("create verified execution: %v", err)
		}
		payload, _ := json.Marshal(map[string]any{"data": map[string]any{"target_entity_id": "entity-b", "title": "transfer"}})
		return Operation{Command: "finance.transfer.apply", Payload: payload, Context: OperationContext{Site: "tenant-a", TenantID: "tenant-a", Actor: actor, Execution: execution, BusinessScope: sourceScope}}
	}
	grants := []policy.Grant{
		{Resource: "Intercompany Transfer", Operation: "write", Scope: source},
		{Resource: "Intercompany Transfer", Operation: "write", Scope: target},
		{Resource: "Transfer", Operation: "create", Scope: source},
		{Resource: "Transfer", Operation: "create", Scope: target},
	}

	valid := makeOperation(grants, source)
	if err := prepareCrossEntityCommand(&valid, command); err != nil {
		t.Fatal("resolve verified registered operation:", err)
	}
	definition := CommandDefinition{Name: command.FullName(), Version: command.Version, AuthorizesWith: command.PermOperation(), IdempotentByKey: true}
	if err := authorizeOp(registry, valid, definition, command); err != nil {
		t.Fatalf("registered operation with both exact grants denied: %v", err)
	}

	for name, test := range map[string]struct {
		grants []policy.Grant
		scope  contract.BusinessScope
	}{
		"missing target grant": {grants: grants[:3], scope: source},
		"forged source entity": {grants: grants, scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-c"}},
	} {
		t.Run(name, func(t *testing.T) {
			operation := makeOperation(test.grants, test.scope)
			if prepareErr := prepareCrossEntityCommand(&operation, command); prepareErr == nil {
				if authErr := authorizeOp(registry, operation, definition, command); authErr == nil {
					t.Fatal("cross-entity operation authorized without exact source and target grants")
				}
			}
		})
	}
	wrongTarget := makeOperation(grants, source)
	wrongTarget.Payload = json.RawMessage(`{"data":{"target_entity_id":"entity-a","title":"transfer"}}`)
	if err := prepareCrossEntityCommand(&wrongTarget, command); err == nil {
		t.Fatal("same-entity target accepted as cross-entity operation")
	}
}

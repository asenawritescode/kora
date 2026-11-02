package kernel

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func TestGenericMutationCannotUseCommandOnlyModuleExecution(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)})
	payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Invoice", Data: json.RawMessage(`{}`)})
	op := Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{TenantID: "tenant-a"}}
	if !mutationTouchesCommandOnly(registry, op) {
		t.Fatal("protected record was not recognized")
	}
	if err := authorizeOp(registry, op, CommandDefinition{Name: CommandRecordCreate}, nil); err == nil {
		t.Fatal("generic mutation authorized without trusted module execution")
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := contract.NewCommandModuleExecution(scope, "sales.orders", "finance.ar", 3, "finance.issue_invoice", "finance.issue_invoice")
	if err != nil {
		t.Fatal(err)
	}
	op.Context.ModuleExecution = &execution
	if err := authorizeOp(registry, op, CommandDefinition{Name: CommandRecordCreate}, nil); err == nil {
		t.Fatal("domain command execution authorized generic record command")
	}
	// User-supplied struct values cannot set the private verification seal.
	forged := contract.ModuleExecution{TenantID: "tenant-a", CallerModuleID: "sales.orders", OwnerModuleID: "finance.ar", BindingRevision: 3, AllowedOperation: CommandRecordCreate, Scope: scope}
	op.Context.ModuleExecution = &forged
	if validModuleExecution(op.Context.ModuleExecution, op, registry) {
		t.Fatal("forged module execution was accepted")
	}
}

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

func TestDynamicCommandCannotBypassCommandOnlyOwnership(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)})
	command := &CommandResource{Name: "issue", Namespace: "finance", Version: 1, Input: CommandInput{Record: "Invoice"}, Steps: []CommandStep{{Create: &StepCreate{Record: "Invoice"}}}}
	op := Operation{Command: command.FullName(), Context: OperationContext{TenantID: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}}}
	if !dynamicCommandTouchesCommandOnly(registry, command) {
		t.Fatal("dynamic command's protected target was not detected")
	}
	if validDynamicModuleExecution(nil, op, registry, command) {
		t.Fatal("missing module execution accepted")
	}
	forged := contract.ModuleExecution{TenantID: "tenant-a", CallerModuleID: "sales.orders", OwnerModuleID: "finance.ar", BindingRevision: 3, AllowedOperation: op.Command, AuthorizedCommand: op.Command, Scope: op.Context.BusinessScope}
	op.Context.ModuleExecution = &forged
	if validDynamicModuleExecution(&forged, op, registry, command) {
		t.Fatal("forged module execution accepted")
	}
	wrongOwner, err := contract.NewCommandModuleExecution(op.Context.BusinessScope, "sales.orders", "sales.orders", 3, op.Command, op.Command)
	if err != nil {
		t.Fatal(err)
	}
	if validDynamicModuleExecution(&wrongOwner, op, registry, command) {
		t.Fatal("non-owner command execution accepted")
	}
	valid, err := contract.NewCommandModuleExecution(op.Context.BusinessScope, "sales.orders", "finance.ar", 3, op.Command, op.Command)
	if err != nil {
		t.Fatal(err)
	}
	if !validDynamicModuleExecution(&valid, op, registry, command) {
		t.Fatal("valid owner command execution rejected")
	}
}

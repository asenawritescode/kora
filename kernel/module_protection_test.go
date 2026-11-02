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

func TestCommandOnlyResourcesRejectGenericMutationSources(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)})

	tests := []struct {
		name    string
		command string
		payload string
		source  Source
	}{
		{name: "http create", command: CommandRecordCreate, payload: `{"doctype":"Invoice","data":{}}`, source: SourceHTTP},
		{name: "ai update", command: CommandRecordUpdate, payload: `{"doctype":"Invoice","name":"INV-1","data":{}}`, source: SourceAI},
		{name: "mcp delete", command: CommandRecordDelete, payload: `{"doctype":"Invoice","name":"INV-1"}`, source: SourceMCP},
		{name: "scheduled worker generic create", command: CommandRecordCreate, payload: `{"doctype":"Invoice","data":{}}`, source: SourceWorker},
		{name: "workflow transition", command: CommandWorkflowStatusTransition, payload: `{"doctype":"Invoice","name":"INV-1","action":"approve"}`, source: SourceWorker},
		{name: "public form", command: CommandPublicFormSubmit, payload: `{"doctype":"Invoice","public_route":"apply","data":{}}`, source: SourceHTTP},
		{name: "bundle with protected child", command: CommandRecordMutateBundle, payload: `{"doctype":"Task","records":[{"key":"task","doctype":"Task","data":{}},{"key":"invoice","doctype":"Invoice","data":{}}]}`, source: SourceAI},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			op := Operation{
				Command: test.command,
				Payload: json.RawMessage(test.payload),
				Context: OperationContext{TenantID: "tenant-a", Source: test.source, Roles: []string{doctype.AdminRole}, UserRole: doctype.AdminRole},
			}
			if err := authorizeOp(registry, op, CommandDefinition{Name: test.command}, nil); err == nil || err.Type != contract.CodePermissionDenied {
				t.Fatalf("broad actor role authorized generic command against commands-only resource: got %v", err)
			}
		})
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

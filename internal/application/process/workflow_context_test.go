package process

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
)

func TestVerifiedWorkflowOperationContextPreservesCausationDepth(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "workflow-service", PrincipalType: contract.PrincipalService, Site: "tenant-route", AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	action := workflow.ActionContext{Execution: execution, BusinessScope: scope, CausationDepth: 11, InstanceID: "01K7INSTANCE00000000000000000"}
	requested := kernel.OperationContext{CorrelationID: "correlation-1", CausationDepth: 2}
	actual, contextErr := verifiedWorkflowOperationContext(action, requested, "")
	if contextErr != nil {
		t.Fatal(contextErr)
	}
	if actual.CausationDepth != 11 || actual.BusinessScope != scope || actual.CorrelationID != requested.CorrelationID {
		t.Fatalf("verified workflow context lost causal or request metadata: %+v", actual)
	}
}

func TestVerifiedWorkflowOperationContextPropagatesOnlyGrantedModuleExecution(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	command := "finance.invoice.issue"
	actor := contract.ActorContext{PrincipalID: "workflow-service", PrincipalType: contract.PrincipalService, Site: "tenant-route", AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", actor, []policy.Grant{{Resource: "Invoice", Operation: "create", Scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	moduleExecution, err := contract.NewCommandModuleExecution(scope, "sales.orders", "finance.ar", 9, command, command)
	if err != nil {
		t.Fatal(err)
	}
	grant := policy.Grant{Resource: "Invoice", Operation: "create", Scope: scope}
	action := workflow.ActionContext{Execution: execution, BusinessScope: scope, RequiredGrant: grant, InstanceID: "01K7INSTANCE00000000000000000", StepID: "issue", IdempotencyKey: "workflow-intent-1"}
	requested := kernel.OperationContext{ModuleExecution: &moduleExecution, BusinessScope: scope}
	actual, err := verifiedWorkflowOperationContext(action, requested, command)
	if err != nil {
		t.Fatal("matching trusted module execution was rejected:", err)
	}
	if actual.ModuleExecution == nil || !actual.ModuleExecution.Verified() || actual.ModuleExecution.CallerModuleID != "sales.orders" || actual.ModuleExecution.OwnerModuleID != "finance.ar" || actual.ModuleExecution.BindingRevision != 9 {
		t.Fatalf("trusted workflow module provenance was not preserved: %+v", actual.ModuleExecution)
	}

	tests := []struct {
		name    string
		module  contract.ModuleExecution
		command string
	}{
		{name: "forged seal", module: contract.ModuleExecution{TenantID: scope.TenantID, CallerModuleID: "sales.orders", OwnerModuleID: "finance.ar", BindingRevision: 9, AllowedOperation: command, AuthorizedCommand: command, Scope: scope}, command: command},
		{name: "wrong command", module: moduleExecution, command: "finance.invoice.cancel"},
		{name: "wrong scope", module: moduleExecution, command: command},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			badAction := action
			badModule := test.module
			badScope := scope
			if test.name == "wrong scope" {
				badScope.LegalEntityID = "entity-b"
			}
			badAction.BusinessScope = badScope
			badRequest := kernel.OperationContext{ModuleExecution: &badModule}
			if _, contextErr := verifiedWorkflowOperationContext(badAction, badRequest, test.command); contextErr == nil {
				t.Fatal("mismatched workflow module provenance was accepted")
			}
		})
	}
}

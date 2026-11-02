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
	actual := verifiedWorkflowOperationContext(action, requested)
	if actual.CausationDepth != 11 || actual.BusinessScope != scope || actual.CorrelationID != requested.CorrelationID {
		t.Fatalf("verified workflow context lost causal or request metadata: %+v", actual)
	}
}

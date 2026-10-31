package engineadapter

import (
	"testing"

	"github.com/asenawritescode/kora/internal/application/process"
	"github.com/asenawritescode/kora/internal/application/purchasing"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/policy"
)

func TestConfigureRegistersOnlyPurchasingOwnedEngineBindings(t *testing.T) {
	config := &engine.Config{Workflow: engine.SelfHostedWorkflowConfig()}
	Configure(config)
	if config.RequestAuthorizer == nil {
		t.Fatal("purchasing request authorizer was not registered")
	}
	wantApproval := process.WorkflowApproval{SubjectKind: purchasing.PurchaseRequest, Action: purchasing.ApprovalAction, RejectAction: "Reject"}
	if got := config.WorkflowApprovals[purchasing.ApprovalPolicyID]; got != wantApproval {
		t.Fatalf("approval binding=%+v, want %+v", got, wantApproval)
	}
	wantGrants := map[policy.Grant]bool{
		{Resource: purchasing.PurchaseRequest, Operation: "read"}:               true,
		{Resource: purchasing.WorkflowApprovalSubmitGrant, Operation: "submit"}: true,
	}
	if len(config.Workflow.ServiceGrants) != len(wantGrants) {
		t.Fatalf("workflow domain grants=%+v, want %+v", config.Workflow.ServiceGrants, wantGrants)
	}
	for _, grant := range config.Workflow.ServiceGrants {
		if !wantGrants[grant] {
			t.Fatalf("unexpected workflow domain grant %+v", grant)
		}
	}
}

func TestConfigureNilConfigIsSafe(t *testing.T) {
	Configure(nil)
}

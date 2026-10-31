package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/workflow"
)

type publishedStatusPolicyStub struct{ policy workflow.StatusPolicy }

func (s publishedStatusPolicyStub) GetStatusPolicyInTx(context.Context, *sql.Tx, contract.TenantID, string) (*workflow.StatusPolicy, bool, error) {
	return &s.policy, true, nil
}

func TestPublishedStatusActionGuardsGenericRecordWrites(t *testing.T) {
	dt := &doctype.DocType{Name: "Purchase Request", Fields: []doctype.Field{{Fieldname: "phase", Fieldtype: "Select", Default: "draft"}, {Fieldname: "supplier", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.Register(dt)
	statusPolicy := workflow.StatusPolicy{Table: dt.Name, StateField: "phase", InitialState: "draft", Transitions: []workflow.StatusTransition{{Action: "Approve", From: "draft", To: "approved", AllowedRoles: []string{"Approver"}}}}
	k := &Kernel{WorkflowStatusPolicies: publishedStatusPolicyStub{policy: statusPolicy}}
	op := Operation{Context: OperationContext{TenantID: "tenant-a"}}
	if err := k.rejectWorkflowStateMutation(context.Background(), nil, op, dt, registry, json.RawMessage(`{"phase":"approved"}`)); err == nil {
		t.Fatal("generic update bypassed published status action")
	}
	if err := k.rejectWorkflowStateOnCreate(context.Background(), nil, op, dt, registry, json.RawMessage(`{"phase":"approved"}`)); err == nil {
		t.Fatal("create inserted a noninitial workflow status")
	}
	before := doctype.NewDocument(dt.Name)
	before.Set("phase", "draft")
	after := doctype.NewDocument(dt.Name)
	after.Set("phase", "approved")
	if err := k.rejectWorkflowStateChange(context.Background(), nil, op, dt, registry, before, after); err == nil {
		t.Fatal("generic document mutation changed workflow status")
	}
}

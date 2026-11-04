package workflow

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestApplyStatusTransitionEnforcesRoleRequiredFieldsAndCondition(t *testing.T) {
	statusPolicy := &StatusPolicy{Table: "Purchase Request", StateField: "status", InitialState: "Draft", Transitions: []StatusTransition{{Action: "Approve", From: "Submitted", To: "Approved", DocStatus: 1, AllowedRoles: []string{"Approver"}, RequireFields: []string{"supplier"}, Condition: "doc.amount > 100"}}}
	doc := doctype.NewDocument("Purchase Request")
	doc.Set("supplier", "S-1")
	doc.Set("amount", 150)
	if _, _, err := ApplyStatusTransition(statusPolicy, "Submitted", "Approve", []string{"Requester"}, doc); err == nil {
		t.Fatal("unauthorized role was allowed")
	}
	if _, _, err := ApplyStatusTransition(statusPolicy, "Submitted", "Approve", []string{"Approver"}, doctype.NewDocument("Purchase Request")); err == nil {
		t.Fatal("missing required field was allowed")
	}
	state, docStatus, err := ApplyStatusTransition(statusPolicy, "Submitted", "Approve", []string{"Approver"}, doc)
	if err != nil || state != "Approved" || docStatus != 1 {
		t.Fatalf("transition result=(%q,%d,%v)", state, docStatus, err)
	}
	doc.Set("amount", 50)
	if _, _, err := ApplyStatusTransition(statusPolicy, "Submitted", "Approve", []string{"Approver"}, doc); err == nil {
		t.Fatal("false transition condition was allowed")
	}
}

func TestValidateStatusTransitionRejectsInvalidCondition(t *testing.T) {
	issues := ValidateStatusTransition(StatusTransition{Action: "Approve", From: "Draft", To: "Approved", Condition: "doc.amount >"})
	if len(issues) != 1 || issues[0].Code != "invalid_expression" {
		t.Fatalf("issues = %+v", issues)
	}
}

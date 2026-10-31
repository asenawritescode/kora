package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
)

func TestBuiltinWorkflowActionSchemas(t *testing.T) {
	tests := []struct {
		name  string
		ref   contract.SchemaRef
		data  string
		valid bool
	}{
		{"email requires recipient field", emailWorkflowInputSchema, `{"subject":"Hello","message":"Body"}`, false},
		{"email accepted", emailWorkflowInputSchema, `{"recipient_field":"contact_email","subject":"Hello","message":"Body"}`, true},
		{"email rejects extra keys", emailWorkflowInputSchema, `{"recipient_field":"email","subject":"Hello","message":"Body","to":"other@example.com"}`, false},
		{"status requires configured transition", statusWorkflowInputSchema, `{}`, false},
		{"status accepts transition", statusWorkflowInputSchema, `{"state_field":"status","initial_state":"Draft","transition_action":"Approve","from":"Submitted","to":"Approved","allowed_roles":["Approver"]}`, true},
		{"status output validates record result", statusWorkflowOutputSchema, `{"doctype":"Request","name":"REQ-1","document":{"status":"Approved"},"operation_id":"op-1"}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateBuiltinWorkflowSchema(test.ref, json.RawMessage(test.data))
			if (err == nil) != test.valid {
				t.Fatalf("validateBuiltinWorkflowSchema() error = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestBuiltinWorkflowActionGrantsAreValid(t *testing.T) {
	registrations := []workflow.ActionRegistration{
		statusWorkflowRegistration(),
		emailWorkflowRegistration(nil, nil, nil, nil),
		notificationWorkflowRegistration(nil, nil, nil, nil, nil),
	}
	for _, registration := range registrations {
		if !registration.RequiredGrant.Valid() {
			t.Errorf("action %s@%d has invalid required grant %+v", registration.Key.Name, registration.Key.Version, registration.RequiredGrant)
		}
	}
	if got := statusWorkflowRegistration().RequiredGrant; got != (policy.Grant{Resource: "workflow.status", Operation: "submit"}) {
		t.Fatalf("status action grant = %+v", got)
	}
}

func TestWorkflowEmailFieldAcceptsDataFieldWithEmailOption(t *testing.T) {
	if !isWorkflowEmailField(&doctype.Field{Fieldtype: "Data", Options: "Email"}) {
		t.Fatal("Data field with Email option was not recognized as an email field")
	}
	if isWorkflowEmailField(&doctype.Field{Fieldtype: "Data", Options: "Phone"}) {
		t.Fatal("non-email Data field was recognized as an email field")
	}
}

func TestStatusWorkflowOperationUsesPinnedRecordAndRevision(t *testing.T) {
	input := workflow.ActionInput{Schema: statusWorkflowInputSchema, Data: json.RawMessage(`{"state_field":"status","initial_state":"Draft","transition_action":"Approve","from":"Submitted","to":"Approved","allowed_roles":["Approver"]}`)}
	action := workflow.ActionContext{Subject: contract.SubjectRef{Kind: "Request", ID: "REQ-1", Revision: "7"}}
	operation, err := statusWorkflowOperation(context.Background(), action, input)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Command != "workflow.status_transition" {
		t.Fatalf("command = %q", operation.Command)
	}
	var payload struct {
		Doctype          string `json:"doctype"`
		Name             string `json:"name"`
		Action           string `json:"action"`
		ExpectedRevision uint64 `json:"expected_revision"`
	}
	if err := json.Unmarshal(operation.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Doctype != "Request" || payload.Name != "REQ-1" || payload.Action != "Approve" || payload.ExpectedRevision != 7 {
		t.Fatalf("status transition payload = %+v", payload)
	}
}

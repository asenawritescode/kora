package requests

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

func projectionExecution(t *testing.T, user string) policy.Execution {
	t.Helper()
	exec, err := policy.NewVerifiedExecution("tenant-stable-id", contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user, Site: "acme", AuthenticatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestDirectTaskProjectorLimitsAssignmentsToViewer(t *testing.T) {
	projector := DirectTaskProjector{}
	record := RequestRecord{ID: "01K7REQUEST00000000000000000", Revision: "2", Kind: contract.RequestTask, Status: contract.RequestPending,
		Requester: "requester", Title: "Review", Message: "Please review", CreatedAt: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC),
		Assignments: []AssignmentRecord{
			{ID: "assignment-a", Principal: "user-a", Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}},
			{ID: "assignment-b", Principal: "user-b", Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}},
		}}
	assigneeView, err := projector.Request(context.Background(), projectionExecution(t, "user-a"), record)
	if err != nil {
		t.Fatal(err)
	}
	if len(assigneeView.Assignments) != 1 || assigneeView.Assignments[0].Assignee.ID != "user-a" || len(assigneeView.AllowedResponses) != 1 {
		t.Fatalf("assignee projection leaks other assignments or drops own action: %+v", assigneeView)
	}
	requesterView, err := projector.Request(context.Background(), projectionExecution(t, "requester"), record)
	if err != nil {
		t.Fatal(err)
	}
	if len(requesterView.Assignments) != 2 || len(requesterView.AllowedResponses) != 0 {
		t.Fatalf("requester projection does not show assignment progress safely: %+v", requesterView)
	}
	if _, err := projector.Request(context.Background(), projectionExecution(t, "unrelated"), record); err == nil {
		t.Fatal("unrelated tenant user received direct task details")
	}
}

func TestDirectTaskProjectorRejectsWorkflowAndBusinessRequests(t *testing.T) {
	projector := DirectTaskProjector{}
	exec := projectionExecution(t, "requester")
	base := RequestRecord{ID: "01K7REQUEST00000000000000000", Kind: contract.RequestTask, Requester: "requester"}
	for _, record := range []RequestRecord{
		{ID: base.ID, Kind: contract.RequestApproval, Requester: "requester"},
		{ID: base.ID, Kind: contract.RequestTask, Requester: "requester", PolicyID: "domain-policy"},
		{ID: base.ID, Kind: contract.RequestTask, Requester: "requester", Subject: &contract.SubjectRef{Kind: "invoice", ID: "INV-1", Revision: "1"}},
		{ID: base.ID, Kind: contract.RequestTask, Requester: "requester", Source: &contract.RequestSource{InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve"}},
	} {
		if _, err := projector.Request(context.Background(), exec, record); err == nil {
			t.Fatal("direct task projector exposed a request requiring domain authorization")
		}
	}
}

func TestDirectTaskProjectorHidesOtherAssignmentsResponsesAndHistory(t *testing.T) {
	projector := DirectTaskProjector{}
	exec := projectionExecution(t, "user-a")
	responseA := contract.ResponseComplete
	responseB := contract.ResponseReturn
	actorB := contract.PrincipalID("user-b")
	record := RequestRecord{ID: "01K7REQUEST00000000000000000", Kind: contract.RequestTask, Status: contract.RequestPending, Requester: "requester",
		Assignments: []AssignmentRecord{
			{ID: "01K7ASSIGNMENT0000000000000001", Principal: "user-a", Status: contract.AssignmentResponded},
			{ID: "01K7ASSIGNMENT0000000000000002", Principal: "user-b", Status: contract.AssignmentPending},
		}}
	otherResponse := DecisionRecord{ID: "01K7DECISION000000000000000001", RequestID: record.ID, AssignmentID: record.Assignments[1].ID,
		Response: responseB, Comment: "Private feedback"}
	if _, err := projector.Response(context.Background(), exec, record, otherResponse); err == nil {
		t.Fatal("assignee received another user's response details")
	}
	ownResponse := otherResponse
	ownResponse.AssignmentID, ownResponse.Response = record.Assignments[0].ID, responseA
	if _, err := projector.Response(context.Background(), exec, record, ownResponse); err != nil {
		t.Fatalf("assignee could not read own response: %v", err)
	}
	entry := contract.RequestHistoryEntry{Sequence: "3", Kind: contract.HistoryResponded, Actor: &actorB,
		AssignmentID: record.Assignments[1].ID, Response: &responseB, Comment: "Private feedback"}
	hidden, err := projector.History(context.Background(), exec, record, entry)
	if err != nil {
		t.Fatal("project hidden history entry:", err)
	}
	if hidden.Actor != nil || hidden.AssignmentID != "" || hidden.Response != nil || hidden.Comment != "" {
		t.Fatalf("history exposed another assignment: %+v", hidden)
	}
	if hidden.Sequence != entry.Sequence || hidden.Kind != entry.Kind || !hidden.At.Equal(entry.At) {
		t.Fatalf("redacted history lost event position: %+v", hidden)
	}
	ownEntry := entry
	ownEntry.Actor = nil
	ownEntry.AssignmentID = record.Assignments[0].ID
	ownEntry.Response, ownEntry.Comment = &responseA, "My response"
	visible, err := projector.History(context.Background(), exec, record, ownEntry)
	if err != nil {
		t.Fatal("project own history entry:", err)
	}
	if visible.AssignmentID != ownEntry.AssignmentID || visible.Response == nil || visible.Comment != "My response" {
		t.Fatalf("own history was redacted: %+v", visible)
	}
	previous := contract.Assignee{Kind: contract.AssigneeUser, ID: "user-a"}
	next := contract.Assignee{Kind: contract.AssigneeUser, ID: "user-c"}
	transfer := contract.RequestHistoryEntry{Sequence: "4", Kind: contract.HistoryReassigned, PreviousAssignee: &previous, NewAssignee: &next}
	visibleTransfer, err := projector.History(context.Background(), exec, record, transfer)
	if err != nil || visibleTransfer.PreviousAssignee == nil || visibleTransfer.NewAssignee == nil {
		t.Fatalf("participant could not inspect their own reassignment: %+v, %v", visibleTransfer, err)
	}
}

func TestParticipantProjectorExposesAuthorizedApprovalViewAndRedactsPeers(t *testing.T) {
	projector := ParticipantProjector{}
	actor := projectionExecution(t, "reviewer-a")
	subject := &contract.SubjectRef{Kind: "Purchase Request", ID: "PR-1", Revision: "3"}
	responseApprove := contract.ResponseApprove
	record := RequestRecord{ID: "01K7REQUEST00000000000000000", Revision: "2", Kind: contract.RequestApproval, Status: contract.RequestPending,
		Requester: "requester", Subject: subject, PolicyID: "purchase-request-approval-v1", Title: "Approve purchase", Message: "Please approve",
		Assignments: []AssignmentRecord{
			{ID: "assignment-a", Principal: "reviewer-a", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseApprove}},
			{ID: "assignment-b", Principal: "reviewer-b", Status: contract.AssignmentResponded},
		}}
	view, err := projector.Request(context.Background(), actor, record)
	if err != nil || view.Subject == nil || *view.Subject != *subject || len(view.Assignments) != 1 || view.Assignments[0].Assignee.ID != "reviewer-a" || len(view.AllowedResponses) != 1 || view.AllowedResponses[0] != contract.ResponseApprove {
		t.Fatalf("approval participant projection=%+v err=%v", view, err)
	}
	other := DecisionRecord{ID: "decision-b", RequestID: record.ID, AssignmentID: record.Assignments[1].ID, Response: contract.ResponseReject, Comment: "Private reason"}
	if _, err := projector.Response(context.Background(), actor, record, other); err == nil {
		t.Fatal("approval participant received another reviewer's response")
	}
	actorB := contract.PrincipalID("reviewer-b")
	entry := contract.RequestHistoryEntry{Sequence: "4", Kind: contract.HistoryResponded, Actor: &actorB, AssignmentID: record.Assignments[1].ID, Response: &responseApprove, Comment: "Private"}
	hidden, err := projector.History(context.Background(), actor, record, entry)
	if err != nil || hidden.Actor != nil || hidden.AssignmentID != "" || hidden.Response != nil || hidden.Comment != "" || hidden.Sequence != entry.Sequence {
		t.Fatalf("approval history privacy projection=%+v err=%v", hidden, err)
	}
	if _, err := projector.Request(context.Background(), projectionExecution(t, "unrelated"), record); err == nil {
		t.Fatal("unrelated tenant user received approval details")
	}
}

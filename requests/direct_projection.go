package requests

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

// DirectTaskProjector exposes subject-free direct tasks only to the requester
// and the requester's own assignment. It is not suitable for governed or
// workflow-originated requests, which require a domain-owned projector.
type DirectTaskProjector struct{}

// ParticipantProjector combines the strict direct-task projection with the
// minimal subject-bound approval projection. Authorization is still performed
// by the service and domain policy before projection; this type only limits
// fields and participant history visible to the current requester/assignee.
type ParticipantProjector struct{}

func (p ParticipantProjector) Request(ctx context.Context, exec policy.Execution, record RequestRecord) (contract.RequestView, error) {
	if directTaskRecord(record) {
		return (DirectTaskProjector{}).Request(ctx, exec, record)
	}
	if !subjectApprovalRecord(record) || !requestRecordVisible(record, contract.PrincipalID(exec.Actor().PrincipalID)) {
		return contract.RequestView{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	view := contract.RequestView{ID: record.ID, Revision: record.Revision, Kind: record.Kind, Status: record.Status,
		Requester: record.Requester, Subject: record.Subject, Title: record.Title, Message: record.Message,
		Source: record.Source, DueAt: cloneTime(record.DueAt), Outcome: cloneOutcome(record.Outcome),
		ResolvedAt: cloneTime(record.ResolvedAt), CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		Assignments: make([]contract.AssignmentView, 0, len(record.Assignments)), AllowedResponses: []contract.ResponseKind{}}
	if record.Continuation != nil {
		continuation := *record.Continuation
		view.Continuation = &continuation
	}
	for _, assignment := range record.Assignments {
		if actor != record.Requester && assignment.Principal != actor {
			continue
		}
		view.Assignments = append(view.Assignments, contract.AssignmentView{ID: assignment.ID, Stage: assignment.Stage,
			Assignee: contract.Assignee{Kind: contract.AssigneeUser, ID: string(assignment.Principal)},
			Status:   assignment.Status, AllowedResponses: append([]contract.ResponseKind(nil), assignment.AllowedResponses...)})
		if actor == assignment.Principal && assignment.Status == contract.AssignmentPending {
			view.AllowedResponses = append(view.AllowedResponses, assignment.AllowedResponses...)
		}
	}
	view.AllowedResponses = uniqueResponsesFrom(view.AllowedResponses)
	return view, nil
}

func (p ParticipantProjector) History(ctx context.Context, exec policy.Execution, record RequestRecord, entry contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error) {
	if directTaskRecord(record) {
		return (DirectTaskProjector{}).History(ctx, exec, record, entry)
	}
	if _, err := p.Request(ctx, exec, record); err != nil {
		return contract.RequestHistoryEntry{}, err
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	if actor != record.Requester && !historyEntryVisibleToAssignee(record, actor, entry) {
		entry.Actor = nil
		entry.AssignmentID = ""
		entry.PreviousAssignee = nil
		entry.NewAssignee = nil
		entry.Response = nil
		entry.Comment = ""
	}
	return entry, nil
}

func (p ParticipantProjector) Response(ctx context.Context, exec policy.Execution, record RequestRecord, value DecisionRecord) (contract.ResponseDetail, error) {
	if directTaskRecord(record) {
		return (DirectTaskProjector{}).Response(ctx, exec, record, value)
	}
	if _, err := p.Request(ctx, exec, record); err != nil {
		return contract.ResponseDetail{}, err
	}
	if value.RequestID != record.ID {
		return contract.ResponseDetail{}, contract.NewError(contract.CodeNotFound, "response not found")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	if actor != record.Requester {
		ownDecision := false
		for _, assignment := range record.Assignments {
			if assignment.ID == value.AssignmentID && assignment.Principal == actor {
				ownDecision = true
				break
			}
		}
		if !ownDecision {
			return contract.ResponseDetail{}, contract.NewError(contract.CodeNotFound, "response not found")
		}
	}
	return contract.ResponseDetail{DecisionID: value.ID, RequestID: value.RequestID, AssignmentID: value.AssignmentID,
		Response: value.Response, Comment: value.Comment, At: value.At}, nil
}

func (DirectTaskProjector) Request(_ context.Context, exec policy.Execution, record RequestRecord) (contract.RequestView, error) {
	if !directTaskRecord(record) || !requestRecordVisible(record, contract.PrincipalID(exec.Actor().PrincipalID)) {
		return contract.RequestView{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	view := contract.RequestView{ID: record.ID, Revision: record.Revision, Kind: record.Kind, Status: record.Status,
		Requester: record.Requester, Title: record.Title, Message: record.Message, DueAt: cloneTime(record.DueAt),
		Outcome: cloneOutcome(record.Outcome), ResolvedAt: cloneTime(record.ResolvedAt),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Assignments: make([]contract.AssignmentView, 0, len(record.Assignments)),
		AllowedResponses: []contract.ResponseKind{}}
	if record.Continuation != nil {
		continuation := *record.Continuation
		view.Continuation = &continuation
	}
	for _, assignment := range record.Assignments {
		if actor != record.Requester && assignment.Principal != actor {
			continue
		}
		view.Assignments = append(view.Assignments, contract.AssignmentView{
			ID: assignment.ID, Stage: assignment.Stage,
			Assignee: contract.Assignee{Kind: contract.AssigneeUser, ID: string(assignment.Principal)},
			Status:   assignment.Status, AllowedResponses: append([]contract.ResponseKind(nil), assignment.AllowedResponses...),
		})
		if actor == assignment.Principal && assignment.Status == contract.AssignmentPending {
			view.AllowedResponses = append(view.AllowedResponses, assignment.AllowedResponses...)
		}
	}
	view.AllowedResponses = uniqueResponsesFrom(view.AllowedResponses)
	return view, nil
}

func (p DirectTaskProjector) History(ctx context.Context, exec policy.Execution, record RequestRecord, entry contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error) {
	if _, err := p.Request(ctx, exec, record); err != nil {
		return contract.RequestHistoryEntry{}, err
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	if actor != record.Requester && !historyEntryVisibleToAssignee(record, actor, entry) {
		// Keep the immutable event's sequence/kind/time so participants see that
		// the request advanced, while withholding another assignment's identity,
		// decision, and private comment.
		entry.Actor = nil
		entry.AssignmentID = ""
		entry.PreviousAssignee = nil
		entry.NewAssignee = nil
		entry.Response = nil
		entry.Comment = ""
	}
	return entry, nil
}

func (p DirectTaskProjector) Response(ctx context.Context, exec policy.Execution, record RequestRecord, value DecisionRecord) (contract.ResponseDetail, error) {
	if _, err := p.Request(ctx, exec, record); err != nil {
		return contract.ResponseDetail{}, err
	}
	if value.RequestID != record.ID {
		return contract.ResponseDetail{}, contract.NewError(contract.CodeNotFound, "response not found")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	if actor != record.Requester {
		ownDecision := false
		for _, assignment := range record.Assignments {
			if assignment.ID == value.AssignmentID && assignment.Principal == actor {
				ownDecision = true
				break
			}
		}
		if !ownDecision {
			return contract.ResponseDetail{}, contract.NewError(contract.CodeNotFound, "response not found")
		}
	}
	return contract.ResponseDetail{DecisionID: value.ID, RequestID: value.RequestID, AssignmentID: value.AssignmentID,
		Response: value.Response, Comment: value.Comment, At: value.At}, nil
}

func historyEntryVisibleToAssignee(record RequestRecord, actor contract.PrincipalID, entry contract.RequestHistoryEntry) bool {
	if entry.Actor != nil && *entry.Actor == actor {
		return true
	}
	if entry.PreviousAssignee != nil && entry.PreviousAssignee.Kind == contract.AssigneeUser && entry.PreviousAssignee.ID == string(actor) {
		return true
	}
	if entry.NewAssignee != nil && entry.NewAssignee.Kind == contract.AssigneeUser && entry.NewAssignee.ID == string(actor) {
		return true
	}
	for _, assignment := range record.Assignments {
		if assignment.ID == entry.AssignmentID && assignment.Principal == actor {
			return true
		}
	}
	return false
}

func directTaskRecord(record RequestRecord) bool {
	return record.Kind == contract.RequestTask && record.Subject == nil && record.PolicyID == "" && record.InformationSchema == nil && record.Source == nil
}

func subjectApprovalRecord(record RequestRecord) bool {
	return record.Kind == contract.RequestApproval && record.Subject != nil && record.PolicyID != "" && record.InformationSchema == nil
}

func cloneOutcome(value *contract.RequestOutcome) *contract.RequestOutcome {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func uniqueResponsesFrom(values []contract.ResponseKind) []contract.ResponseKind {
	seen := make(map[contract.ResponseKind]struct{}, len(values))
	result := make([]contract.ResponseKind, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// NoResponseSchemas fails closed until a tenant/domain schema registry is
// assembled. DirectTaskAuthorizer rejects information requests before this
// adapter is reached.
type NoResponseSchemas struct{}

func (NoResponseSchemas) Registered(context.Context, contract.SchemaRef) error {
	return fmt.Errorf("information response schemas are not configured")
}

func (NoResponseSchemas) Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error {
	return fmt.Errorf("information response schemas are not configured")
}

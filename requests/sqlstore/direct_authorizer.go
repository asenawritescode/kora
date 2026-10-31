package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
)

// DirectTaskAuthorizer implements the policy for direct, subject-free user
// tasks. It deliberately does not authorize governed approvals, role/group
// expansion, or subject-bound requests; those decisions belong to a domain
// policy provider.
type DirectTaskAuthorizer struct {
	database *sql.DB
	dialect  db.QueryDialect
}

func NewDirectTaskAuthorizer(database *sql.DB, dialect db.QueryDialect) (*DirectTaskAuthorizer, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("direct task authorizer requires database and dialect")
	}
	return &DirectTaskAuthorizer{database: database, dialect: dialect}, nil
}

var _ requests.Authorizer = (*DirectTaskAuthorizer)(nil)

func (a *DirectTaskAuthorizer) Submission(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	if err := a.requireHuman(ctx, exec); err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	if cmd.Kind != contract.RequestTask || cmd.Subject != nil || cmd.PolicyID != "" || cmd.InformationSchema != nil || cmd.Assignment == nil {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "direct task policy does not authorize this request shape")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	targets := make([]contract.PrincipalID, 0, len(cmd.Assignment.Assignees))
	seen := make(map[contract.PrincipalID]struct{}, len(cmd.Assignment.Assignees))
	for _, target := range cmd.Assignment.Assignees {
		if target.Kind != contract.AssigneeUser {
			return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "direct task assignments must name users")
		}
		principal, err := contract.ParsePrincipalID(target.ID)
		if err != nil || principal == actor {
			return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "direct task assignee is invalid or is the requester")
		}
		if _, exists := seen[principal]; exists {
			return requests.SubmissionAuthorization{}, contract.NewError(contract.CodeValidationFailed, "duplicate direct task assignee")
		}
		seen[principal] = struct{}{}
		targets = append(targets, principal)
	}
	policyRevision, membershipRevision, err := a.authorizationRevisions(ctx)
	if err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	if _, err := a.activeMember(ctx, exec.Actor().Site, actor); err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	active, err := a.activeMembers(ctx, exec.Actor().Site, targets)
	if err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	if len(active) != len(targets) {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "every direct task assignee must be an active tenant user")
	}
	assignments := make([]requests.AssignmentRecord, len(targets))
	for i, principal := range targets {
		status := contract.AssignmentPending
		stage := uint16(1)
		if cmd.Assignment.Mode == contract.AssignmentSequential {
			stage = uint16(i + 1)
			if i > 0 {
				status = contract.AssignmentBlocked
			}
		}
		assignments[i] = requests.AssignmentRecord{
			Principal: principal, OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(principal)},
			Stage: stage, Status: status, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete, contract.ResponseReturn},
			MembershipRevision: membershipRevision,
		}
	}
	return requests.SubmissionAuthorization{PolicyRevision: policyRevision, MembershipRevision: membershipRevision,
		AssignmentMode: cmd.Assignment.Mode, Assignments: assignments}, nil
}

func (a *DirectTaskAuthorizer) Response(ctx context.Context, exec policy.Execution, record requests.RequestRecord, cmd contract.RespondRequest) (requests.ResponseAuthorization, error) {
	if err := a.requireHuman(ctx, exec); err != nil {
		return requests.ResponseAuthorization{}, err
	}
	if !isDirectTask(record) {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "no direct task policy applies to this request")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	eligible := false
	for _, assignment := range record.Assignments {
		if assignment.ID != cmd.AssignmentID || assignment.Principal != actor || assignment.Status != contract.AssignmentPending {
			continue
		}
		for _, allowed := range assignment.AllowedResponses {
			eligible = eligible || allowed == cmd.Response
		}
	}
	if !eligible {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "actor is not eligible for this task response")
	}
	policyRevision, membershipRevision, err := a.authorizationRevisions(ctx)
	if err != nil {
		return requests.ResponseAuthorization{}, err
	}
	if _, err := a.activeMember(ctx, exec.Actor().Site, actor); err != nil {
		return requests.ResponseAuthorization{}, err
	}
	return requests.ResponseAuthorization{PolicyRevision: policyRevision, MembershipRevision: membershipRevision}, nil
}

func (a *DirectTaskAuthorizer) Management(ctx context.Context, exec policy.Execution, record requests.RequestRecord, operation string, command *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	if err := a.requireHuman(ctx, exec); err != nil {
		return requests.ManagementAuthorization{}, err
	}
	if !isDirectTask(record) || record.Requester != contract.PrincipalID(exec.Actor().PrincipalID) {
		return requests.ManagementAuthorization{}, contract.NewError(contract.CodePermissionDenied, "only the requester may manage a direct task")
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	policyRevision, membershipRevision, err := a.authorizationRevisions(ctx)
	if err != nil {
		return requests.ManagementAuthorization{}, err
	}
	if _, err := a.activeMember(ctx, exec.Actor().Site, actor); err != nil {
		return requests.ManagementAuthorization{}, err
	}
	authorization := requests.ManagementAuthorization{PolicyRevision: policyRevision, MembershipRevision: membershipRevision}
	switch operation {
	case "cancel":
		return authorization, nil
	case "reassign":
		if command == nil || command.To.Kind != contract.AssigneeUser {
			return requests.ManagementAuthorization{}, contract.NewError(contract.CodePermissionDenied, "direct task reassignment must name a user")
		}
		principal, err := contract.ParsePrincipalID(command.To.ID)
		if err != nil || principal == actor {
			return requests.ManagementAuthorization{}, contract.NewError(contract.CodePermissionDenied, "direct task replacement assignee is invalid")
		}
		if _, err := a.activeMember(ctx, exec.Actor().Site, principal); err != nil {
			return requests.ManagementAuthorization{}, err
		}
		var current *requests.AssignmentRecord
		for i := range record.Assignments {
			if record.Assignments[i].ID == command.AssignmentID {
				current = &record.Assignments[i]
				break
			}
		}
		if current == nil || current.Status != contract.AssignmentPending {
			return requests.ManagementAuthorization{}, contract.NewError(contract.CodeConflict, "only a pending task assignment may be reassigned")
		}
		authorization.Replacement = &requests.AssignmentRecord{Principal: principal,
			OriginalTarget:   contract.Assignee{Kind: contract.AssigneeUser, ID: string(principal)},
			AllowedResponses: append([]contract.ResponseKind(nil), current.AllowedResponses...), MembershipRevision: membershipRevision}
		return authorization, nil
	default:
		return requests.ManagementAuthorization{}, contract.NewError(contract.CodePermissionDenied, "direct task operation is not authorized")
	}
}

func (a *DirectTaskAuthorizer) Visibility(ctx context.Context, exec policy.Execution) (requests.VisibilityScope, error) {
	if err := a.requireHuman(ctx, exec); err != nil {
		return requests.VisibilityScope{}, err
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	if _, err := a.activeMember(ctx, exec.Actor().Site, actor); err != nil {
		return requests.VisibilityScope{}, err
	}
	return requests.VisibilityScope{Tenant: exec.Tenant(), Actor: actor}, nil
}

func (a *DirectTaskAuthorizer) requireHuman(ctx context.Context, exec policy.Execution) error {
	if ctx == nil || !exec.Valid() || exec.Tenant() == "" {
		return contract.NewError(contract.CodeUnauthenticated, "verified tenant execution is required")
	}
	actor := exec.Actor()
	if actor.PrincipalType != contract.PrincipalHuman || actor.PrincipalID == "" || actor.SubjectUserID != actor.PrincipalID || strings.TrimSpace(actor.Site) == "" {
		return contract.NewError(contract.CodePermissionDenied, "direct task requests require the authenticated tenant user")
	}
	if _, err := contract.ParsePrincipalID(actor.PrincipalID); err != nil {
		return contract.NewError(contract.CodeUnauthenticated, "authenticated tenant user identity is invalid")
	}
	return ctx.Err()
}

func (a *DirectTaskAuthorizer) activeMember(ctx context.Context, site string, principal contract.PrincipalID) (bool, error) {
	var enabled bool
	err := a.database.QueryRowContext(ctx, db.Rebind(a.dialect, `SELECT enabled FROM _kora_user WHERE site = ? AND name = ?`), site, string(principal)).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !enabled) {
		return false, contract.NewError(contract.CodePermissionDenied, "tenant user is not active")
	}
	if err != nil {
		return false, fmt.Errorf("verify tenant user membership: %w", err)
	}
	return true, nil
}

func (a *DirectTaskAuthorizer) activeMembers(ctx context.Context, site string, principals []contract.PrincipalID) (map[contract.PrincipalID]struct{}, error) {
	if len(principals) == 0 || len(principals) > 50 {
		return nil, contract.NewError(contract.CodeValidationFailed, "direct task assignee count is invalid")
	}
	placeholders := make([]string, len(principals))
	args := make([]any, 0, len(principals)+1)
	args = append(args, site, db.BoolParam(a.dialect, true))
	for i, principal := range principals {
		placeholders[i] = "?"
		args = append(args, string(principal))
	}
	query := `SELECT name FROM _kora_user WHERE site = ? AND enabled = ? AND name IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := a.database.QueryContext(ctx, db.Rebind(a.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("resolve direct task assignees: %w", err)
	}
	defer rows.Close()
	active := make(map[contract.PrincipalID]struct{}, len(principals))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read direct task assignee: %w", err)
		}
		active[contract.PrincipalID(name)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate direct task assignees: %w", err)
	}
	return active, nil
}

func (a *DirectTaskAuthorizer) authorizationRevisions(ctx context.Context) (contract.Revision, contract.Revision, error) {
	var policyRevision, membershipRevision uint64
	err := a.database.QueryRowContext(ctx, db.Rebind(a.dialect, `SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1`)).Scan(&policyRevision, &membershipRevision)
	if err != nil {
		return "", "", fmt.Errorf("read authorization revisions: %w", err)
	}
	return contract.Revision(fmt.Sprint(policyRevision)), contract.Revision(fmt.Sprint(membershipRevision)), nil
}

func isDirectTask(record requests.RequestRecord) bool {
	return record.Kind == contract.RequestTask && record.Subject == nil && record.PolicyID == "" && record.InformationSchema == nil && record.Source == nil
}

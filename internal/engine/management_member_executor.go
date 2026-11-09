package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/site"
)

// managementMemberExecutor creates tenant members for Cloud invitations and
// answers the read-only plan for them. Cloud owns identity; the Engine is the
// only writer of `_kora_user` and the only judge of which roles may be given.
type managementMemberExecutor struct {
	acquire managementMemberAcquirer
}

func newManagementMemberExecutor(acquire managementMemberAcquirer) (*managementMemberExecutor, error) {
	if acquire == nil {
		return nil, errors.New("management member executor requires a tenant acquirer")
	}
	return &managementMemberExecutor{acquire: acquire}, nil
}

// memberQueryer is satisfied by *sql.DB (plan) and *sql.Tx (grant).
type memberQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type memberUserRow struct {
	name, site, email string
	enabled           bool
	passwordless      bool
}

func scanMemberUsers(rows *sql.Rows) ([]memberUserRow, error) {
	defer rows.Close()
	var users []memberUserRow
	for rows.Next() {
		var user memberUserRow
		var hash string
		if err := rows.Scan(&user.name, &user.site, &user.email, &user.enabled, &hash); err != nil {
			return nil, err
		}
		user.passwordless = hash == site.PasswordlessPasswordHash
		users = append(users, user)
	}
	return users, rows.Err()
}

// memberRoleProblem refuses the configured administrator role and any role not
// defined for this tenant. The role must match exactly: the database collation
// may fold case, the permission checks do not.
func memberRoleProblem(ctx context.Context, q memberQueryer, dialect db.Dialect, tenantSite, role string) (*contract.TenantMemberProblem, error) {
	if strings.EqualFold(role, doctype.AdminRole) {
		return &contract.TenantMemberProblem{Code: contract.TenantMemberRoleNotAssignable, Message: "the administrator role cannot be given through a team invitation"}, nil
	}
	rows, err := q.QueryContext(ctx, db.Rebind(dialect, `SELECT name FROM _kora_role WHERE name = ? AND (site = ? OR site = '')`), role, tenantSite)
	if err != nil {
		return nil, fmt.Errorf("read tenant roles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read tenant role: %w", err)
		}
		if name == role {
			return nil, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tenant roles: %w", err)
	}
	return &contract.TenantMemberProblem{Code: contract.TenantMemberRoleUnknown, Message: "the role does not exist in this tenant"}, nil
}

// memberEmailProblem judges the users that already hold the email (compared
// ignoring case; the unique index is case sensitive on some dialects). A
// disabled Cloud member may be invited again, every other holder blocks it.
// A plan has no Cloud subject yet, so a disabled Cloud member is assumed to be
// the invitee. A grant only gets here when no row holds its subject, so every
// holder of the email is another user.
// memberEmailCheck is the email lookup memberEmailProblem judges: planning
// is true for a read-only plan and false when a grant is applied.
type memberEmailCheck struct {
	dialect  db.Dialect
	site     string
	email    string
	planning bool
}

func memberEmailProblem(ctx context.Context, q memberQueryer, check memberEmailCheck) (*contract.TenantMemberProblem, error) {
	rows, err := q.QueryContext(ctx, db.Rebind(check.dialect, `SELECT name, site, email, enabled, password_hash FROM _kora_user WHERE site = ? AND LOWER(email) = LOWER(?)`), check.site, check.email)
	if err != nil {
		return nil, fmt.Errorf("read tenant users by email: %w", err)
	}
	holders, err := scanMemberUsers(rows)
	if err != nil {
		return nil, fmt.Errorf("read tenant users by email: %w", err)
	}
	for _, holder := range holders {
		switch {
		case check.planning && holder.passwordless && !holder.enabled:
		case check.planning && holder.passwordless:
			return &contract.TenantMemberProblem{Code: contract.TenantMemberAlreadyExists, Message: "this email already belongs to a member of this tenant"}, nil
		default:
			return &contract.TenantMemberProblem{Code: contract.TenantMemberEmailInUse, Message: "this email already belongs to another user of this tenant"}, nil
		}
	}
	return nil, nil
}

// PlanMember is the read-only check behind `POST .../members/plan`.
func (e *managementMemberExecutor) PlanMember(ctx context.Context, tenant contract.TenantID, request contract.PlanTenantMember) (contract.TenantMemberPlan, error) {
	if e == nil || e.acquire == nil || ctx == nil || contract.ValidatePlanTenantMember(request) != nil {
		return contract.TenantMemberPlan{}, contract.NewError(contract.CodeValidationFailed, "tenant member plan request is invalid")
	}
	target, release, err := e.acquire(ctx, tenant)
	if err != nil {
		return contract.TenantMemberPlan{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant is not ready to plan members")
	}
	defer release()
	var plan contract.TenantMemberPlan
	err = withTenantWorkAdmission(ctx, target.Admission, "tenant member plan", func() error {
		for _, check := range []func() (*contract.TenantMemberProblem, error){
			func() (*contract.TenantMemberProblem, error) {
				return memberRoleProblem(ctx, target.DB, target.Dialect, target.Site, request.Roles[0])
			},
			func() (*contract.TenantMemberProblem, error) {
				return memberEmailProblem(ctx, target.DB, memberEmailCheck{dialect: target.Dialect, site: target.Site, email: request.Email, planning: true})
			},
		} {
			problem, err := check()
			if err != nil {
				return err
			}
			if problem != nil {
				plan.Problems = append(plan.Problems, *problem)
			}
		}
		return nil
	})
	if err != nil {
		return contract.TenantMemberPlan{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant members could not be checked")
	}
	plan.Allowed = len(plan.Problems) == 0
	return plan, nil
}

func memberFailure(problem contract.TenantMemberProblem) management.FinishResult {
	code := contract.CodeConflict
	if problem.Code == contract.TenantMemberRoleUnknown || problem.Code == contract.TenantMemberRoleNotAssignable {
		code = contract.CodeValidationFailed
	}
	// The management error code set is frozen, so the member problem code
	// travels as the stable message prefix.
	return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(code, string(problem.Code)+": "+problem.Message)}
}

func (e *managementMemberExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	failed := func(message string) management.FinishResult {
		return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(contract.CodeValidationFailed, message)}
	}
	if e == nil || e.acquire == nil || ctx == nil || claim.Owner == "" || claim.Fence == 0 {
		return management.FinishResult{}, errors.New("management member executor requires a fenced claim")
	}
	operation := claim.Operation
	if operation.Kind != management.CommandTenantMemberGrant {
		return failed("operation kind is not enabled on this Engine deployment"), nil
	}
	var command contract.GrantTenantMember
	if json.Unmarshal(operation.Command, &command) != nil || contract.ValidateGrantTenantMember(command) != nil {
		return failed("stored tenant member grant command is invalid"), nil
	}
	target, release, err := e.acquire(ctx, operation.Tenant)
	if err != nil {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant runtime is unavailable for the member grant")}, nil
	}
	defer release()
	var problem *contract.TenantMemberProblem
	err = withTenantWorkAdmission(ctx, target.Admission, "tenant member grant", func() error {
		var grantErr error
		problem, grantErr = grantTenantMember(ctx, target, operation, command)
		return grantErr
	})
	if err != nil {
		slog.Error("tenant member grant outcome is uncertain", "operation_id", operation.ID, "tenant", operation.Tenant, "error", err)
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant member grant outcome is uncertain; retry the same invitation")}, nil
	}
	if problem != nil {
		return memberFailure(*problem), nil
	}
	return management.FinishResult{State: contract.OperationSucceeded}, nil
}

// grantTenantMember runs the whole grant in one tenant transaction. It returns
// a problem when the grant is refused and an error when the outcome is unknown.
// Bumping the membership revision first takes the write lock that serializes
// concurrent grants, so two invitations for one email cannot both pass the
// checks. A no-op replay rolls the bump back.
func grantTenantMember(ctx context.Context, target managementMemberRuntime, operation management.Operation, command contract.GrantTenantMember) (*contract.TenantMemberProblem, error) {
	tx, err := target.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := configstore.BumpAuthorizationRevisionTx(tx, target.Dialect, false, true); err != nil {
		return nil, err
	}
	role := command.Roles[0]
	rows, err := tx.QueryContext(ctx, db.Rebind(target.Dialect, `SELECT name, site, email, enabled, password_hash FROM _kora_user WHERE name = ?`), string(command.Subject))
	if err != nil {
		return nil, err
	}
	existing, err := scanMemberUsers(rows)
	if err != nil {
		return nil, err
	}
	if len(existing) == 1 {
		row := existing[0]
		if row.site != target.Site || !strings.EqualFold(row.email, command.Email) {
			return &contract.TenantMemberProblem{Code: contract.TenantMemberAlreadyExists, Message: "this Cloud user is already bound to a different tenant user"}, nil
		}
		if row.enabled {
			return nil, nil // replay or an existing member: never change what an enabled member holds.
		}
	}
	if problem, err := memberRoleProblem(ctx, tx, target.Dialect, target.Site, role); problem != nil || err != nil {
		return problem, err
	}
	if len(existing) == 1 {
		_, err = tx.ExecContext(ctx, db.Rebind(target.Dialect, `UPDATE _kora_user SET enabled = ?, roles = ?, full_name = ?, email_verified_at = COALESCE(email_verified_at, CURRENT_TIMESTAMP) WHERE site = ? AND name = ?`),
			true, role, command.FullName, target.Site, string(command.Subject))
	} else {
		if problem, emailErr := memberEmailProblem(ctx, tx, memberEmailCheck{dialect: target.Dialect, site: target.Site, email: command.Email}); problem != nil || emailErr != nil {
			return problem, emailErr
		}
		_, err = tx.ExecContext(ctx, db.Rebind(target.Dialect, `INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, email_verified_at, roles) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)`),
			string(command.Subject), target.Site, command.Email, site.PasswordlessPasswordHash, command.FullName, true, role)
	}
	if err != nil {
		return nil, err
	}
	if err := writeMemberGrantAudit(ctx, tx, target, operation, command); err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

// writeMemberGrantAudit records which invitation and which Cloud service
// principal created the member, in the same transaction as the change.
func writeMemberGrantAudit(ctx context.Context, tx *sql.Tx, target managementMemberRuntime, operation management.Operation, command contract.GrantTenantMember) error {
	correlation := command.InvitationID
	if len(correlation) > 64 {
		sum := sha256.Sum256([]byte(correlation))
		correlation = hex.EncodeToString(sum[:])
	}
	payload := sha256.Sum256(operation.Command)
	caller := string(operation.Caller)
	_, err := tx.ExecContext(ctx, db.Rebind(target.Dialect, `INSERT INTO _kora_operation_audit
		(id, site, operation_id, correlation_id, source, principal_type, principal_id, actor_user, command_name, doctype, doc_name, status, payload_hash)
		VALUES (?, ?, ?, ?, 'management', ?, ?, ?, ?, '_kora_user', ?, ?, ?)`),
		contract.NewEventID(), target.Site, string(operation.ID), correlation, string(contract.PrincipalService), caller, clipString(caller, 140),
		string(management.CommandTenantMemberGrant), string(command.Subject), string(contract.StatusCompleted), hex.EncodeToString(payload[:]))
	return err
}

func clipString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

var _ management.OperationExecutor = (*managementMemberExecutor)(nil)

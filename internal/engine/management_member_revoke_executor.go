package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/management"
	knet "github.com/asenawritescode/kora/net"
)

// managementMemberRuntime is the tenant-scoped state a member operation needs.
type managementMemberRuntime struct {
	Site      string
	DB        *sql.DB
	Dialect   db.Dialect
	Sessions  *auth.SessionManager
	Admission managementWorkAdmission
}

// managementMemberAcquirer pins a tenant runtime for one operation and returns
// the release function.
type managementMemberAcquirer func(context.Context, contract.TenantID) (managementMemberRuntime, func(), error)

// tenantMemberAcquirer resolves the tenant through the Engine router, the same
// way module lifecycle work does, and refuses an inconsistent runtime.
func tenantMemberAcquirer(tenants *knet.SiteRouter) managementMemberAcquirer {
	return func(ctx context.Context, tenant contract.TenantID) (managementMemberRuntime, func(), error) {
		if tenants == nil {
			return managementMemberRuntime{}, nil, errors.New("tenant router is unavailable")
		}
		handle, err := tenants.AcquireByID(ctx, string(tenant))
		if err != nil {
			return managementMemberRuntime{}, nil, err
		}
		runtime := handle.Snapshot()
		dialect := db.Resolve("")
		if runtime != nil {
			dialect = db.Resolve(runtime.DBType)
		}
		if runtime == nil || runtime.DB == nil || runtime.SessionManager == nil || dialect == nil || !runtime.HasServices || runtime.Services.WorkAdmission == nil || runtime.Activation.Tenant != tenant {
			handle.Release()
			return managementMemberRuntime{}, nil, errors.New("tenant runtime identity or resources are inconsistent")
		}
		return managementMemberRuntime{Site: runtime.Name, DB: runtime.DB, Dialect: dialect, Sessions: runtime.SessionManager, Admission: runtime.Services.WorkAdmission}, handle.Release, nil
	}
}

// managementMemberRevokeExecutor cuts a tenant member's access without
// deleting the _kora_user row that records point to as owner.
//
// The change is state-based, so it is idempotent: a reclaimed claim or a second
// operation for the same subject converges to the same disabled, role-less row.
// A stale worker that re-runs a finished revoke after a later grant of the same
// subject would revoke it again; Cloud must not re-grant before the revoke
// operation is terminal.
type managementMemberRevokeExecutor struct {
	acquire managementMemberAcquirer
}

func newManagementMemberRevokeExecutor(acquire managementMemberAcquirer) (*managementMemberRevokeExecutor, error) {
	if acquire == nil {
		return nil, errors.New("management member revoke executor requires a tenant runtime acquirer")
	}
	return &managementMemberRevokeExecutor{acquire: acquire}, nil
}

var (
	errMemberUnknown      = errors.New("tenant member is unknown")
	errMemberLastAdmin    = errors.New("tenant member is the last enabled administrator")
	errMemberRevokeSystem = errors.New("tenant member revocation could not be applied")
)

func (e *managementMemberRevokeExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	if e == nil || e.acquire == nil || ctx == nil || claim.Owner == "" || claim.Fence == 0 {
		return management.FinishResult{}, errors.New("management member revoke requires a configured executor, context and fenced claim")
	}
	failed := func(code contract.Code, message string) (management.FinishResult, error) {
		return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(code, message)}, nil
	}
	if claim.Operation.Kind != management.CommandTenantMemberRevoke {
		return failed(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment")
	}
	var command contract.RevokeTenantMember
	if err := json.Unmarshal(claim.Operation.Command, &command); err != nil || contract.ValidateRevokeTenantMember(command) != nil {
		return failed(contract.CodeValidationFailed, "stored tenant member revoke command is invalid")
	}
	// Nothing is changed before the transaction commits, and the operation is
	// idempotent, so an infrastructure failure is a clean failure: the caller
	// repeats the revoke with a new idempotency key.
	const retry = "tenant member revocation did not complete; repeat the revoke with a new idempotency key"
	runtime, release, err := e.acquire(ctx, claim.Operation.Tenant)
	if err != nil {
		return failed(contract.CodeDependencyUnavailable, "tenant runtime is unavailable; "+retry)
	}
	defer release()
	admitted, err := runtime.Admission.Acquire(ctx)
	if err != nil {
		return failed(contract.CodeDependencyUnavailable, "tenant database admission is unavailable; "+retry)
	}
	defer admitted()
	switch err := revokeTenantMember(ctx, runtime, string(command.Subject)); {
	case err == nil:
		return management.FinishResult{State: contract.OperationSucceeded}, nil
	case errors.Is(err, errMemberUnknown):
		return failed(contract.CodeNotFound, "the member is not a user of this tenant")
	case errors.Is(err, errMemberLastAdmin):
		return failed(contract.CodeConflict, "at least one enabled Administrator must remain")
	default:
		return failed(contract.CodeDependencyUnavailable, retry)
	}
}

// revokeTenantMember disables the user, clears its roles and deletes its
// sessions in one transaction. The authorization revision is bumped first: its
// write lock serializes concurrent access changes, so two revocations or demotions
// cannot both pass the last-Administrator check. The session cache of this
// instance is purged after the commit; other instances may serve a cached session
// for the session cache TTL at most (see auth.SessionManager.RevokeUserSessions).
func revokeTenantMember(ctx context.Context, rt managementMemberRuntime, subject string) error {
	tx, err := rt.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin: %v", errMemberRevokeSystem, err)
	}
	defer tx.Rollback()
	if err := configstore.BumpAuthorizationRevisionTx(tx, rt.Dialect, false, true); err != nil {
		return fmt.Errorf("%w: %v", errMemberRevokeSystem, err)
	}
	var roles string
	var enabled bool
	err = tx.QueryRowContext(ctx, db.Rebind(rt.Dialect, "SELECT COALESCE(roles, ''), enabled FROM _kora_user WHERE site = ? AND name = ?"), rt.Site, subject).Scan(&roles, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return errMemberUnknown
	}
	if err != nil {
		return fmt.Errorf("%w: load: %v", errMemberRevokeSystem, err)
	}
	if enabled && memberRolesContain(roles, doctype.AdminRole) {
		others, err := api.CountOtherEnabledAdministrators(ctx, tx, rt.Dialect, rt.Site, subject)
		if err != nil {
			return fmt.Errorf("%w: count administrators: %v", errMemberRevokeSystem, err)
		}
		if others == 0 {
			return errMemberLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(rt.Dialect, "UPDATE _kora_user SET enabled = ?, roles = '', modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?"), db.BoolParam(rt.Dialect, false), rt.Site, subject); err != nil {
		return fmt.Errorf("%w: update: %v", errMemberRevokeSystem, err)
	}
	if err := rt.Sessions.RevokeUserSessionsTx(ctx, tx, rt.Site, subject); err != nil {
		return fmt.Errorf("%w: %v", errMemberRevokeSystem, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit: %v", errMemberRevokeSystem, err)
	}
	rt.Sessions.PurgeUserCache(rt.Site, subject)
	return nil
}

func memberRolesContain(roles, role string) bool {
	for _, candidate := range strings.Split(roles, ",") {
		if strings.TrimSpace(candidate) == role {
			return true
		}
	}
	return false
}

var _ management.OperationExecutor = (*managementMemberRevokeExecutor)(nil)

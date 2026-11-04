package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/management"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

type managementInvitationDirectory interface {
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
}

type managementInvitationExecutor struct {
	directory        managementInvitationDirectory
	coordinator      managementStateCoordinator
	admission        managementWorkAdmission
	tenants          *knet.SiteRouter
	mailer           auth.MagicLinkMailer
	publicAppBaseURL string
}

func (e *managementInvitationExecutor) ReconcileInvitation(ctx context.Context, operation management.Operation, command contract.ReconcileTenantInvitationOperation) error {
	if ctx == nil || e == nil || e.directory == nil || e.coordinator == nil || e.admission == nil || e.tenants == nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant invitation reconciliation is unavailable")
	}
	if operation.Kind != management.CommandTenantInvitation || operation.State != contract.OperationRecoveryRequired || contract.ValidateReconcileTenantInvitationOperation(command) != nil {
		return contract.NewError(contract.CodeConflict, "only an uncertain tenant invitation can be reconciled")
	}
	var original contract.RequestTenantInvitation
	if err := json.Unmarshal(operation.Command, &original); err != nil || contract.ValidateRequestTenantInvitation(original) != nil {
		return contract.NewError(contract.CodeConflict, "stored tenant invitation command is invalid")
	}
	releasePlatform, err := e.admission.Acquire(ctx)
	if err != nil {
		return contract.NewError(contract.CodeResourceExhausted, "tenant invitation reconciliation admission is unavailable")
	}
	info, err := e.directory.GetByIDContext(ctx, string(operation.Tenant))
	releasePlatform()
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant directory could not verify invitation identity")
	}
	if info.SiteID != string(operation.Tenant) || info.Status != "active" || info.Name == "" {
		return contract.NewError(contract.CodeConflict, "tenant identity is no longer active for invitation reconciliation")
	}
	if err := e.coordinator.ActivateFromDirectory(ctx, info.SiteID); err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant runtime activation is unavailable")
	}
	handle, err := e.tenants.AcquireByID(ctx, info.SiteID)
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant runtime is unavailable")
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || !runtime.HasServices || runtime.Services.WorkAdmission == nil || runtime.Activation.Tenant != operation.Tenant || runtime.Name != info.Name {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant runtime identity or admission is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return contract.NewError(contract.CodeUnsupportedVersion, "tenant database dialect is unsupported")
	}
	releaseTenant, err := runtime.Services.WorkAdmission.Acquire(ctx)
	if err != nil {
		return contract.NewError(contract.CodeResourceExhausted, "tenant invitation reconciliation admission is unavailable")
	}
	defer releaseTenant()
	tx, err := runtime.DB.BeginTx(ctx, nil)
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant invitation receipt could not be opened")
	}
	defer tx.Rollback()
	var tenant, emailAddress, status, linkID string
	var fence int64
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT tenant_id, email, status, magic_link_id, worker_fence FROM _kora_magic_link_delivery WHERE operation_id = ?`), string(operation.ID)).Scan(&tenant, &emailAddress, &status, &linkID, &fence); err != nil {
		return contract.NewError(contract.CodeConflict, "tenant invitation receipt is missing")
	}
	if tenant != string(operation.Tenant) || emailAddress != original.Email || fence < 1 || linkID == "" {
		return contract.NewError(contract.CodeConflict, "tenant invitation receipt identity does not match operation")
	}
	if (command.Outcome == contract.InvitationDelivered && status == "accepted") || (command.Outcome == contract.InvitationNotDelivered && status == "retryable") {
		return tx.Commit()
	}
	if status != "unknown" {
		return contract.NewError(contract.CodeConflict, "invitation receipt is no longer uncertain")
	}
	now := time.Now().UTC()
	newStatus := "accepted"
	if command.Outcome == contract.InvitationNotDelivered {
		newStatus = "retryable"
	}
	result, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link_delivery SET status = ?, updated_at = ? WHERE operation_id = ? AND tenant_id = ? AND email = ? AND status = 'unknown' AND worker_fence = ?`), newStatus, now, string(operation.ID), tenant, emailAddress, fence)
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant invitation receipt could not be reconciled")
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return contract.NewError(contract.CodeConflict, "tenant invitation receipt changed during reconciliation")
	}
	if command.Outcome == contract.InvitationNotDelivered {
		if err := revokeInvitationForReconciliation(ctx, tx, dialect, tenant, emailAddress, linkID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "tenant invitation reconciliation could not be committed")
	}
	return nil
}

func revokeInvitationForReconciliation(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant, emailAddress, linkID string, now time.Time) error {
	var usedAt, revokedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT used_at, revoked_at FROM _kora_magic_link WHERE id = ? AND site = ? AND email = ?`), linkID, tenant, emailAddress).Scan(&usedAt, &revokedAt); err != nil {
		return contract.NewError(contract.CodeConflict, "uncertain invitation token is missing or does not match its receipt")
	}
	if usedAt.Valid {
		return contract.NewError(contract.CodeConflict, "invitation token has already been used; delivered is the only safe provider outcome")
	}
	if revokedAt.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link SET revoked_at = ? WHERE id = ? AND site = ? AND email = ? AND used_at IS NULL AND revoked_at IS NULL`), now, linkID, tenant, emailAddress); err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "uncertain invitation token could not be revoked")
	}
	return nil
}

func newManagementInvitationExecutor(directory managementInvitationDirectory, coordinator managementStateCoordinator, admission managementWorkAdmission, tenants *knet.SiteRouter, mailer auth.MagicLinkMailer, publicAppBaseURL string) (*managementInvitationExecutor, error) {
	if directory == nil || coordinator == nil || admission == nil || tenants == nil || mailer == nil {
		return nil, errors.New("management invitation executor requires tenant directory, runtime coordinator, bounded admission, router, and configured mailer")
	}
	if err := auth.ValidatePublicAppBaseURL(publicAppBaseURL); err != nil {
		return nil, fmt.Errorf("invalid public application base URL: %w", err)
	}
	return &managementInvitationExecutor{directory: directory, coordinator: coordinator, admission: admission, tenants: tenants, mailer: mailer, publicAppBaseURL: publicAppBaseURL}, nil
}

func (e *managementInvitationExecutor) magicLinkURL(tenant, token string) (string, error) {
	if e != nil && strings.TrimSpace(e.publicAppBaseURL) != "" {
		return auth.TenantPathMagicLinkURL(e.publicAppBaseURL, tenant, token)
	}
	return auth.TenantMagicLinkURL(tenant, token)
}

func (e *managementInvitationExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	if e == nil || e.directory == nil || e.coordinator == nil || e.admission == nil || e.tenants == nil || e.mailer == nil {
		return management.FinishResult{}, errors.New("management invitation executor is not configured")
	}
	if ctx == nil || claim.Owner == "" || claim.Fence == 0 || claim.Fence > math.MaxInt64 {
		return management.FinishResult{}, errors.New("management invitation requires a context and valid fenced claim")
	}
	failed := func(code contract.Code, message string) (management.FinishResult, error) {
		return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(code, message)}, nil
	}
	recovery := func(message string) (management.FinishResult, error) {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeInternal, message)}, nil
	}
	if claim.Operation.Kind != management.CommandTenantInvitation {
		return failed(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment")
	}
	var command contract.RequestTenantInvitation
	if err := json.Unmarshal(claim.Operation.Command, &command); err != nil || contract.ValidateRequestTenantInvitation(command) != nil {
		return failed(contract.CodeValidationFailed, "stored tenant invitation command is invalid")
	}
	if _, err := contract.ParseTenantID(string(claim.Operation.Tenant)); err != nil {
		return failed(contract.CodeValidationFailed, "stored tenant invitation identity is invalid")
	}
	releasePlatform, err := e.admission.Acquire(ctx)
	if err != nil {
		return recovery("tenant invitation platform admission needs recovery")
	}
	info, err := e.directory.GetByIDContext(ctx, string(claim.Operation.Tenant))
	releasePlatform()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return failed(contract.CodeNotFound, "tenant was not found in the Engine directory")
		}
		return recovery("tenant invitation directory lookup needs recovery")
	}
	if info.SiteID != string(claim.Operation.Tenant) || info.Status != "active" || info.Name == "" {
		return failed(contract.CodeConflict, "tenant invitation requires the same active tenant identity")
	}
	ownedAlias := false
	for _, alias := range info.Domains {
		if alias == info.Name {
			ownedAlias = true
			break
		}
	}
	if !ownedAlias {
		return failed(contract.CodeConflict, "tenant directory does not confirm ownership of its canonical invitation host")
	}
	if err := e.coordinator.ActivateFromDirectory(ctx, info.SiteID); err != nil {
		return recovery("tenant runtime activation needs recovery")
	}
	handle, err := e.tenants.AcquireByID(ctx, info.SiteID)
	if err != nil {
		return recovery("tenant runtime acquisition needs recovery")
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.SessionManager == nil || runtime.Activation.Tenant != claim.Operation.Tenant || runtime.Name != info.Name {
		return recovery("tenant runtime identity or resources are inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return failed(contract.CodeUnsupportedVersion, "tenant database dialect is unsupported for invitation delivery")
	}
	if !runtime.HasServices || runtime.Services.WorkAdmission == nil {
		return recovery("tenant invitation database admission is unavailable")
	}
	tenantAdmission := runtime.Services.WorkAdmission
	releaseTenant, err := tenantAdmission.Acquire(ctx)
	if err != nil {
		return recovery("tenant invitation database admission wait needs recovery")
	}
	defer releaseTenant()
	retryApproved := false
	if accepted, result, err := invitationReceipt(ctx, runtime.DB, dialect, claim, command); err != nil {
		return recovery("tenant invitation receipt lookup needs recovery")
	} else if accepted {
		return result, nil
	} else {
		retryApproved = result.State == contract.OperationRequested
	}
	user, err := runtime.SessionManager.FindUserByEmailContext(ctx, runtime.Name, command.Email)
	if err != nil {
		return recovery("tenant invitation administrator lookup needs recovery")
	}
	if user == nil {
		if retryApproved {
			return recovery("operator-approved invitation retry is waiting for the administrator account to be available")
		}
		return failed(contract.CodeNotFound, "the requested bootstrap administrator is unavailable or disabled")
	}
	token, err := auth.NewMagicLinkToken()
	if err != nil {
		return recovery("tenant invitation token generation needs recovery")
	}
	loginURL, err := e.magicLinkURL(info.Name, token)
	if err != nil {
		return failed(contract.CodeValidationFailed, "tenant canonical host cannot be used for a secure invitation link")
	}
	linkID := fmt.Sprintf("ml-%s-%d", claim.Operation.ID, claim.Fence)
	var prepareErr error
	if retryApproved {
		prepareErr = retryInvitationDelivery(ctx, runtime.DB, dialect, claim, command.Email, linkID, auth.MagicLinkTokenHash(token))
	} else {
		prepareErr = beginInvitationDelivery(ctx, runtime.DB, dialect, claim, command.Email, linkID, auth.MagicLinkTokenHash(token))
	}
	if prepareErr != nil {
		if accepted, result, lookupErr := invitationReceipt(ctx, runtime.DB, dialect, claim, command); lookupErr == nil && accepted {
			return result, nil
		}
		return recovery("tenant invitation attempt could not be safely prepared")
	}
	subject, textBody, htmlBody := email.MagicLinkTemplate("Kora", loginURL, 15)
	if err := e.mailer.SendContext(ctx, &email.Message{To: []string{user.Email}, Subject: subject, TextBody: textBody, HTMLBody: htmlBody}); err != nil {
		if err := recordInvitationOutcome(ctx, runtime.DB, dialect, claim, "unknown", true); err != nil {
			return recovery("tenant invitation delivery outcome is uncertain and its receipt needs recovery")
		}
		return recovery("tenant invitation delivery outcome is uncertain; inspect the provider before issuing another link")
	}
	if err := recordInvitationOutcome(ctx, runtime.DB, dialect, claim, "accepted", false); err != nil {
		return recovery("email provider accepted the invitation but its durable receipt needs recovery")
	}
	return management.FinishResult{State: contract.OperationSucceeded}, nil
}

func invitationReceipt(ctx context.Context, database *sql.DB, dialect db.Dialect, claim management.Claim, command contract.RequestTenantInvitation) (bool, management.FinishResult, error) {
	var tenant, email, status string
	var fence int64
	err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT tenant_id, email, status, worker_fence FROM _kora_magic_link_delivery WHERE operation_id = ?`), string(claim.Operation.ID)).Scan(&tenant, &email, &status, &fence)
	if errors.Is(err, sql.ErrNoRows) {
		return false, management.FinishResult{}, nil
	}
	if err != nil {
		return false, management.FinishResult{}, err
	}
	if tenant != string(claim.Operation.Tenant) || email != command.Email || fence < 1 {
		return false, management.FinishResult{}, fmt.Errorf("stored invitation receipt identity does not match operation")
	}
	switch status {
	case "accepted":
		return true, management.FinishResult{State: contract.OperationSucceeded}, nil
	case "sending", "unknown":
		// A reclaimed worker must fence the previous sender before returning
		// recovery-required. The prior SMTP outcome cannot safely be repeated.
		if uint64(fence) < claim.Fence {
			if err := fenceUncertainInvitation(ctx, database, dialect, claim, fence); err != nil {
				return false, management.FinishResult{}, err
			}
		}
		return true, management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeOutcomeUnknown, "invitation delivery outcome is uncertain; inspect the email provider before retrying")}, nil
	case "retryable":
		return false, management.FinishResult{State: contract.OperationRequested}, nil
	default:
		return false, management.FinishResult{}, fmt.Errorf("stored invitation receipt state is invalid")
	}
}

func fenceUncertainInvitation(ctx context.Context, database *sql.DB, dialect db.Dialect, claim management.Claim, previousFence int64) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var linkID string
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT magic_link_id FROM _kora_magic_link_delivery WHERE operation_id = ? AND status = 'sending' AND worker_fence = ?`), string(claim.Operation.ID), previousFence).Scan(&linkID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link_delivery SET status = 'unknown', worker_fence = ?, updated_at = ? WHERE operation_id = ? AND status = 'sending' AND worker_fence = ?`), int64(claim.Fence), now, string(claim.Operation.ID), previousFence)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("tenant invitation could not fence previous SMTP attempt")
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND used_at IS NULL`), now, linkID); err != nil {
		return err
	}
	return tx.Commit()
}

func beginInvitationDelivery(ctx context.Context, database *sql.DB, dialect db.Dialect, claim management.Claim, emailAddress, linkID, tokenHash string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_magic_link (id, site, email, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`),
		linkID, string(claim.Operation.Tenant), emailAddress, tokenHash, now.Add(15*time.Minute), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_magic_link_delivery (operation_id, tenant_id, email, magic_link_id, status, worker_fence, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		string(claim.Operation.ID), string(claim.Operation.Tenant), emailAddress, linkID, "sending", int64(claim.Fence), now, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func recordInvitationOutcome(ctx context.Context, database *sql.DB, dialect db.Dialect, claim management.Claim, status string, revoke bool) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC()
	var linkID string
	if revoke {
		if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT magic_link_id FROM _kora_magic_link_delivery WHERE operation_id = ? AND status = 'sending' AND worker_fence = ?`), string(claim.Operation.ID), int64(claim.Fence)).Scan(&linkID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link_delivery SET status = ?, updated_at = ? WHERE operation_id = ? AND status = 'sending' AND worker_fence = ?`), status, now, string(claim.Operation.ID), int64(claim.Fence))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("tenant invitation claim fence is no longer current")
	}
	if revoke {
		if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND used_at IS NULL`), now, linkID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func retryInvitationDelivery(ctx context.Context, database *sql.DB, dialect db.Dialect, claim management.Claim, emailAddress, linkID, tokenHash string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tenant, storedEmail, status string
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT tenant_id, email, status FROM _kora_magic_link_delivery WHERE operation_id = ?`), string(claim.Operation.ID)).Scan(&tenant, &storedEmail, &status); err != nil {
		return err
	}
	if tenant != string(claim.Operation.Tenant) || storedEmail != emailAddress || status != "retryable" {
		return fmt.Errorf("invitation is not in an operator-approved retry state")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_magic_link (id, site, email, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`), linkID, tenant, emailAddress, tokenHash, now.Add(15*time.Minute), now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_magic_link_delivery SET magic_link_id = ?, status = 'sending', worker_fence = ?, updated_at = ? WHERE operation_id = ? AND tenant_id = ? AND email = ? AND status = 'retryable'`), linkID, int64(claim.Fence), now, string(claim.Operation.ID), tenant, emailAddress)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("invitation retry state changed concurrently")
	}
	return tx.Commit()
}

var _ management.OperationExecutor = (*managementInvitationExecutor)(nil)

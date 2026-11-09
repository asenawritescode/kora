package engine

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
)

func TestManagementInvitationMagicLinkUsesConfiguredWorkspaceBase(t *testing.T) {
	executor := &managementInvitationExecutor{publicAppBaseURL: "http://localhost:5173"}
	link, err := executor.magicLinkURL("clientele", "token+value")
	if err != nil {
		t.Fatal("build shared workspace invitation URL:", err)
	}
	const want = "http://localhost:5173/s/clientele/workspace/auth/login?magic_token=token%2Bvalue"
	if link != want {
		t.Fatalf("invitation URL = %q, want %q", link, want)
	}
}

func TestManagementInvitationMagicLinkKeepsHostBasedCompatibility(t *testing.T) {
	executor := &managementInvitationExecutor{}
	link, err := executor.magicLinkURL("clientele.example.test", "token")
	if err != nil {
		t.Fatal("build tenant-host invitation URL:", err)
	}
	const want = "https://clientele.example.test/workspace/auth/login?magic_token=token"
	if link != want {
		t.Fatalf("invitation URL = %q, want %q", link, want)
	}
}

func invitationClaim(fence uint64) management.Claim {
	return management.Claim{Owner: "worker-a", Fence: fence, Operation: management.Operation{ID: "operation-invite-1", Tenant: "tenant-123456789", Kind: management.CommandTenantInvitation}}
}

func TestInvitationDeliveryJournalCommitsIntentBeforeSMTP(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dialect := db.Resolve("mysql")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_magic_link (id, site, email, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)")).WithArgs("ml-operation-invite-1", "tenant-123456789", "owner@example.test", "token-hash", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_magic_link_delivery (operation_id, tenant_id, email, magic_link_id, status, worker_fence, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")).WithArgs("operation-invite-1", "tenant-123456789", "owner@example.test", "ml-operation-invite-1", "sending", int64(1), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := beginInvitationDelivery(context.Background(), invitationLedger{database: database, dialect: dialect, claim: invitationClaim(1)}, invitationLink{email: "owner@example.test", id: "ml-operation-invite-1", tokenHash: "token-hash"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationReceiptAcceptedAndUnknownAreTerminalForAutomaticRetry(t *testing.T) {
	for _, test := range []struct {
		status    string
		wantState contract.OperationState
	}{
		{status: "accepted", wantState: contract.OperationSucceeded},
		{status: "unknown", wantState: contract.OperationRecoveryRequired},
	} {
		t.Run(test.status, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			claim := invitationClaim(2)
			command := contract.RequestTenantInvitation{Email: "owner@example.test"}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT tenant_id, email, status, worker_fence FROM _kora_magic_link_delivery WHERE operation_id = ?")).WithArgs(string(claim.Operation.ID)).WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "email", "status", "worker_fence"}).AddRow(string(claim.Operation.Tenant), command.Email, test.status, int64(2)))
			found, result, err := invitationReceipt(context.Background(), database, db.Resolve("mysql"), claim, command)
			if err != nil || !found || result.State != test.wantState {
				t.Fatalf("receipt=(%v,%+v,%v), want state %q", found, result, err, test.wantState)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReclaimedInvitationFencesAndRevokesUncertainLink(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	claim := invitationClaim(2)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT magic_link_id FROM _kora_magic_link_delivery WHERE operation_id = ? AND status = 'sending' AND worker_fence = ?")).WithArgs("operation-invite-1", int64(1)).WillReturnRows(sqlmock.NewRows([]string{"magic_link_id"}).AddRow("ml-operation-invite-1-1"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_magic_link_delivery SET status = 'unknown', worker_fence = ?, updated_at = ? WHERE operation_id = ? AND status = 'sending' AND worker_fence = ?")).WithArgs(int64(2), sqlmock.AnyArg(), "operation-invite-1", int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_magic_link SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND used_at IS NULL")).WithArgs(sqlmock.AnyArg(), "ml-operation-invite-1-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := fenceUncertainInvitation(context.Background(), database, db.Resolve("mysql"), claim, 1); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryableInvitationReceiptAllowsOnlyOperatorApprovedNewAttempt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	claim := invitationClaim(3)
	command := contract.RequestTenantInvitation{Email: "owner@example.test"}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT tenant_id, email, status, worker_fence FROM _kora_magic_link_delivery WHERE operation_id = ?")).WithArgs(string(claim.Operation.ID)).WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "email", "status", "worker_fence"}).AddRow(string(claim.Operation.Tenant), command.Email, "retryable", int64(2)))
	found, result, err := invitationReceipt(context.Background(), database, db.Resolve("mysql"), claim, command)
	if err != nil || found || result.State != contract.OperationRequested {
		t.Fatalf("retryable receipt=(%v,%+v,%v), want operator-approved new attempt", found, result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryInvitationDeliveryCreatesFreshSingleUseLink(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	claim := invitationClaim(3)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT tenant_id, email, status FROM _kora_magic_link_delivery WHERE operation_id = ?")).WithArgs("operation-invite-1").WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "email", "status"}).AddRow(string(claim.Operation.Tenant), "owner@example.test", "retryable"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_magic_link (id, site, email, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)")).WithArgs("ml-operation-invite-1-3", string(claim.Operation.Tenant), "owner@example.test", "new-token-hash", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_magic_link_delivery SET magic_link_id = ?, status = 'sending', worker_fence = ?, updated_at = ? WHERE operation_id = ? AND tenant_id = ? AND email = ? AND status = 'retryable'")).WithArgs("ml-operation-invite-1-3", int64(3), sqlmock.AnyArg(), "operation-invite-1", string(claim.Operation.Tenant), "owner@example.test").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := retryInvitationDelivery(context.Background(), invitationLedger{database: database, dialect: db.Resolve("mysql"), claim: claim}, invitationLink{email: "owner@example.test", id: "ml-operation-invite-1-3", tokenHash: "new-token-hash"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNotDeliveredReconciliationCannotReuseAUsedInvitationToken(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	usedAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT used_at, revoked_at FROM _kora_magic_link WHERE id = ? AND site = ? AND email = ?")).WithArgs("link-1", "tenant-123456789", "owner@example.test").WillReturnRows(sqlmock.NewRows([]string{"used_at", "revoked_at"}).AddRow(usedAt, nil))
	mock.ExpectRollback()
	err = revokeInvitationForReconciliation(context.Background(), tx, db.Resolve("mysql"), invitationRevocation{tenant: "tenant-123456789", email: "owner@example.test", linkID: "link-1", now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("used invitation token error=%v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

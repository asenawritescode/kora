package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
)

func TestAcceptPersistsOperationBeforeReturningRequested(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := testAcceptCommand()
	expectMissingByKey(mock, input)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ? FOR UPDATE")).
		WithArgs(string(input.Tenant), string(input.Caller), string(input.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation (id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)")).
		WithArgs(string(input.ID), string(input.Tenant), string(input.Caller), string(input.IdempotencyKey), string(input.Kind), management.CommandDigest(input.Kind, input.Command), string(input.Command), string(contract.OperationRequested), input.Now, input.Now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	got, err := store.Accept(context.Background(), input)
	if err != nil {
		t.Fatal("Accept:", err)
	}
	if got.ID != input.ID || got.State != contract.OperationRequested || got.CommandDigest != management.CommandDigest(input.Kind, input.Command) || string(got.Command) != string(input.Command) {
		t.Fatalf("accepted operation is incomplete: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestAcceptRegistersTenantMemberGrantKindOnly(t *testing.T) {
	for _, test := range []struct {
		name string
		kind management.CommandKind
		want bool
	}{
		{name: "tenant member grant", kind: management.CommandTenantMemberGrant, want: true},
		{name: "unknown kind", kind: management.CommandKind("tenant_member_unknown")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, mock, store := newTestStore(t)
			input := testAcceptCommand()
			input.Kind = test.kind
			input.Command = []byte(`{"subject":"usr_member_1"}`)
			expectMissingByKey(mock, input)
			if test.want {
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ? FOR UPDATE")).
					WithArgs(string(input.Tenant), string(input.Caller), string(input.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
				mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation")).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			}
			_, err := store.Accept(context.Background(), input)
			if test.want && err != nil {
				t.Fatalf("Accept(%s) = %v", test.kind, err)
			}
			if !test.want && (err == nil || !strings.Contains(err.Error(), "not registered")) {
				t.Fatalf("Accept(%s) = %v, want unregistered kind", test.kind, err)
			}
		})
	}
}

func TestAcceptReturnsErrorWhenInitialPersistenceFails(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := testAcceptCommand()
	expectMissingByKey(mock, input)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ? FOR UPDATE")).
		WithArgs(string(input.Tenant), string(input.Caller), string(input.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation (id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)")).
		WillReturnError(errors.New("disk unavailable"))
	mock.ExpectRollback()
	expectMissingByKey(mock, input)

	if _, err := store.Accept(context.Background(), input); err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("Accept error = %v, want persistence error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestAcceptReplaysIdenticalCommandAndRejectsChangedContent(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "changed"}[changed], func(t *testing.T) {
			_, mock, store := newTestStore(t)
			input := testAcceptCommand()
			priorPayload := input.Command
			priorDigest := management.CommandDigest(input.Kind, priorPayload)
			if changed {
				input.Command = []byte(`{"tenant_id":"tenant-2"}`)
			}
			expectStoredByKey(mock, input, input.ID, priorPayload, priorDigest)
			operation, err := store.Accept(context.Background(), input)
			if changed {
				if err == nil || !hasCode(err, contract.CodeIdempotencyKeyReused) {
					t.Fatalf("changed retry error = %v", err)
				}
			} else if err != nil || string(operation.Command) != string(priorPayload) || operation.ID != input.ID {
				t.Fatalf("same retry = %+v, %v", operation, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal("SQL expectations:", err)
			}
		})
	}
}

func TestGetScopesOperationToAuthenticatedManagementCaller(t *testing.T) {
	_, mock, store := newTestStore(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ?")).
		WithArgs("01K7OPERATION000000000000000000", "cloud-service").WillReturnError(sql.ErrNoRows)
	if _, err := store.Get(context.Background(), "cloud-service", "01K7OPERATION000000000000000000"); !hasCode(err, contract.CodeNotFound) {
		t.Fatalf("cross-caller operation lookup error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestClaimDueForDeploymentFiltersProvisioningJobsInDatabase(t *testing.T) {
	_, mock, store := newTestStore(t)
	mock.ExpectBegin()
	query := "SELECT " + operationColumns + ",lease_fence FROM _kora_managed_operation WHERE state IN ('requested','running') AND (lease_until IS NULL OR lease_until <= CURRENT_TIMESTAMP(6)) AND (command_kind <> 'provision_tenant' OR JSON_UNQUOTE(JSON_EXTRACT(command_json, '$.deployment_id')) = ?) ORDER BY created_at,id LIMIT ? FOR UPDATE SKIP LOCKED"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("deploy-1", uint16(2)).WillReturnRows(sqlmock.NewRows(strings.Split(operationColumns+",lease_fence", ",")))
	mock.ExpectCommit()
	claims, err := store.ClaimDueForDeployment(context.Background(), "engine:worker", 2, 30*time.Second, "deploy-1")
	if err != nil || len(claims) != 0 {
		t.Fatalf("ClaimDueForDeployment() = %v, %v", claims, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryStateOperationRequiresUncertainStateCommandAndPersistsAudit(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{
		ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service",
		IdempotencyKey: "state-key-1", Kind: management.CommandSetTenantState,
		Command: []byte(`{"meta":{"idempotency_key":"state-key-1"},"state":"active","expected_config_revision":"3","reason":"restore"}`),
		Now:     time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	reason := "operator verified current tenant state and requested convergence retry"
	key := contract.IdempotencyKey("reconcile-state-1")
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	digest := management.CommandDigest("reconcile_tenant_state", payload)

	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.ID), string(input.Caller), string(key)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)")).
		WithArgs(sqlmock.AnyArg(), string(input.ID), string(input.Caller), string(key), digest, reason, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	updateQuery := "UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?"
	mock.ExpectExec(regexp.QuoteMeta(updateQuery)).WithArgs(string(input.ID), string(input.Caller), string(management.CommandSetTenantState), string(contract.OperationRecoveryRequired)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ?")).
		WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRequested, ""))
	mock.ExpectCommit()

	operation, err := store.RetryStateOperation(context.Background(), input.Caller, input.ID, key, reason)
	if err != nil || operation.ID != input.ID || operation.State != contract.OperationRequested || operation.Error != nil {
		t.Fatalf("RetryStateOperation() = %+v, %v", operation, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryStateOperationRejectsOtherOperationKinds(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := testAcceptCommand()
	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.ID), string(input.Caller), "reconcile-state-1").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	err := func() error {
		_, err := store.RetryStateOperation(context.Background(), input.Caller, input.ID, "reconcile-state-1", "operator reason")
		return err
	}()
	if !hasCode(err, contract.CodeConflict) {
		t.Fatalf("RetryStateOperation error = %v, want conflict for non-state operation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryProvisionOperationRequiresRecoveryRequiredBaseProvisionAndPersistsAudit(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{
		ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service",
		IdempotencyKey: "provision-key-1", Kind: management.CommandProvisionTenant,
		Command: []byte(`{"meta":{"idempotency_key":"provision-key-1"},"tenant_id":"tenant-1","bootstrap_admin_email":"admin@example.test","bootstrap_admin_subject":"usr_test_subject_1","primary_alias":"tenant.example.test","aliases":[],"deployment_id":"deployment-1","database_allocation_ref":"mysql:allocation-1"}`),
		Now:     time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	key := contract.IdempotencyKey("reconcile-provision-1")
	reason := "operator verified the canonical tenant and allocation before replay"
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	digest := management.CommandDigest("reconcile_tenant_provision", payload)

	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.ID), string(input.Caller), string(key)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)")).
		WithArgs(sqlmock.AnyArg(), string(input.ID), string(input.Caller), string(key), digest, reason, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	updateQuery := "UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?"
	mock.ExpectExec(regexp.QuoteMeta(updateQuery)).WithArgs(string(input.ID), string(input.Caller), string(management.CommandProvisionTenant), string(contract.OperationRecoveryRequired)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ?")).
		WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRequested, ""))
	mock.ExpectCommit()

	operation, err := store.RetryProvisionOperation(context.Background(), input.Caller, input.ID, key, reason)
	if err != nil || operation.ID != input.ID || operation.State != contract.OperationRequested || operation.Error != nil {
		t.Fatalf("RetryProvisionOperation() = %+v, %v", operation, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryProvisionOperationRejectsPackageSideEffects(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{
		ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service",
		IdempotencyKey: "provision-key-1", Kind: management.CommandProvisionTenant,
		Command: []byte(`{"meta":{"idempotency_key":"provision-key-1"},"tenant_id":"tenant-1","bootstrap_admin_email":"admin@example.test","primary_alias":"tenant.example.test","aliases":[],"deployment_id":"deployment-1","database_allocation_ref":"mysql:allocation-1","package_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		Now:     time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.ID), string(input.Caller), "reconcile-provision-1").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	err := func() error {
		_, err := store.RetryProvisionOperation(context.Background(), input.Caller, input.ID, "reconcile-provision-1", "operator reason")
		return err
	}()
	if !hasCode(err, contract.CodeConflict) {
		t.Fatalf("RetryProvisionOperation error = %v, want conflict for package side effects", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryPackageOperationRequiresUncertainPackageCommandAndPersistsAudit(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{
		ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service",
		IdempotencyKey: "package-key-1", Kind: management.CommandApplyPackage,
		Command: []byte(`{"meta":{"idempotency_key":"package-key-1"},"tenant_id":"tenant-1","artifact_ref":"object:release-1","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_config_revision":"4"}`),
		Now:     time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	key := contract.IdempotencyKey("reconcile-package-1")
	reason := "operator inspected the activation journal and confirmed retry"
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	digest := management.CommandDigest("reconcile_tenant_package", payload)

	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.ID), string(input.Caller), string(key)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)")).
		WithArgs(sqlmock.AnyArg(), string(input.ID), string(input.Caller), string(key), digest, reason, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	updateQuery := "UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?"
	mock.ExpectExec(regexp.QuoteMeta(updateQuery)).WithArgs(string(input.ID), string(input.Caller), string(management.CommandApplyPackage), string(contract.OperationRecoveryRequired)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ?")).
		WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRequested, ""))
	mock.ExpectCommit()

	operation, err := store.RetryPackageOperation(context.Background(), input.Caller, input.ID, key, reason)
	if err != nil || operation.ID != input.ID || operation.State != contract.OperationRequested || operation.Error != nil {
		t.Fatalf("RetryPackageOperation() = %+v, %v", operation, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryPackageOperationRejectsOtherKinds(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{
		ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service",
		IdempotencyKey: "state-key-1", Kind: management.CommandSetTenantState,
		Command: []byte(`{"meta":{"idempotency_key":"state-key-1"},"state":"active","expected_config_revision":"3","reason":"restore"}`),
		Now:     time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.ID), string(input.Caller), "reconcile-package-1").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	_, err := store.RetryPackageOperation(context.Background(), input.Caller, input.ID, "reconcile-package-1", "operator confirmed retry")
	if !hasCode(err, contract.CodeConflict) {
		t.Fatalf("RetryPackageOperation error = %v, want conflict for non-package operation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryPackageOperationReplaysAuditByKeyAndRejectsChangedReason(t *testing.T) {
	for _, changedReason := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "changed"}[changedReason], func(t *testing.T) {
			_, mock, store := newTestStore(t)
			input := management.AcceptCommand{
				ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service",
				IdempotencyKey: "package-key-1", Kind: management.CommandApplyPackage,
				Command: []byte(`{"meta":{"idempotency_key":"package-key-1"},"tenant_id":"tenant-1","artifact_ref":"object:release-1","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_config_revision":"4"}`),
				Now:     time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
			}
			key := contract.IdempotencyKey("reconcile-package-1")
			priorReason := "operator inspected the activation journal"
			reason := priorReason
			if changedReason {
				reason = "different reason"
			}
			payload, _ := json.Marshal(struct {
				Reason string `json:"reason"`
			}{Reason: priorReason})
			priorDigest := management.CommandDigest("reconcile_tenant_package", payload)
			mock.ExpectBegin()
			operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
			mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRequested, ""))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).
				WithArgs(string(input.ID), string(input.Caller), string(key)).WillReturnRows(sqlmock.NewRows([]string{"command_digest"}).AddRow(priorDigest))
			if changedReason {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}

			operation, err := store.RetryPackageOperation(context.Background(), input.Caller, input.ID, key, reason)
			if changedReason {
				if !hasCode(err, contract.CodeIdempotencyKeyReused) {
					t.Fatalf("changed reason error = %v, want idempotency conflict", err)
				}
			} else if err != nil || operation.State != contract.OperationRequested {
				t.Fatalf("same-key retry = %+v, %v", operation, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal("SQL expectations:", err)
			}
		})
	}
}

func TestRetryDeleteOperationRequiresRecoveryRequiredCheckpointedDeleteAndPersistsAudit(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service", IdempotencyKey: "delete-key-1", Kind: management.CommandDeleteTenant,
		Command: []byte(`{"meta":{"idempotency_key":"delete-key-1"},"recovery_checkpoint_ref":"checkpoint:backup-7"}`), Now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	key := contract.IdempotencyKey("reconcile-delete-1")
	reason := "operator verified checkpoint and allocation before retrying deletion"
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	digest := management.CommandDigest("reconcile_tenant_delete", payload)
	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "uncertain"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).WithArgs(string(input.ID), string(input.Caller), string(key)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)")).WithArgs(sqlmock.AnyArg(), string(input.ID), string(input.Caller), string(key), digest, reason, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	updateQuery := "UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?"
	mock.ExpectExec(regexp.QuoteMeta(updateQuery)).WithArgs(string(input.ID), string(input.Caller), string(management.CommandDeleteTenant), string(contract.OperationRecoveryRequired)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ?")).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRequested, ""))
	mock.ExpectCommit()
	operation, err := store.RetryDeleteOperation(context.Background(), input.Caller, input.ID, key, reason)
	if err != nil || operation.ID != input.ID || operation.State != contract.OperationRequested || operation.Error != nil {
		t.Fatalf("RetryDeleteOperation() = %+v, %v", operation, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestRetryDeleteOperationRejectsOtherKindsAndChangedOriginalCommand(t *testing.T) {
	for _, test := range []struct {
		name  string
		input management.AcceptCommand
	}{
		{name: "wrong kind", input: testAcceptCommand()},
		{name: "corrupt command", input: management.AcceptCommand{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service", IdempotencyKey: "delete-key-1", Kind: management.CommandDeleteTenant, Command: []byte(`{"meta":{"idempotency_key":"other-key"},"recovery_checkpoint_ref":"checkpoint:backup-7"}`), Now: time.Now()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, mock, store := newTestStore(t)
			mock.ExpectBegin()
			operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
			mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(test.input.ID), string(test.input.Caller)).WillReturnRows(recoveryOperationRows(test.input, contract.OperationRecoveryRequired, "uncertain"))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).WithArgs(string(test.input.ID), string(test.input.Caller), "reconcile-delete-1").WillReturnError(sql.ErrNoRows)
			mock.ExpectRollback()
			_, err := store.RetryDeleteOperation(context.Background(), test.input.Caller, test.input.ID, "reconcile-delete-1", "operator reason")
			if !hasCode(err, contract.CodeConflict) {
				t.Fatalf("RetryDeleteOperation error = %v, want conflict", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal("SQL expectations:", err)
			}
		})
	}
}

func TestRetryInvitationOperationPersistsAuditAndRequeuesSameOperation(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := management.AcceptCommand{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service", IdempotencyKey: "invite-key-1", Kind: management.CommandTenantInvitation, Command: []byte(`{"meta":{"idempotency_key":"invite-key-1"},"email":"owner@example.test"}`), Now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	key := contract.IdempotencyKey("reconcile-invite-1")
	reason := "provider logs confirm the message was not accepted"
	payload, _ := json.Marshal(struct {
		Outcome contract.InvitationDeliveryOutcome `json:"outcome"`
		Reason  string                             `json:"reason"`
	}{contract.InvitationNotDelivered, reason})
	digest := management.CommandDigest("reconcile_tenant_invitation", payload)

	mock.ExpectBegin()
	operationQuery := "SELECT " + operationColumns + " FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE"
	mock.ExpectQuery(regexp.QuoteMeta(operationQuery)).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRecoveryRequired, "outcome unknown"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).WithArgs(string(input.ID), string(input.Caller), string(key)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)")).WithArgs(sqlmock.AnyArg(), string(input.ID), string(input.Caller), string(key), digest, reason, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	updateQuery := "UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?"
	mock.ExpectExec(regexp.QuoteMeta(updateQuery)).WithArgs(string(input.ID), string(input.Caller), string(management.CommandTenantInvitation), string(contract.OperationRecoveryRequired)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ?")).WithArgs(string(input.ID), string(input.Caller)).WillReturnRows(recoveryOperationRows(input, contract.OperationRequested, ""))
	mock.ExpectCommit()

	operation, err := store.RetryInvitationOperation(context.Background(), input.Caller, input.ID, key, contract.InvitationNotDelivered, reason)
	if err != nil || operation.ID != input.ID || operation.State != contract.OperationRequested || operation.Error != nil {
		t.Fatalf("RetryInvitationOperation() = %+v, %v", operation, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func recoveryOperationRows(input management.AcceptCommand, state contract.OperationState, errorMessage string) *sqlmock.Rows {
	var errorType, message any
	if errorMessage != "" {
		errorType, message = string(contract.CodeDependencyUnavailable), errorMessage
	}
	return sqlmock.NewRows([]string{"id", "tenant_id", "caller_id", "idempotency_key", "command_kind", "command_digest", "command_json", "state", "observed_revision", "error_type", "error_message", "created_at", "updated_at"}).
		AddRow(string(input.ID), string(input.Tenant), string(input.Caller), string(input.IdempotencyKey), string(input.Kind), management.CommandDigest(input.Kind, input.Command), input.Command, string(state), "", errorType, message, input.Now, input.Now)
}

func TestClaimRenewAndFinishAreFenced(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := testAcceptCommand()
	rows := sqlmock.NewRows([]string{"id", "tenant_id", "caller_id", "idempotency_key", "command_kind", "command_digest", "command_json", "state", "observed_revision", "error_type", "error_message", "created_at", "updated_at", "lease_fence"}).
		AddRow(string(input.ID), string(input.Tenant), string(input.Caller), string(input.IdempotencyKey), string(input.Kind), management.CommandDigest(input.Kind, input.Command), input.Command, string(contract.OperationRequested), nil, nil, nil, input.Now, input.Now, uint64(4))
	claimQuery := "SELECT " + operationColumns + ",lease_fence FROM _kora_managed_operation WHERE state IN ('requested','running') AND (lease_until IS NULL OR lease_until <= CURRENT_TIMESTAMP(6)) ORDER BY created_at,id LIMIT ? FOR UPDATE SKIP LOCKED"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(claimQuery)).WithArgs(uint16(2)).WillReturnRows(rows)
	claimUpdate := "UPDATE _kora_managed_operation SET state = 'running',lease_owner = ?,lease_fence = lease_fence + 1,lease_until = DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND),updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND lease_fence = ? AND state IN ('requested','running') AND (lease_until IS NULL OR lease_until <= CURRENT_TIMESTAMP(6))"
	mock.ExpectExec(regexp.QuoteMeta(claimUpdate)).WithArgs("worker-a", time.Minute.Microseconds(), string(input.ID), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claims, err := store.ClaimDue(context.Background(), "worker-a", 2, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Fence != 5 || claims[0].Operation.State != contract.OperationRunning {
		t.Fatalf("ClaimDue = %+v, %v", claims, err)
	}
	claim := claims[0]
	renewQuery := "UPDATE _kora_managed_operation SET lease_until = DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND),updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > CURRENT_TIMESTAMP(6)"
	mock.ExpectExec(regexp.QuoteMeta(renewQuery)).WithArgs(time.Minute.Microseconds(), string(input.ID), "worker-a", uint64(5)).WillReturnResult(sqlmock.NewResult(0, 1))
	claim, err = store.Renew(context.Background(), claim, time.Minute)
	if err != nil {
		t.Fatal("Renew:", err)
	}
	finishQuery := "UPDATE _kora_managed_operation SET state = ?,observed_revision = ?,error_type = ?,error_message = ?,lease_owner = NULL,lease_until = NULL,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > CURRENT_TIMESTAMP(6)"
	mock.ExpectExec(regexp.QuoteMeta(finishQuery)).WithArgs(string(contract.OperationSucceeded), "8", nil, nil, string(input.ID), "worker-a", uint64(5)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Finish(context.Background(), claim, management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: "8"}); err != nil {
		t.Fatal("Finish:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestFinishRejectsStaleFenceAndInvalidOutcomes(t *testing.T) {
	_, mock, store := newTestStore(t)
	claim := management.Claim{Operation: management.Operation{ID: "01K7OPERATION000000000000000000"}, Owner: "worker-a", Fence: 7}
	if err := store.Finish(context.Background(), claim, management.FinishResult{State: contract.OperationFailed}); err == nil {
		t.Fatal("accepted terminal failure without details")
	}
	if err := store.Finish(context.Background(), claim, management.FinishResult{State: contract.OperationRunning}); err == nil {
		t.Fatal("accepted nonterminal completion")
	}
	query := "UPDATE _kora_managed_operation SET state = ?,observed_revision = ?,error_type = ?,error_message = ?,lease_owner = NULL,lease_until = NULL,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > CURRENT_TIMESTAMP(6)"
	mock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(string(contract.OperationSucceeded), nil, nil, nil, string(claim.Operation.ID), claim.Owner, claim.Fence).WillReturnResult(sqlmock.NewResult(0, 0))
	err := store.Finish(context.Background(), claim, management.FinishResult{State: contract.OperationSucceeded})
	var apiErr *contract.Error
	if !errors.As(err, &apiErr) || apiErr.Type != contract.CodeConflict {
		t.Fatalf("stale Finish = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestReleaseAndRenewRejectStaleClaims(t *testing.T) {
	_, mock, store := newTestStore(t)
	claim := management.Claim{Operation: management.Operation{ID: "01K7OPERATION000000000000000000"}, Owner: "worker-a", Fence: 7}
	renewQuery := "UPDATE _kora_managed_operation SET lease_until = DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND),updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > CURRENT_TIMESTAMP(6)"
	mock.ExpectExec(regexp.QuoteMeta(renewQuery)).WithArgs(time.Minute.Microseconds(), string(claim.Operation.ID), claim.Owner, claim.Fence).WillReturnResult(sqlmock.NewResult(0, 0))
	if _, err := store.Renew(context.Background(), claim, time.Minute); !hasCode(err, contract.CodeConflict) {
		t.Fatalf("stale Renew = %v, want conflict", err)
	}
	releaseQuery := "UPDATE _kora_managed_operation SET state = 'requested',lease_owner = NULL,lease_until = NULL,updated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > CURRENT_TIMESTAMP(6)"
	mock.ExpectExec(regexp.QuoteMeta(releaseQuery)).WithArgs(string(claim.Operation.ID), claim.Owner, claim.Fence).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.Release(context.Background(), claim); !hasCode(err, contract.CodeConflict) {
		t.Fatalf("stale Release = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestLeaseExpressionsAndArgumentsMatchSupportedDialects(t *testing.T) {
	for _, test := range []struct {
		driver, expression string
		argument           any
	}{
		{driver: "mysql", expression: "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND)", argument: time.Second.Microseconds()},
		{driver: "postgres", expression: "CURRENT_TIMESTAMP + (? * INTERVAL '1 microsecond')", argument: time.Second.Microseconds()},
		{driver: "libsql", expression: "datetime('now', '+' || ? || ' seconds')", argument: "1.000000"},
	} {
		dialect := db.Resolve(test.driver)
		if got := leaseExpression(dialect); got != test.expression {
			t.Errorf("%s lease expression = %q, want %q", test.driver, got, test.expression)
		}
		if got := leaseArgument(dialect, time.Second); got != test.argument {
			t.Errorf("%s lease argument = %#v, want %#v", test.driver, got, test.argument)
		}
	}
}

func hasCode(err error, code contract.Code) bool {
	var value *contract.Error
	return errors.As(err, &value) && value.Type == code
}

func newTestStore(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *Store) {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	return database, mock, store
}

func testAcceptCommand() management.AcceptCommand {
	return management.AcceptCommand{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service", IdempotencyKey: "provision-key-1", Kind: management.CommandProvisionTenant, Command: []byte(`{"tenant_id":"tenant-1"}`), Now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
}

func expectMissingByKey(mock sqlmock.Sqlmock, input management.AcceptCommand) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.Tenant), string(input.Caller), string(input.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
}

func expectStoredByKey(mock sqlmock.Sqlmock, input management.AcceptCommand, id contract.OperationID, command []byte, digest string) {
	columns := []string{"id", "tenant_id", "caller_id", "idempotency_key", "command_kind", "command_digest", "command_json", "state", "observed_revision", "error_type", "error_message", "created_at", "updated_at"}
	now := input.Now
	rows := sqlmock.NewRows(columns).AddRow(string(id), string(input.Tenant), string(input.Caller), string(input.IdempotencyKey), string(input.Kind), digest, command, string(contract.OperationRequested), nil, nil, nil, now, now)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ?")).
		WithArgs(string(input.Tenant), string(input.Caller), string(input.IdempotencyKey)).WillReturnRows(rows)
}

func TestAcceptRegistersMemberRevokeKind(t *testing.T) {
	_, mock, store := newTestStore(t)
	input := testAcceptCommand()
	input.Kind = management.CommandTenantMemberRevoke
	input.Command = []byte(`{"meta":{"idempotency_key":"` + string(input.IdempotencyKey) + `"},"subject":"cloud-user-1","reason":"removed from team"}`)
	expectMissingByKey(mock, input)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ? FOR UPDATE")).
		WithArgs(string(input.Tenant), string(input.Caller), string(input.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_managed_operation")).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	got, err := store.Accept(context.Background(), input)
	if err != nil || got.Kind != management.CommandTenantMemberRevoke {
		t.Fatalf("Accept member revoke = %+v, %v", got, err)
	}
}

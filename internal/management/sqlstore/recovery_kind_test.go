package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
)

// TestRetryKindsShareOneRequeuePath runs every operator retry through the
// shared path: a replayed key writes nothing, a lost requeue race, a wrong
// kind or state and a missing context each keep the kind's own error text.
func TestRetryKindsShareOneRequeuePath(t *testing.T) {
	const key = contract.IdempotencyKey("reconcile-kind-1")
	const reason = "operator verified the outcome and requests a retry"
	reasonOnly, _ := json.Marshal(reasonPayload{Reason: reason})
	invitation, _ := json.Marshal(struct {
		Outcome contract.InvitationDeliveryOutcome `json:"outcome"`
		Reason  string                             `json:"reason"`
	}{contract.InvitationNotDelivered, reason})
	type retryFunc func(context.Context, *Store, management.AcceptCommand) (management.Operation, error)
	for _, test := range []struct {
		kind    retryKind
		command string
		payload []byte
		call    retryFunc
		nilCtx  contract.Code
	}{
		{kind: stateRetry, payload: reasonOnly, command: `{"meta":{"idempotency_key":"k-1"},"state":"active","expected_config_revision":"3","reason":"restore"}`,
			call: func(ctx context.Context, s *Store, in management.AcceptCommand) (management.Operation, error) {
				return s.RetryStateOperation(ctx, in.Caller, in.ID, key, reason)
			}},
		{kind: provisionRetry, payload: reasonOnly, command: `{"meta":{"idempotency_key":"k-1"},"tenant_id":"tenant-1","bootstrap_admin_email":"admin@example.test","bootstrap_admin_subject":"usr_test_subject_1","primary_alias":"tenant.example.test","aliases":[],"deployment_id":"deployment-1","database_allocation_ref":"mysql:allocation-1"}`,
			call: func(ctx context.Context, s *Store, in management.AcceptCommand) (management.Operation, error) {
				return s.RetryProvisionOperation(ctx, in.Caller, in.ID, key, reason)
			}},
		{kind: packageRetry, payload: reasonOnly, command: `{"meta":{"idempotency_key":"k-1"},"tenant_id":"tenant-1","artifact_ref":"object:release-1","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_config_revision":"4"}`,
			call: func(ctx context.Context, s *Store, in management.AcceptCommand) (management.Operation, error) {
				return s.RetryPackageOperation(ctx, in.Caller, in.ID, key, reason)
			}},
		{kind: deleteRetry, payload: reasonOnly, command: `{"meta":{"idempotency_key":"k-1"},"recovery_checkpoint_ref":"checkpoint:backup-7"}`,
			call: func(ctx context.Context, s *Store, in management.AcceptCommand) (management.Operation, error) {
				return s.RetryDeleteOperation(ctx, in.Caller, in.ID, key, reason)
			}},
		{kind: invitationRetry, payload: invitation, nilCtx: contract.CodeValidationFailed, command: `{"meta":{"idempotency_key":"k-1"},"email":"owner@example.test"}`,
			call: func(ctx context.Context, s *Store, in management.AcceptCommand) (management.Operation, error) {
				return s.RetryInvitationOperation(ctx, in.Caller, in.ID, key, contract.InvitationNotDelivered, reason)
			}},
	} {
		input := management.AcceptCommand{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service", IdempotencyKey: "k-1", Kind: test.kind.command, Command: []byte(test.command), Now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
		digest := management.CommandDigest(test.kind.digestName, test.payload)
		expectLocked := func(mock sqlmock.Sqlmock, in management.AcceptCommand, state contract.OperationState, prior string) {
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT "+operationColumns+" FROM _kora_managed_operation WHERE id = ? AND caller_id = ? FOR UPDATE")).WithArgs(string(in.ID), string(in.Caller)).WillReturnRows(recoveryOperationRows(in, state, "uncertain"))
			lookup := mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?")).WithArgs(string(in.ID), string(in.Caller), string(key))
			if prior == "" {
				lookup.WillReturnError(sql.ErrNoRows)
			} else {
				lookup.WillReturnRows(sqlmock.NewRows([]string{"command_digest"}).AddRow(prior))
			}
		}
		wantConflict := func(t *testing.T, err error, message string) {
			t.Helper()
			var value *contract.Error
			if !errors.As(err, &value) || value.Type != contract.CodeConflict || value.Message != message {
				t.Fatalf("error = %v, want conflict %q", err, message)
			}
		}
		t.Run(string(test.kind.command), func(t *testing.T) {
			t.Run("replay writes nothing", func(t *testing.T) {
				_, mock, store := newTestStore(t)
				expectLocked(mock, input, contract.OperationRequested, digest)
				mock.ExpectCommit()
				if operation, err := test.call(context.Background(), store, input); err != nil || operation.ID != input.ID {
					t.Fatalf("replay = %+v, %v", operation, err)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("lost requeue race", func(t *testing.T) {
				_, mock, store := newTestStore(t)
				expectLocked(mock, input, contract.OperationRecoveryRequired, "")
				mock.ExpectExec("INSERT INTO _kora_managed_operation_recovery").WithArgs(sqlmock.AnyArg(), string(input.ID), string(input.Caller), string(key), digest, reason, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectExec("UPDATE _kora_managed_operation SET state = 'requested'").WithArgs(string(input.ID), string(input.Caller), string(test.kind.command), string(contract.OperationRecoveryRequired)).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectRollback()
				_, err := test.call(context.Background(), store, input)
				wantConflict(t, err, test.kind.text.changed)
			})
			for _, wrong := range []struct {
				name  string
				kind  management.CommandKind
				state contract.OperationState
			}{{"wrong kind", management.CommandTenantMemberGrant, contract.OperationRecoveryRequired}, {"failed state", test.kind.command, contract.OperationFailed}} {
				t.Run(wrong.name, func(t *testing.T) {
					_, mock, store := newTestStore(t)
					other := input
					other.Kind = wrong.kind
					expectLocked(mock, other, wrong.state, "")
					mock.ExpectRollback()
					_, err := test.call(context.Background(), store, other)
					wantConflict(t, err, test.kind.conflict)
				})
			}
			t.Run("missing context", func(t *testing.T) {
				_, _, store := newTestStore(t)
				_, err := test.call(nil, store, input) //nolint:staticcheck // nil context is the case under test
				if test.nilCtx != "" && !hasCode(err, test.nilCtx) || err == nil || err.Error() != test.kind.contextErr().Error() {
					t.Fatalf("nil context error = %v", err)
				}
			})
		})
	}
}

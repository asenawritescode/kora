//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
)

func TestLiveMySQLOperationTransitionGuards(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	exerciseLiveTransitionGuards(t, "mysql", dsn)
}

func TestLivePostgresOperationTransitionGuards(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	exerciseLiveTransitionGuards(t, "postgres", dsn)
}

// transitionCase calls one store method on an operation seeded in a given
// state and owned by a live claim (no lease for ClaimDue, so only the state
// guard decides). to is the state the move writes when it is allowed.
type transitionCase struct {
	move management.Move
	to   contract.OperationState
	call func(context.Context, *Store, management.Claim) (bool, error)
}

// exerciseLiveTransitionGuards proves every SQL state guard agrees with the
// declared transition table: each store method accepts exactly the from-states
// the table allows for its move and leaves every other state untouched.
func exerciseLiveTransitionGuards(t *testing.T, driver, dsn string) {
	t.Helper()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable management database:", err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dialect := db.Resolve(driver)
	store, err := New(database, dialect)
	if err != nil {
		t.Fatal("construct management store:", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure management schema:", err)
	}
	finish := func(state contract.OperationState) func(context.Context, *Store, management.Claim) (bool, error) {
		return func(ctx context.Context, s *Store, claim management.Claim) (bool, error) {
			result := management.FinishResult{State: state}
			if state != contract.OperationSucceeded {
				result.Error = contract.NewError(contract.CodeInternal, "transition guard test")
			}
			return true, s.Finish(ctx, claim, result)
		}
	}
	cases := []transitionCase{
		{move: management.MoveClaimDue, to: contract.OperationRunning, call: func(ctx context.Context, s *Store, claim management.Claim) (bool, error) {
			claims, err := s.ClaimDue(ctx, "transition-reclaimer", 100, time.Minute)
			mine := false
			for _, other := range claims {
				if other.Operation.ID == claim.Operation.ID {
					mine = true
				} else if releaseErr := s.Release(ctx, other); releaseErr != nil {
					return false, releaseErr
				}
			}
			return mine, err
		}},
		{move: management.MoveRenew, to: contract.OperationRunning, call: func(ctx context.Context, s *Store, claim management.Claim) (bool, error) {
			_, err := s.Renew(ctx, claim, time.Minute)
			return true, err
		}},
		{move: management.MoveRelease, to: contract.OperationRequested, call: func(ctx context.Context, s *Store, claim management.Claim) (bool, error) {
			return true, s.Release(ctx, claim)
		}},
		{move: management.MoveFinish, to: contract.OperationSucceeded, call: finish(contract.OperationSucceeded)},
		{move: management.MoveFinish, to: contract.OperationFailed, call: finish(contract.OperationFailed)},
		{move: management.MoveFinish, to: contract.OperationRecoveryRequired, call: finish(contract.OperationRecoveryRequired)},
		{move: management.MoveRetry, to: contract.OperationRequested, call: func(ctx context.Context, s *Store, claim management.Claim) (bool, error) {
			key := contract.IdempotencyKey("transition-retry-" + claim.Operation.ID[len(claim.Operation.ID)-8:])
			_, err := s.RetryStateOperation(ctx, claim.Operation.Caller, claim.Operation.ID, key, "operator verified the tenant state before a transition guard retry")
			return true, err
		}},
	}
	states := []contract.OperationState{contract.OperationRequested, contract.OperationRunning, contract.OperationSucceeded, contract.OperationFailed, contract.OperationRecoveryRequired}
	for _, test := range cases {
		for _, from := range states {
			t.Run(fmt.Sprintf("%s to %s from %s", test.move, test.to, from), func(t *testing.T) {
				claim := seedTransitionOperation(ctx, t, store, from, test.move != management.MoveClaimDue)
				applied, err := test.call(ctx, store, claim)
				want := test.move.Allows(from, test.to)
				if want && (err != nil || !applied) {
					t.Fatalf("allowed %s from %s refused: applied=%v err=%v", test.move, from, applied, err)
				}
				if !want && err == nil && applied {
					t.Fatalf("%s accepted disallowed from-state %s", test.move, from)
				}
				if !want && err != nil && !hasCode(err, contract.CodeConflict) {
					t.Fatalf("%s from %s refused with %v, want a conflict", test.move, from, err)
				}
				expect := from
				if want {
					expect = test.to
				}
				var state string
				if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT state FROM _kora_managed_operation WHERE id = ?`), string(claim.Operation.ID)).Scan(&state); err != nil || contract.OperationState(state) != expect {
					t.Fatalf("state after %s from %s = %q, %v; want %s", test.move, from, state, err, expect)
				}
			})
		}
	}
}

// seedTransitionOperation accepts a tenant-state operation, then forces it into
// from with a fenced owner. A live lease isolates the state guard for the
// claim-holding methods; ClaimDue gets no lease so the row is due.
func seedTransitionOperation(ctx context.Context, t *testing.T, store *Store, from contract.OperationState, leased bool) management.Claim {
	t.Helper()
	id := contract.OperationID(contract.NewID())
	key := contract.IdempotencyKey("transition-" + id[len(id)-8:])
	operation, err := store.Accept(ctx, management.AcceptCommand{
		ID: id, Tenant: "tenant-transition", Caller: "transition-service", IdempotencyKey: key, Kind: management.CommandSetTenantState,
		Command: []byte(fmt.Sprintf(`{"meta":{"idempotency_key":%q},"state":"suspended","expected_config_revision":"4","reason":"operator request"}`, string(key))),
		Now:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("accept transition operation:", err)
	}
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = store.database.ExecContext(cleanup, db.Rebind(store.dialect, `DELETE FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(id))
		_, _ = store.database.ExecContext(cleanup, db.Rebind(store.dialect, `DELETE FROM _kora_managed_operation WHERE id = ?`), string(id))
	})
	var errorType, errorMessage any
	if from == contract.OperationFailed || from == contract.OperationRecoveryRequired {
		errorType, errorMessage = string(contract.CodeInternal), "seeded outcome"
	}
	lease := "NULL"
	args := []any{string(from), errorType, errorMessage}
	if leased {
		lease = leaseExpression(store.dialect)
		args = append(args, leaseArgument(store.dialect, time.Minute))
	}
	args = append(args, string(id))
	query := `UPDATE _kora_managed_operation SET state = ?,error_type = ?,error_message = ?,lease_owner = 'transition-worker',lease_fence = 7,lease_until = ` + lease + ` WHERE id = ?`
	if _, err := store.database.ExecContext(ctx, db.Rebind(store.dialect, query), args...); err != nil {
		t.Fatal("seed transition from-state:", err)
	}
	operation.State = from
	return management.Claim{Operation: operation, Owner: "transition-worker", Fence: 7}
}

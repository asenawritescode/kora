//go:build integration

package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/management/sqlstore"
	"github.com/asenawritescode/kora/internal/sweep"
)

type noAdmission struct{}

func (noAdmission) Acquire(context.Context) (func(), error) { return func() {}, nil }

type wakeExecutor func(context.Context, management.Claim) (management.FinishResult, error)

func (f wakeExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	return f(ctx, claim)
}

// newWakeStore opens a disposable database with the management schema.
func newWakeStore(t *testing.T, driver, dsn string) *sqlstore.Store {
	t.Helper()
	database := disposabledb.Open(t, driver, dsn)
	t.Cleanup(func() { _ = database.DB.Close() })
	store, err := sqlstore.New(database.DB, db.Resolve(driver))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

// startWakeWorker runs one management worker over store and reports each
// finished operation on the returned channel.
func startWakeWorker(t *testing.T, store *sqlstore.Store, wake <-chan struct{}, sweepEvery time.Duration) <-chan contract.OperationID {
	t.Helper()
	done := make(chan contract.OperationID, 8)
	worker, err := management.NewWorker(store, wakeExecutor(func(_ context.Context, claim management.Claim) (management.FinishResult, error) {
		done <- claim.Operation.ID
		return management.FinishResult{State: contract.OperationSucceeded}, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	config := management.WorkerConfig{Owner: "wake-test", Concurrency: 1, Lease: 3 * time.Second, Sweep: sweepEvery, Wake: wake}
	go func() {
		if err := worker.Run(ctx, config); err != nil {
			t.Errorf("management worker stopped: %v", err)
		}
	}()
	return done
}

func acceptWakeOperation(t *testing.T, accept func(context.Context, management.AcceptCommand) (management.Operation, error)) contract.OperationID {
	t.Helper()
	id := contract.OperationID(contract.NewID())
	key := contract.IdempotencyKey("wake-" + string(id[len(id)-8:]))
	command := fmt.Sprintf(`{"meta":{"idempotency_key":%q},"state":"suspended","expected_config_revision":"4","reason":"wake test"}`, string(key))
	if _, err := accept(t.Context(), management.AcceptCommand{
		ID: id, Tenant: "tenant-wake", Caller: "wake-service", IdempotencyKey: key, Kind: management.CommandSetTenantState,
		Command: []byte(command), Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func awaitWakeOperation(t *testing.T, done <-chan contract.OperationID, want contract.OperationID, within time.Duration) {
	t.Helper()
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("finished %s, want %s", got, want)
		}
	case <-time.After(within):
		t.Fatalf("operation not finished within %v", within)
	}
}

// TestManagementWakeIntegration proves on each dialect that an accepted
// operation runs within a second of its commit, that a claim whose worker died
// is reclaimed at lease expiry, and that a dropped wake is recovered by the sweep.
func TestManagementWakeIntegration(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.env, tc.driver)
			}
			t.Run("commit wakes worker", func(t *testing.T) {
				store := newWakeStore(t, tc.driver, dsn)
				wake := sweep.NewWake()
				done := startWakeWorker(t, store, wake.Subscribe(), 5*time.Minute)
				// The admitted store is the path the Engine's API takes; it signals after commit.
				admitted := admittedManagementStore{store: store, admission: noAdmission{}, wake: wake}
				id := acceptWakeOperation(t, admitted.Accept)
				awaitWakeOperation(t, done, id, time.Second)
			})
			t.Run("dropped wake recovered by sweep", func(t *testing.T) {
				store := newWakeStore(t, tc.driver, dsn)
				done := startWakeWorker(t, store, nil, 200*time.Millisecond)
				id := acceptWakeOperation(t, store.Accept)
				awaitWakeOperation(t, done, id, 3*time.Second)
			})
			t.Run("crashed claim reclaimed at lease expiry", func(t *testing.T) {
				store := newWakeStore(t, tc.driver, dsn)
				id := acceptWakeOperation(t, store.Accept)
				// A claim taken by a process that then dies: no Finish, no Release.
				claims, err := store.ClaimDue(t.Context(), "crashed-engine", 1, 3*time.Second)
				if err != nil || len(claims) != 1 {
					t.Fatalf("seed crashed claim: %d claims, err %v", len(claims), err)
				}
				claimedAt := time.Now()
				done := startWakeWorker(t, store, nil, 5*time.Minute)
				awaitWakeOperation(t, done, id, 10*time.Second)
				if elapsed := time.Since(claimedAt); elapsed < 2*time.Second {
					t.Fatalf("reclaimed after %v, before the 3s lease expired", elapsed)
				}
			})
		})
	}
}

//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/management"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLivePostgresTenantStateOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	exerciseLiveStateRecovery(t, "postgres", dsn)
}

func TestLiveLibSQLTenantStateOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL HTTP database")
	}
	exerciseLiveStateRecovery(t, "libsql", dsn)
}

func TestLiveMySQLTenantProvisionOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	exerciseLiveProvisionRecovery(t, "mysql", dsn)
}

func TestLivePostgresTenantProvisionOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	exerciseLiveProvisionRecovery(t, "postgres", dsn)
}

func TestLiveLibSQLTenantProvisionOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL HTTP database")
	}
	exerciseLiveProvisionRecovery(t, "libsql", dsn)
}

func TestLiveMySQLTenantPackageOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	exerciseLivePackageRecovery(t, "mysql", dsn)
}

func TestLivePostgresTenantPackageOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	exerciseLivePackageRecovery(t, "postgres", dsn)
}

func TestLiveLibSQLTenantPackageOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL HTTP database")
	}
	exerciseLivePackageRecovery(t, "libsql", dsn)
}

func TestLiveMySQLTenantDeleteOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	exerciseLiveDeleteRecovery(t, "mysql", dsn)
}

func TestLivePostgresTenantDeleteOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	exerciseLiveDeleteRecovery(t, "postgres", dsn)
}

func TestLiveLibSQLTenantDeleteOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL HTTP database")
	}
	exerciseLiveDeleteRecovery(t, "libsql", dsn)
}

func TestLiveMySQLTenantInvitationOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	exerciseLiveInvitationRecovery(t, "mysql", dsn)
}

func TestLivePostgresTenantInvitationOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	exerciseLiveInvitationRecovery(t, "postgres", dsn)
}

func TestLiveLibSQLTenantInvitationOperationRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL HTTP database")
	}
	exerciseLiveInvitationRecovery(t, "libsql", dsn)
}

func exerciseLiveInvitationRecovery(t *testing.T, driver, dsn string) {
	t.Helper()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable management database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable management database:", err)
	}
	store, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("construct management store:", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure management schema:", err)
	}
	operationID := contract.OperationID(contract.NewID())
	defer func() {
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(operationID))
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation WHERE id = ?`), string(operationID))
	}()
	tenant := contract.TenantID("tenant-invitation-recovery-" + operationID[len(operationID)-8:])
	caller := contract.PrincipalID("operator-invitation-recovery")
	key := contract.IdempotencyKey("invitation-key-" + operationID[len(operationID)-8:])
	command := contract.RequestTenantInvitation{Meta: contract.CommandMeta{IdempotencyKey: key}, Email: "owner@example.test"}
	commandJSON, err := json.Marshal(command)
	if err != nil {
		t.Fatal("encode invitation command:", err)
	}
	accepted, err := store.Accept(ctx, management.AcceptCommand{ID: operationID, Tenant: tenant, Caller: caller, IdempotencyKey: key, Kind: management.CommandTenantInvitation, Command: commandJSON, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal("accept invitation operation:", err)
	}
	claims, err := store.ClaimDue(ctx, "invitation-recovery-worker", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Operation.ID != accepted.ID {
		t.Fatalf("claim invitation operation=%+v, %v", claims, err)
	}
	if err := store.Finish(ctx, claims[0], management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeOutcomeUnknown, "provider outcome unknown")}); err != nil {
		t.Fatal("finish uncertain invitation operation:", err)
	}
	recoveryKey := contract.IdempotencyKey("invitation-retry-" + operationID[len(operationID)-8:])
	reason := "provider logs confirm the message was not accepted"
	retried, err := store.RetryInvitationOperation(ctx, caller, operationID, recoveryKey, contract.InvitationNotDelivered, reason)
	if err != nil || retried.ID != accepted.ID || retried.State != contract.OperationRequested || retried.Error != nil {
		t.Fatalf("reconcile invitation operation=%+v, %v", retried, err)
	}
	restarted, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("reopen management store after invitation recovery:", err)
	}
	replayed, err := restarted.RetryInvitationOperation(ctx, caller, operationID, recoveryKey, contract.InvitationNotDelivered, reason)
	if err != nil || replayed.ID != retried.ID || replayed.State != retried.State {
		t.Fatalf("replay durable invitation recovery=%+v, %v", replayed, err)
	}
	if _, err := restarted.RetryInvitationOperation(ctx, caller, operationID, recoveryKey, contract.InvitationDelivered, reason); !hasCode(err, contract.CodeIdempotencyKeyReused) {
		t.Fatalf("changed invitation recovery outcome error=%v", err)
	}
	var audits int
	if err := database.QueryRowContext(ctx, db.Rebind(db.Resolve(driver), `SELECT COUNT(*) FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(operationID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("invitation recovery audit count=%d, %v", audits, err)
	}
	secondClaims, err := restarted.ClaimDue(ctx, "invitation-recovery-worker-after-restart", 1, time.Minute)
	if err != nil || len(secondClaims) != 1 || secondClaims[0].Operation.ID != accepted.ID || secondClaims[0].Fence <= claims[0].Fence+1 {
		t.Fatalf("claim reconciled invitation after restart=%+v, %v", secondClaims, err)
	}
}

func exerciseLiveStateRecovery(t *testing.T, driver, dsn string) {
	t.Helper()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable management database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable management database:", err)
	}
	store, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("construct management store:", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure management schema:", err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation_recovery WHERE operation_id IN (SELECT id FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ?)`), "tenant-recovery", "operator-service"); err != nil {
		t.Fatal("clear prior disposable recovery audit fixture:", err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ?`), "tenant-recovery", "operator-service"); err != nil {
		t.Fatal("clear prior disposable operation fixture:", err)
	}
	operationID := contract.OperationID(contract.NewID())
	key := contract.IdempotencyKey("state-recovery-" + operationID[len(operationID)-8:])
	command := []byte(fmt.Sprintf(`{"meta":{"idempotency_key":%q},"state":"suspended","expected_config_revision":"4","reason":"operator request"}`, string(key)))
	accepted, err := store.Accept(ctx, management.AcceptCommand{
		ID: operationID, Tenant: "tenant-recovery", Caller: "operator-service", IdempotencyKey: key,
		Kind: management.CommandSetTenantState, Command: command, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("accept state operation:", err)
	}
	claims, err := store.ClaimDue(ctx, "recovery-worker", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Operation.ID != accepted.ID {
		t.Fatalf("claim state operation = %+v, %v", claims, err)
	}
	if err := store.Finish(ctx, claims[0], management.FinishResult{
		State: contract.OperationRecoveryRequired,
		Error: contract.NewError(contract.CodeDependencyUnavailable, "runtime convergence uncertain"),
	}); err != nil {
		t.Fatal("finish uncertain operation:", err)
	}
	recoveryKey := contract.IdempotencyKey("recovery-submit-" + operationID[len(operationID)-8:])
	reason := "operator checked the directory and requested an idempotent runtime convergence retry"
	retried, err := store.RetryStateOperation(ctx, accepted.Caller, accepted.ID, recoveryKey, reason)
	if err != nil || retried.ID != accepted.ID || retried.State != contract.OperationRequested || retried.Error != nil {
		t.Fatalf("reconcile state operation = %+v, %v", retried, err)
	}
	restarted, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("reopen management store after recovery:", err)
	}
	replayed, err := restarted.RetryStateOperation(ctx, accepted.Caller, accepted.ID, recoveryKey, reason)
	if err != nil || replayed.ID != retried.ID || replayed.State != retried.State {
		t.Fatalf("replay durable recovery = %+v, %v", replayed, err)
	}
	var audits int
	if err := database.QueryRowContext(ctx, db.Rebind(db.Resolve(driver), `SELECT COUNT(*) FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(accepted.ID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("durable recovery audit count = %d, %v", audits, err)
	}
	secondClaims, err := restarted.ClaimDue(ctx, "recovery-worker-after-restart", 1, time.Minute)
	if err != nil || len(secondClaims) != 1 || secondClaims[0].Operation.ID != accepted.ID || secondClaims[0].Fence <= claims[0].Fence+1 {
		t.Fatalf("claim reconciled operation after restart = %+v, %v", secondClaims, err)
	}
}

func exerciseLiveProvisionRecovery(t *testing.T, driver, dsn string) {
	t.Helper()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable management database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable management database:", err)
	}
	store, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("construct management store:", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure management schema:", err)
	}
	stamp := contract.NewID()
	tenant := contract.TenantID("tenant-provision-recovery-" + stamp[len(stamp)-8:])
	caller := contract.PrincipalID("operator-provision-recovery")
	if _, err := database.ExecContext(ctx, db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation_recovery WHERE operation_id IN (SELECT id FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ?)`), string(tenant), string(caller)); err != nil {
		t.Fatal("clear prior disposable recovery audit fixture:", err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ?`), string(tenant), string(caller)); err != nil {
		t.Fatal("clear prior disposable operation fixture:", err)
	}
	operationID := contract.OperationID(contract.NewID())
	defer func() {
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(operationID))
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation WHERE id = ?`), string(operationID))
	}()
	key := contract.IdempotencyKey("provision-recovery-" + operationID[len(operationID)-8:])
	command := contract.ProvisionTenant{
		Meta: contract.CommandMeta{IdempotencyKey: key}, Tenant: tenant,
		BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test",
		Deployment: "deployment-1", DatabaseAllocationRef: driver + ":allocation-1",
	}
	commandJSON, err := json.Marshal(command)
	if err != nil {
		t.Fatal("encode base provision command:", err)
	}
	accepted, err := store.Accept(ctx, management.AcceptCommand{
		ID: operationID, Tenant: tenant, Caller: caller, IdempotencyKey: key,
		Kind: management.CommandProvisionTenant, Command: commandJSON, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("accept provision operation:", err)
	}
	claims, err := store.ClaimDue(ctx, "provision-recovery-worker", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Operation.ID != accepted.ID {
		t.Fatalf("claim provision operation = %+v, %v", claims, err)
	}
	if err := store.Finish(ctx, claims[0], management.FinishResult{
		State: contract.OperationRecoveryRequired,
		Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant creation outcome requires inspection"),
	}); err != nil {
		t.Fatal("finish uncertain provision operation:", err)
	}
	recoveryKey := contract.IdempotencyKey("provision-retry-" + operationID[len(operationID)-8:])
	reason := "operator verified tenant identity and allocation before replay"
	retried, err := store.RetryProvisionOperation(ctx, caller, operationID, recoveryKey, reason)
	if err != nil || retried.ID != accepted.ID || retried.State != contract.OperationRequested || retried.Error != nil {
		t.Fatalf("reconcile provision operation = %+v, %v", retried, err)
	}
	restarted, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("reopen management store after provision recovery:", err)
	}
	replayed, err := restarted.RetryProvisionOperation(ctx, caller, operationID, recoveryKey, reason)
	if err != nil || replayed.ID != retried.ID || replayed.State != retried.State {
		t.Fatalf("replay durable provision recovery = %+v, %v", replayed, err)
	}
	var audits int
	if err := database.QueryRowContext(ctx, db.Rebind(db.Resolve(driver), `SELECT COUNT(*) FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(accepted.ID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("durable provision recovery audit count = %d, %v", audits, err)
	}
	secondClaims, err := restarted.ClaimDue(ctx, "provision-recovery-worker-after-restart", 1, time.Minute)
	if err != nil || len(secondClaims) != 1 || secondClaims[0].Operation.ID != accepted.ID || secondClaims[0].Fence <= claims[0].Fence+1 {
		t.Fatalf("claim reconciled provision operation after restart = %+v, %v", secondClaims, err)
	}
}

func exerciseLivePackageRecovery(t *testing.T, driver, dsn string) {
	t.Helper()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable management database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable management database:", err)
	}
	store, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("construct management store:", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure management schema:", err)
	}
	operationID := contract.OperationID(contract.NewID())
	defer func() {
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(operationID))
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation WHERE id = ?`), string(operationID))
	}()
	tenant := contract.TenantID("tenant-package-recovery-" + operationID[len(operationID)-8:])
	caller := contract.PrincipalID("operator-package-recovery")
	key := contract.IdempotencyKey("package-key-" + operationID[len(operationID)-8:])
	command := contract.ApplyPackage{
		Meta: contract.CommandMeta{IdempotencyKey: key}, Tenant: tenant,
		ArtifactRef: "object:package-recovery", Digest: strings.Repeat("a", 64), ExpectedConfigRevision: "4",
	}
	commandJSON, err := json.Marshal(command)
	if err != nil {
		t.Fatal("encode package command:", err)
	}
	accepted, err := store.Accept(ctx, management.AcceptCommand{
		ID: operationID, Tenant: tenant, Caller: caller, IdempotencyKey: key,
		Kind: management.CommandApplyPackage, Command: commandJSON, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("accept package operation:", err)
	}
	claims, err := store.ClaimDue(ctx, "package-recovery-worker", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Operation.ID != accepted.ID {
		t.Fatalf("claim package operation = %+v, %v", claims, err)
	}
	if err := store.Finish(ctx, claims[0], management.FinishResult{
		State: contract.OperationRecoveryRequired,
		Error: contract.NewError(contract.CodeDependencyUnavailable, "package activation outcome requires inspection"),
	}); err != nil {
		t.Fatal("finish uncertain package operation:", err)
	}
	recoveryKey := contract.IdempotencyKey("package-retry-" + operationID[len(operationID)-8:])
	reason := "operator inspected the durable package activation journal"
	retried, err := store.RetryPackageOperation(ctx, caller, operationID, recoveryKey, reason)
	if err != nil || retried.ID != accepted.ID || retried.State != contract.OperationRequested || retried.Error != nil {
		t.Fatalf("reconcile package operation = %+v, %v", retried, err)
	}
	restarted, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("reopen management store after package recovery:", err)
	}
	replayed, err := restarted.RetryPackageOperation(ctx, caller, operationID, recoveryKey, reason)
	if err != nil || replayed.ID != retried.ID || replayed.State != retried.State {
		t.Fatalf("replay durable package recovery = %+v, %v", replayed, err)
	}
	var audits int
	if err := database.QueryRowContext(ctx, db.Rebind(db.Resolve(driver), `SELECT COUNT(*) FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(accepted.ID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("durable package recovery audit count = %d, %v", audits, err)
	}
	secondClaims, err := restarted.ClaimDue(ctx, "package-recovery-worker-after-restart", 1, time.Minute)
	if err != nil || len(secondClaims) != 1 || secondClaims[0].Operation.ID != accepted.ID || secondClaims[0].Fence <= claims[0].Fence+1 {
		t.Fatalf("claim reconciled package operation after restart = %+v, %v", secondClaims, err)
	}
}

func exerciseLiveDeleteRecovery(t *testing.T, driver, dsn string) {
	t.Helper()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable management database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable management database:", err)
	}
	store, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("construct management store:", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure management schema:", err)
	}
	operationID := contract.OperationID(contract.NewID())
	defer func() {
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(operationID))
		_, _ = database.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_managed_operation WHERE id = ?`), string(operationID))
	}()
	tenant := contract.TenantID("tenant-delete-recovery-" + operationID[len(operationID)-8:])
	caller := contract.PrincipalID("operator-delete-recovery")
	key := contract.IdempotencyKey("delete-recovery-" + operationID[len(operationID)-8:])
	commandJSON, err := json.Marshal(contract.DeleteTenant{Meta: contract.CommandMeta{IdempotencyKey: key}, RecoveryCheckpointRef: "checkpoint:backup-7"})
	if err != nil {
		t.Fatal("encode delete command:", err)
	}
	accepted, err := store.Accept(ctx, management.AcceptCommand{ID: operationID, Tenant: tenant, Caller: caller, IdempotencyKey: key, Kind: management.CommandDeleteTenant, Command: commandJSON, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal("accept delete operation:", err)
	}
	claims, err := store.ClaimDue(ctx, "delete-recovery-worker", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Operation.ID != accepted.ID {
		t.Fatalf("claim delete operation = %+v, %v", claims, err)
	}
	if err := store.Finish(ctx, claims[0], management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant deletion outcome requires inspection")}); err != nil {
		t.Fatal("finish uncertain delete operation:", err)
	}
	recoveryKey := contract.IdempotencyKey("delete-retry-" + operationID[len(operationID)-8:])
	reason := "operator verified the original checkpoint and requested a fenced retry"
	retried, err := store.RetryDeleteOperation(ctx, caller, operationID, recoveryKey, reason)
	if err != nil || retried.ID != accepted.ID || retried.State != contract.OperationRequested || retried.Error != nil {
		t.Fatalf("reconcile delete operation = %+v, %v", retried, err)
	}
	restarted, err := New(database, db.Resolve(driver))
	if err != nil {
		t.Fatal("reopen management store after delete recovery:", err)
	}
	replayed, err := restarted.RetryDeleteOperation(ctx, caller, operationID, recoveryKey, reason)
	if err != nil || replayed.ID != retried.ID || replayed.State != retried.State {
		t.Fatalf("replay durable delete recovery = %+v, %v", replayed, err)
	}
	var audits int
	if err := database.QueryRowContext(ctx, db.Rebind(db.Resolve(driver), `SELECT COUNT(*) FROM _kora_managed_operation_recovery WHERE operation_id = ?`), string(accepted.ID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("durable delete recovery audit count = %d, %v", audits, err)
	}
	secondClaims, err := restarted.ClaimDue(ctx, "delete-recovery-after-restart", 1, time.Minute)
	if err != nil || len(secondClaims) != 1 || secondClaims[0].Operation.ID != accepted.ID || secondClaims[0].Fence <= claims[0].Fence+1 {
		t.Fatalf("claim reconciled delete operation after restart = %+v, %v", secondClaims, err)
	}
}

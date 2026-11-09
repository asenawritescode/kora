//go:build integration

package script

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// TestAsyncHookStoreTransactionsAndRecoveryAcrossDialects must run only against
// fresh disposable databases. It proves the source append rollback, local
// effect/receipt atomicity, stale-fence rejection, retry, and external-outcome
// uncertainty using each supported SQL transport.
func TestAsyncHookStoreTransactionsAndRecoveryAcrossDialects(t *testing.T) {
	cases := []struct {
		name, driver, env string
	}{
		{name: "mysql", driver: "mysql", env: "KORA_ASYNC_HOOK_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", env: "KORA_ASYNC_HOOK_DISPOSABLE_POSTGRES_DSN"},
		{name: "libsql-http", driver: "libsql", env: "KORA_ASYNC_HOOK_DISPOSABLE_LIBSQL_DSN"},
	}
	configured := false
	for _, tc := range cases {
		configured = configured || os.Getenv(tc.env) != ""
	}
	if !configured {
		t.Skip("set KORA_ASYNC_HOOK_DISPOSABLE_{MYSQL,POSTGRES,LIBSQL}_DSN to fresh disposable databases")
	}
	for _, tc := range cases {
		dsn := os.Getenv(tc.env)
		if dsn == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open disposable database:", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			if err := database.PingContext(ctx); err != nil {
				t.Fatal("ping disposable database:", err)
			}
			dialect := db.Resolve(tc.driver)
			store := &Store{DB: database, Dialect: dialect}
			if err := store.EnsureAsyncHookInvocationSchema(ctx); err != nil {
				t.Fatal("ensure lifecycle invocation schema:", err)
			}

			tenant := contract.TenantID("hook-test-" + contract.NewID())
			source := contract.OperationID(contract.NewOperationID())
			invocation := integrationHookInvocation(tenant, source)
			effectTable := dialect.QuoteIdent("hook_effect_" + contract.NewID())
			if _, err := database.ExecContext(ctx, `CREATE TABLE `+effectTable+` (id VARCHAR(64) PRIMARY KEY)`); err != nil {
				t.Fatal("create disposable local-effect table:", err)
			}
			t.Cleanup(func() { _, _ = database.ExecContext(context.Background(), `DROP TABLE `+effectTable) })

			rollback, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin source rollback transaction:", err)
			}
			if err := store.AppendAsyncHookTx(ctx, rollback, invocation); err != nil {
				_ = rollback.Rollback()
				t.Fatal("append invocation in source transaction:", err)
			}
			if err := rollback.Rollback(); err != nil {
				t.Fatal("rollback source transaction:", err)
			}
			if got := integrationInvocationCount(t, ctx, database, dialect, tenant); got != 0 {
				t.Fatalf("rolled-back source invocation count = %d, want 0", got)
			}

			appendTx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin committed source transaction:", err)
			}
			if err := store.AppendAsyncHookTx(ctx, appendTx, invocation); err != nil {
				_ = appendTx.Rollback()
				t.Fatal("append committed invocation:", err)
			}
			if err := appendTx.Commit(); err != nil {
				t.Fatal("commit source invocation:", err)
			}
			first := integrationClaim(t, ctx, store, tenant, "worker-one")

			// A domain effect and the success receipt are one transaction. A
			// simulated crash before commit must leave neither committed.
			crashedTx, err := store.BeginAsyncHook(ctx, first)
			if err != nil {
				t.Fatal("begin first fenced execution:", err)
			}
			if _, err := crashedTx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO `+effectTable+` (id) VALUES (?)`), "local-effect"); err != nil {
				_ = crashedTx.Rollback()
				t.Fatal("write local effect:", err)
			}
			if err := store.SucceedAsyncHookTx(ctx, crashedTx, first); err != nil {
				_ = crashedTx.Rollback()
				t.Fatal("write success receipt in local-effect transaction:", err)
			}
			if err := crashedTx.Rollback(); err != nil {
				t.Fatal("simulate crash rollback:", err)
			}
			if got := integrationEffectCount(t, ctx, database, effectTable); got != 0 {
				t.Fatalf("local effects after rollback = %d, want 0", got)
			}
			integrationExpireClaim(t, ctx, database, dialect, tenant, source, invocation.Sequence)
			second := integrationClaim(t, ctx, store, tenant, "worker-two")
			if second.Fence <= first.Fence {
				t.Fatalf("replacement fence = %d, previous = %d", second.Fence, first.Fence)
			}
			if _, err := store.BeginAsyncHook(ctx, first); err == nil {
				t.Fatal("stale worker claim began after replacement fence")
			}
			finishTx, err := store.BeginAsyncHook(ctx, second)
			if err != nil {
				t.Fatal("begin replacement execution:", err)
			}
			if _, err := finishTx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO `+effectTable+` (id) VALUES (?)`), "local-effect"); err != nil {
				_ = finishTx.Rollback()
				t.Fatal("write replacement local effect:", err)
			}
			if err := store.SucceedAsyncHookTx(ctx, finishTx, second); err != nil {
				_ = finishTx.Rollback()
				t.Fatal("write replacement success receipt:", err)
			}
			if err := finishTx.Commit(); err != nil {
				t.Fatal("commit replacement local effect and receipt:", err)
			}
			if got := integrationEffectCount(t, ctx, database, effectTable); got != 1 {
				t.Fatalf("committed local effect count = %d, want 1", got)
			}
			if got := integrationInvocationStatus(t, ctx, database, dialect, tenant, source, invocation.Sequence); got != "succeeded" {
				t.Fatalf("invocation status = %q, want succeeded", got)
			}
			staleTx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin stale receipt check:", err)
			}
			if err := store.SucceedAsyncHookTx(ctx, staleTx, first); err == nil {
				_ = staleTx.Rollback()
				t.Fatal("stale worker receipt was accepted")
			}
			_ = staleTx.Rollback()

			// A retryable provider-free failure can be reclaimed; once an
			// external effect marker is committed, lease expiry becomes uncertain.
			retryInvocation := integrationHookInvocation(tenant, contract.OperationID(contract.NewOperationID()))
			retryTx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin retry source transaction:", err)
			}
			if err := store.AppendAsyncHookTx(ctx, retryTx, retryInvocation); err != nil {
				_ = retryTx.Rollback()
				t.Fatal("append retry fixture:", err)
			}
			if err := retryTx.Commit(); err != nil {
				t.Fatal("commit retry fixture:", err)
			}
			retryClaim := integrationClaim(t, ctx, store, tenant, "retry-worker-one")
			if err := store.FinishAsyncHook(ctx, retryClaim, AsyncHookOutcome{Error: "transient", Retry: true}); err != nil {
				t.Fatal("schedule retry:", err)
			}
			integrationMakeDue(t, ctx, database, dialect, tenant, retryInvocation.SourceOperation, retryInvocation.Sequence)
			externalClaim := integrationClaim(t, ctx, store, tenant, "retry-worker-two")
			if err := store.MarkAsyncHookExternalEffect(ctx, externalClaim); err != nil {
				t.Fatal("persist external-effect start marker:", err)
			}
			integrationExpireClaim(t, ctx, database, dialect, tenant, retryInvocation.SourceOperation, retryInvocation.Sequence)
			claims, err := store.ClaimAsyncHooks(ctx, tenant, "retry-worker-three", 1, time.Minute)
			if err != nil {
				t.Fatal("claim after uncertain external effect:", err)
			}
			if len(claims) != 0 {
				t.Fatal("external effect with expired lease was retried automatically")
			}
			if got := integrationInvocationStatus(t, ctx, database, dialect, tenant, retryInvocation.SourceOperation, retryInvocation.Sequence); got != "uncertain" {
				t.Fatalf("expired external invocation status = %q, want uncertain", got)
			}
		})
	}
}

func integrationHookInvocation(tenant contract.TenantID, source contract.OperationID) AsyncHookInvocation {
	site := string(tenant)
	return AsyncHookInvocation{
		TenantID: tenant, Site: site, SourceOperation: source, ConfigRevision: 1, Sequence: 1,
		Doctype: "Task", Event: EventAfterSave,
		Record: ScriptRecord{Name: "task-after-save", Site: site, ScriptType: TypeDocEvent,
			DocType: "Task", Event: EventAfterSave, IsActive: true, TimeoutMs: 1000, Script: "return true"},
		Document: []byte(`{"name":"TASK-1"}`),
		Actor: contract.ActorContext{PrincipalID: "worker-user", PrincipalType: contract.PrincipalHuman,
			Site: site, AuthenticatedAt: time.Now().UTC()},
	}
}

func integrationClaim(t *testing.T, ctx context.Context, store *Store, tenant contract.TenantID, owner string) AsyncHookClaim {
	t.Helper()
	claims, err := store.ClaimAsyncHooks(ctx, tenant, owner, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim async hook = %d, %v; want one claim", len(claims), err)
	}
	return claims[0]
}

func integrationInvocationCount(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, tenant contract.TenantID) int {
	t.Helper()
	var count int
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_script_invocation WHERE tenant_id = ?`), string(tenant)).Scan(&count); err != nil {
		t.Fatal("count lifecycle invocation rows:", err)
	}
	return count
}

func integrationEffectCount(t *testing.T, ctx context.Context, database *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
		t.Fatal("count local effects:", err)
	}
	return count
}

func integrationInvocationStatus(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, tenant contract.TenantID, source contract.OperationID, sequence uint16) string {
	t.Helper()
	var status string
	query := db.Rebind(dialect, `SELECT status FROM _kora_script_invocation WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ?`)
	if err := database.QueryRowContext(ctx, query, string(tenant), string(source), sequence).Scan(&status); err != nil {
		t.Fatal("read lifecycle invocation status:", err)
	}
	return status
}

func integrationExpireClaim(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, tenant contract.TenantID, source contract.OperationID, sequence uint16) {
	t.Helper()
	query := db.Rebind(dialect, `UPDATE _kora_script_invocation SET lease_until = '2000-01-01 00:00:00' WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ?`)
	if _, err := database.ExecContext(ctx, query, string(tenant), string(source), sequence); err != nil {
		t.Fatal("expire lifecycle hook lease:", err)
	}
}

func integrationMakeDue(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, tenant contract.TenantID, source contract.OperationID, sequence uint16) {
	t.Helper()
	query := db.Rebind(dialect, `UPDATE _kora_script_invocation SET next_attempt_at = '2000-01-01 00:00:00' WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ?`)
	if _, err := database.ExecContext(ctx, query, string(tenant), string(source), sequence); err != nil {
		t.Fatal("make retryable lifecycle hook due:", err)
	}
}

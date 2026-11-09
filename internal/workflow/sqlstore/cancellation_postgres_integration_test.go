//go:build integration

package sqlstore_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/asenawritescode/kora/internal/workflow/sqlstore"
	_ "github.com/lib/pq"
)

func TestPostgresWorkflowCancellationIsFencedAndIdempotent(t *testing.T) {
	dsn := os.Getenv("KORA_WORKFLOW_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_WORKFLOW_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	ctx := context.Background()
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable PostgreSQL:", err)
	}
	dialect := db.Resolve("postgres")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap system tables:", err)
	}
	store, err := sqlstore.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := sqlstore.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow tables:", err)
	}
	tenant := contract.TenantID("cancel-" + contract.NewID())
	instanceID := contract.InstanceID(contract.NewID())
	now := time.Now().UTC()
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,?,?)`), string(instanceID), string(tenant), "cancel-flow", 1, "Invoice", "INV-1", "1", string(contract.InstanceReady), "start", now, now); err != nil {
		t.Fatal("insert cancellable workflow:", err)
	}
	command := workflow.CancelCommit{Tenant: tenant, InstanceID: instanceID, Actor: "operator-1", Site: "tenant.example.test", ExpectedRevision: "1", Reason: "operator requested cancellation", IdempotencyKey: "cancel-workflow-1"}
	first, err := store.Cancel(ctx, command)
	if err != nil {
		t.Fatal("cancel workflow:", err)
	}
	if first.Status != contract.InstanceCancelled || first.Revision != "2" {
		t.Fatalf("cancelled instance = %+v", first)
	}
	replay, err := store.Cancel(ctx, command)
	if err != nil {
		t.Fatal("replay workflow cancellation:", err)
	}
	if replay.ID != first.ID || replay.Revision != first.Revision || replay.Status != first.Status {
		t.Fatalf("cancellation replay = %+v, first = %+v", replay, first)
	}
	var count int
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? AND kind='cancelled'`), string(tenant), string(instanceID)).Scan(&count); err != nil {
		t.Fatal("count cancellation history:", err)
	}
	if count != 1 {
		t.Fatalf("cancellation history count=%d, want 1", count)
	}
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_id=? AND event_type='kora.workflow.cancelled'`), string(instanceID)).Scan(&count); err != nil {
		t.Fatal("count cancellation events:", err)
	}
	if count != 1 {
		t.Fatalf("cancellation event count=%d, want 1", count)
	}
	stale := command
	stale.IdempotencyKey = "cancel-workflow-2"
	if _, err := store.Cancel(ctx, stale); err == nil {
		t.Fatal("stale cancellation revision was accepted")
	}
}

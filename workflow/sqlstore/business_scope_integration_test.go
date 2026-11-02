//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestWorkflowBusinessScopePersistsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_WORKFLOW_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_WORKFLOW_DISPOSABLE_POSTGRES_DSN"},
		{"libsql", "KORA_WORKFLOW_DISPOSABLE_LIBSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to an isolated disposable database", tc.env)
			}
			runWorkflowBusinessScopePersistence(t, tc.driver, dsn)
		})
	}
}

func runWorkflowBusinessScopePersistence(t *testing.T, driver, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable database:", err)
	}
	dialect := db.Resolve(driver)
	for _, statement := range outboxStatements(driver) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("ensure disposable outbox schema: %v\n%s", err, statement)
		}
	}
	tenant := contract.TenantID("scope-test-" + contract.NewID())
	site := "scope-route-" + contract.NewID()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanupWorkflowTenant(t, cleanupCtx, database, dialect, tenant, site)
	})
	definitionID := contract.DefinitionID("scope-def-" + contract.NewID())
	instanceID := contract.InstanceID(contract.NewID())
	definitions, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	instances, err := NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	definition := contract.WorkflowDefinition{ID: definitionID, Version: 1, Entry: "finish", Steps: []contract.StepDefinition{{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "done"}}}}
	now := time.Now().UTC()
	ref, err := definitions.PublishDefinition(ctx, tenant, definition, "actor", contract.IdempotencyKey("publish-"+contract.NewID()), now)
	if err != nil {
		t.Fatal("publish workflow definition:", err)
	}
	scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-ledger-a"}
	started, err := instances.StartInstance(ctx, workflow.StartInstanceInput{Tenant: tenant, BusinessScope: scope, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-SCOPED", Revision: "1"}, Actor: "actor", IdempotencyKey: contract.IdempotencyKey("start-" + contract.NewID()), Site: site, Now: now})
	if err != nil {
		t.Fatal("start scoped workflow:", err)
	}
	if started.BusinessScope != scope {
		t.Fatalf("started scope = %+v, want %+v", started.BusinessScope, scope)
	}
	reloaded, err := instances.GetInstance(ctx, tenant, instanceID)
	if err != nil {
		t.Fatal("reload scoped workflow:", err)
	}
	if reloaded.BusinessScope != scope {
		t.Fatalf("reloaded scope = %+v, want %+v", reloaded.BusinessScope, scope)
	}
	claims, err := executor.ClaimDue(ctx, tenant, "scope-test-worker", 1, 5*time.Second)
	if err != nil {
		t.Fatal("claim scoped workflow:", err)
	}
	if len(claims) != 1 || claims[0].InstanceID != instanceID || claims[0].BusinessScope != scope {
		t.Fatalf("claimed workflow scope did not recover: %+v", claims)
	}
	var payload []byte
	err = database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT payload FROM _kora_outbox WHERE aggregate_id=? AND event_type='kora.workflow.started'`), string(instanceID)).Scan(&payload)
	if err != nil {
		t.Fatal("load started workflow event:", err)
	}
	var event contract.EventEnvelope
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal("decode started workflow event:", err)
	}
	if event.BusinessScope != scope {
		t.Fatalf("started event scope = %+v, want %+v", event.BusinessScope, scope)
	}
}

func outboxStatements(driver string) []string {
	switch driver {
	case "mysql":
		return db.OutboxTablesMySQL()
	case "postgres":
		return db.OutboxTablesPostgres()
	default:
		return db.OutboxTablesLibSQL()
	}
}

func cleanupWorkflowTenant(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, tenant contract.TenantID, site string) {
	t.Helper()
	for _, table := range []string{"_kora_workflow_attempt", "_kora_workflow_history", "_kora_workflow_start_receipt", "_kora_workflow_instance", "_kora_workflow_publish_receipt", "_kora_workflow_definition"} {
		if _, err := database.ExecContext(ctx, db.Rebind(dialect, fmt.Sprintf("DELETE FROM %s WHERE tenant_id=?", table)), string(tenant)); err != nil {
			t.Errorf("cleanup %s: %v", table, err)
		}
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `DELETE FROM _kora_outbox WHERE site=?`), site); err != nil {
		t.Errorf("cleanup workflow outbox: %v", err)
	}
}

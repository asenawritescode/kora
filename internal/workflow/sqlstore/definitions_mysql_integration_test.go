//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLDefinitionPublishIsImmutableTenantScopedAndIdempotent(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_workflow_def_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := NewDefinitionStore(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("ensure disposable outbox schema:", err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	definition := contract.WorkflowDefinition{ID: "purchase", Version: 1, Entry: "end", Steps: []contract.StepDefinition{{ID: "end", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}}}}
	first, err := store.PublishDefinition(ctx, "tenant-a", definition, "user-a", "publish-1", now)
	if err != nil {
		t.Fatal("publish:", err)
	}
	replay, err := store.PublishDefinition(ctx, "tenant-a", definition, "user-a", "publish-1", now.Add(time.Minute))
	if err != nil {
		t.Fatal("idempotent replay:", err)
	}
	if replay != first {
		t.Fatalf("replay reference %+v differs from original %+v", replay, first)
	}
	instanceStore, err := NewInstanceStore(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	instanceID := contract.InstanceID(contract.NewID())
	start := workflow.StartInstanceInput{Tenant: "tenant-a", ID: instanceID, Definition: definition, DefinitionDigest: first.Digest, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "1"}, Actor: "user-a", IdempotencyKey: "workflow-start-1", Site: "tenant-route", Now: now}
	started, err := instanceStore.StartInstance(ctx, start)
	if err != nil {
		t.Fatal("start instance:", err)
	}
	if started.Status != contract.InstanceReady || started.CurrentStep != "end" || started.DefinitionVersion != 1 {
		t.Fatalf("unexpected started instance: %+v", started)
	}
	replayedStart := start
	replayedStart.ID = contract.InstanceID(contract.NewID())
	replayedInstance, err := instanceStore.StartInstance(ctx, replayedStart)
	if err != nil {
		t.Fatal("replay start:", err)
	}
	if replayedInstance.ID != instanceID {
		t.Fatalf("idempotent start returned new ID %s, want %s", replayedInstance.ID, instanceID)
	}
	concurrent := start
	concurrent.IdempotencyKey = "workflow-start-race-1"
	var wg sync.WaitGroup
	results := make(chan contract.WorkflowInstance, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt := concurrent
			attempt.ID = contract.InstanceID(contract.NewID())
			instance, startErr := instanceStore.StartInstance(ctx, attempt)
			if startErr != nil {
				errs <- startErr
				return
			}
			results <- instance
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for startErr := range errs {
		t.Errorf("concurrent idempotent start: %v", startErr)
	}
	var raceID contract.InstanceID
	for instance := range results {
		if raceID == "" {
			raceID = instance.ID
		} else if raceID != instance.ID {
			t.Errorf("concurrent starts returned different IDs %s and %s", raceID, instance.ID)
		}
	}
	if raceID == "" {
		t.Fatal("concurrent starts returned no instance")
	}
	changedStart := start
	changedStart.Subject.Revision = "2"
	if _, err := instanceStore.StartInstance(ctx, changedStart); err == nil {
		t.Fatal("changed subject reused a workflow start key")
	}
	var startedEvents int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type = 'kora.workflow.started'`).Scan(&startedEvents); err != nil {
		t.Fatal(err)
	}
	if startedEvents != 2 {
		t.Fatalf("workflow started event count = %d, want 2", startedEvents)
	}
	changed := definition
	changed.Steps = append([]contract.StepDefinition(nil), definition.Steps...)
	changed.Steps[0].End = &contract.EndSpec{Outcome: "different"}
	if _, err := store.PublishDefinition(ctx, "tenant-a", changed, "user-a", "publish-1", now); err == nil {
		t.Fatal("same idempotency key with different content succeeded")
	}
	if _, err := store.PublishDefinition(ctx, "tenant-a", changed, "user-a", "publish-2", now); err == nil {
		t.Fatal("changed immutable definition version succeeded")
	}
	loaded, ref, err := store.GetDefinition(ctx, "tenant-a", definition.ID, 1)
	if err != nil {
		t.Fatal("load definition:", err)
	}
	if loaded.Steps[0].End.Outcome != "complete" || ref != first {
		t.Fatalf("persisted definition changed: %+v %+v", loaded, ref)
	}
	if _, _, err := store.GetDefinition(ctx, "tenant-b", definition.ID, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant definition read = %v, want sql.ErrNoRows", err)
	}
}

func TestMySQLWorkflowStoreRetiresLegacyRuntimeTables(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_workflow_retire_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, table := range []string{"_kora_workflow_audit", "_kora_workflow_timer", "_kora_workflow_actor"} {
		if _, err := database.ExecContext(ctx, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY)"); err != nil {
			t.Fatalf("create retired table %s: %v", table, err)
		}
		if _, err := database.ExecContext(ctx, "INSERT INTO `"+table+"` (id) VALUES (1)"); err != nil {
			t.Fatalf("seed retired table %s: %v", table, err)
		}
	}
	store, err := NewDefinitionStore(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(ctx); err != nil {
		t.Fatal("ensure unified workflow schema:", err)
	}
	for _, table := range []string{"_kora_workflow_audit", "_kora_workflow_timer", "_kora_workflow_actor"} {
		var count int
		if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", table).Scan(&count); err != nil {
			t.Fatalf("check retired table %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("retired workflow table %s still exists", table)
		}
	}
	var unifiedCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='_kora_workflow_definition'").Scan(&unifiedCount); err != nil {
		t.Fatal("check unified workflow storage:", err)
	}
	if unifiedCount != 1 {
		t.Fatal("unified workflow definition table was not initialized")
	}
}

//go:build integration

package site

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/internal/db"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
)

func TestBootstrapMigratesExistingSystemAndKernelReceiptSchema(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open mysql:", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_bootstrap_receipt_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse mysql DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE _kora_idempotency_receipt (
		site VARCHAR(140) NOT NULL DEFAULT '', idempotency_key VARCHAR(255) NOT NULL,
		operation_id VARCHAR(26) NOT NULL, command_name VARCHAR(190) NOT NULL,
		payload_hash VARCHAR(64) NOT NULL, result_hash VARCHAR(64) NOT NULL DEFAULT '',
		status VARCHAR(20) NOT NULL DEFAULT 'completed', actor_user VARCHAR(140) NOT NULL DEFAULT '',
		created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
		PRIMARY KEY (site, idempotency_key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		t.Fatal("create pre-migration receipt table:", err)
	}
	// Model an existing v17 outbox whose tenant source-sequence column and
	// bounded backlog-sampling index predate the current bootstrap revision.
	if _, err := database.Exec(`CREATE TABLE _kora_outbox (
		id VARCHAR(26) PRIMARY KEY, site VARCHAR(140) NOT NULL DEFAULT '',
		event_type VARCHAR(255) NOT NULL, event_version INT NOT NULL DEFAULT 1,
		aggregate_type VARCHAR(140) NOT NULL DEFAULT '', aggregate_id VARCHAR(255) NOT NULL DEFAULT '',
		payload JSON, status VARCHAR(20) NOT NULL DEFAULT 'pending', attempts INT NOT NULL DEFAULT 0,
		next_attempt_at DATETIME(6), lease_owner VARCHAR(140) NOT NULL DEFAULT '', lease_until DATETIME(6),
		created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), published_at DATETIME(6), last_error TEXT,
		INDEX idx_outbox_pending (status, next_attempt_at), INDEX idx_outbox_site (site),
		INDEX idx_outbox_aggregate (aggregate_type, aggregate_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		t.Fatal("create pre-migration outbox table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox (id,site,event_type,status) VALUES ('00000000000000000000000001','bootstrap-test','request.created','pending')`); err != nil {
		t.Fatal("seed pre-migration durable outbox event:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_outbox_receipt (
		consumer_name VARCHAR(140) NOT NULL, event_id VARCHAR(255) NOT NULL,
		site VARCHAR(140) NOT NULL DEFAULT '', received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
		PRIMARY KEY (consumer_name, event_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		t.Fatal("create pre-migration outbox receipt table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox_receipt (consumer_name,event_id,site) VALUES ('legacy-consumer','legacy-event','bootstrap-test')`); err != nil {
		t.Fatal("seed pre-migration outbox receipt:", err)
	}
	requestDDL, err := requestsql.SchemaStatements(db.Resolve("mysql"))
	if err != nil {
		t.Fatal("create pre-migration request schema:", err)
	}
	for _, statement := range requestDDL {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create pre-migration request object: %v", err)
		}
	}
	if _, err := database.Exec(`ALTER TABLE _kora_request DROP INDEX idx_kora_request_workflow_source`); err != nil {
		t.Fatal("simulate tenant without workflow-source index:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_system_bootstrap (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		t.Fatal("create existing bootstrap marker:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_system_bootstrap (id, version) VALUES (1, 10)`); err != nil {
		t.Fatal("seed v10 bootstrap marker:", err)
	}
	if err := BootstrapSystemTables(context.Background(), database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap existing site:", err)
	}
	var outboxSequenceColumns, backlogIndexes, retainedEvents, tenantSequenceTables, receiptSequenceColumns, retainedReceipts, kernelReceiptIndexes int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='_kora_outbox' AND column_name='source_sequence'`).Scan(&outboxSequenceColumns); err != nil {
		t.Fatal("check migrated outbox source sequence:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(DISTINCT index_name) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='_kora_outbox' AND index_name='idx_outbox_status_created_at'`).Scan(&backlogIndexes); err != nil {
		t.Fatal("check migrated outbox backlog index:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox WHERE id='00000000000000000000000001'`).Scan(&retainedEvents); err != nil {
		t.Fatal("check existing outbox event after migration:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='_kora_tenant_event_sequence'`).Scan(&tenantSequenceTables); err != nil {
		t.Fatal("check tenant event-sequence singleton table:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='_kora_outbox_receipt' AND column_name='source_sequence'`).Scan(&receiptSequenceColumns); err != nil {
		t.Fatal("check migrated receipt source sequence:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox_receipt WHERE consumer_name='legacy-consumer' AND event_id='legacy-event' AND source_sequence IS NULL`).Scan(&retainedReceipts); err != nil {
		t.Fatal("check retained outbox receipt:", err)
	}
	if outboxSequenceColumns != 1 || backlogIndexes != 1 || retainedEvents != 1 || tenantSequenceTables != 1 || receiptSequenceColumns != 1 || retainedReceipts != 1 {
		t.Fatalf("outbox migration columns/index/retained events/sequence singleton/receipt sequence/retained receipts = %d/%d/%d/%d/%d/%d, want 1/1/1/1/1/1", outboxSequenceColumns, backlogIndexes, retainedEvents, tenantSequenceTables, receiptSequenceColumns, retainedReceipts)
	}
	if _, err := database.Query(`SELECT result_json FROM _kora_idempotency_receipt LIMIT 0`); err != nil {
		t.Fatalf("bootstrap did not add receipt result column: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='_kora_idempotency_receipt' AND index_name='idx_idem_key'`).Scan(&kernelReceiptIndexes); err != nil {
		t.Fatal("check stable-tenant idempotency receipt index:", err)
	}
	if kernelReceiptIndexes != 1 {
		t.Fatalf("stable-tenant idempotency receipt indexes = %d, want one", kernelReceiptIndexes)
	}
	for _, table := range []string{"_kora_request", "_kora_request_assignment", "_kora_request_decision", "_kora_request_history", "_kora_request_read", "_kora_request_idempotency", "_kora_auth_revision", "_kora_config_revision", "_kora_management_activation_fence", "_kora_notification_event", "_kora_notification_delivery", "_kora_workflow_definition", "_kora_workflow_publish_receipt", "_kora_workflow_instance", "_kora_workflow_attempt", "_kora_workflow_process_timer", "_kora_workflow_signal", "_kora_workflow_signal_capacity", "_kora_workflow_subscription", "_kora_workflow_signal_receipt", "_kora_workflow_history", "_kora_workflow_start_receipt", "_kora_workflow_reconcile_receipt"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("check request schema table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("bootstrap did not create request schema table %s", table)
		}
	}
	if _, err := database.Query(`SELECT assignment_mode FROM _kora_request LIMIT 0`); err != nil {
		t.Fatalf("bootstrap did not add request assignment mode column: %v", err)
	}
	if _, err := database.Query(`SELECT previous_assignee_json, new_assignee_json FROM _kora_request_history LIMIT 0`); err != nil {
		t.Fatalf("bootstrap did not add immutable reassignment history columns: %v", err)
	}
	var workflowSourceIndex int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = '_kora_request' AND index_name = 'idx_kora_request_workflow_source'`).Scan(&workflowSourceIndex); err != nil {
		t.Fatal("check migrated workflow request source index:", err)
	}
	if workflowSourceIndex == 0 {
		t.Fatal("bootstrap did not add workflow request source index to existing tenant")
	}
	if _, err := database.Query(`SELECT definition_version FROM _kora_workflow_instance LIMIT 0`); err != nil {
		t.Fatalf("bootstrap did not create durable workflow instance schema: %v", err)
	}
	var policyRevision, membershipRevision int64
	if err := database.QueryRow(`SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1`).Scan(&policyRevision, &membershipRevision); err != nil {
		t.Fatal("read authorization revision seed:", err)
	}
	if policyRevision != 1 || membershipRevision != 1 {
		t.Fatalf("authorization revision seed = (%d, %d), want (1, 1)", policyRevision, membershipRevision)
	}
	var version int
	if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal("read migrated bootstrap marker:", err)
	}
	if version != systemBootstrapVersion {
		t.Fatalf("bootstrap marker version = %d, want %d", version, systemBootstrapVersion)
	}

	// A routine restart must be a no-op after the successful migration.
	if err := BootstrapSystemTables(context.Background(), database, db.Resolve("mysql")); err != nil {
		t.Fatalf("repeat bootstrap after migration: %v", err)
	}
	if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal("read bootstrap marker after repeat:", err)
	}
	if version != systemBootstrapVersion {
		t.Fatalf("bootstrap marker changed after repeat: got %d, want %d", version, systemBootstrapVersion)
	}
}

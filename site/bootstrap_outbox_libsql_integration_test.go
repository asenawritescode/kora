//go:build integration

package site

import (
	"database/sql"
	"os"
	"testing"

	"github.com/asenawritescode/kora/db"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestBootstrapMigratesExistingLibSQLV17OutboxWithoutLosingRows(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_BOOTSTRAP_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_BOOTSTRAP_DISPOSABLE_LIBSQL_DSN to a fresh disposable LibSQL HTTP endpoint")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP endpoint:", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP endpoint:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_outbox (
		id TEXT PRIMARY KEY, site TEXT NOT NULL DEFAULT '', event_type TEXT NOT NULL,
		event_version INTEGER NOT NULL DEFAULT 1, aggregate_type TEXT NOT NULL DEFAULT '', aggregate_id TEXT NOT NULL DEFAULT '',
		payload TEXT, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at TEXT, lease_owner TEXT NOT NULL DEFAULT '', lease_until TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')), published_at TEXT, last_error TEXT
	)`); err != nil {
		t.Fatal("create pre-migration LibSQL outbox table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox (id,site,event_type,status) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FAV','bootstrap-test','request.created','pending')`); err != nil {
		t.Fatal("seed pre-migration LibSQL outbox event:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_outbox_receipt (
		consumer_name TEXT NOT NULL, event_id TEXT NOT NULL, site TEXT NOT NULL DEFAULT '',
		received_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (consumer_name, event_id)
	)`); err != nil {
		t.Fatal("create pre-migration LibSQL outbox receipt table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox_receipt (consumer_name,event_id,site) VALUES ('legacy-consumer','legacy-event','bootstrap-test')`); err != nil {
		t.Fatal("seed pre-migration LibSQL outbox receipt:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_system_bootstrap (id INTEGER PRIMARY KEY, version INTEGER NOT NULL)`); err != nil {
		t.Fatal("create bootstrap marker:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_system_bootstrap (id,version) VALUES (1,17)`); err != nil {
		t.Fatal("seed v17 bootstrap marker:", err)
	}
	if err := BootstrapSystemTables(database, db.Resolve("libsql")); err != nil {
		t.Fatal("upgrade v17 LibSQL system tables:", err)
	}
	var sourceSequence int64
	if err := database.QueryRow(`SELECT source_sequence FROM _kora_outbox WHERE id='01ARZ3NDEKTSV4RRFFQ69G5FAV'`).Scan(&sourceSequence); err != nil {
		t.Fatal("read migrated outbox sequence:", err)
	}
	var backlogIndexes, retainedRows, version, tenantSequenceTables, receiptRows, receiptSequenceColumns, invitationJournalTables int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_outbox_status_created_at'`).Scan(&backlogIndexes); err != nil {
		t.Fatal("check migrated outbox backlog index:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox WHERE id='01ARZ3NDEKTSV4RRFFQ69G5FAV'`).Scan(&retainedRows); err != nil {
		t.Fatal("check retained outbox row:", err)
	}
	if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id=1`).Scan(&version); err != nil {
		t.Fatal("read upgraded bootstrap marker:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_kora_tenant_event_sequence'`).Scan(&tenantSequenceTables); err != nil {
		t.Fatal("check LibSQL tenant event-sequence singleton table:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox_receipt WHERE consumer_name='legacy-consumer' AND event_id='legacy-event' AND source_sequence IS NULL`).Scan(&receiptRows); err != nil {
		t.Fatal("check preserved LibSQL consumer receipt:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('_kora_outbox_receipt') WHERE name='source_sequence'`).Scan(&receiptSequenceColumns); err != nil {
		t.Fatal("check migrated LibSQL receipt source-sequence column:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_kora_magic_link_delivery'`).Scan(&invitationJournalTables); err != nil {
		t.Fatal("check LibSQL invitation delivery journal:", err)
	}
	if sourceSequence != 0 || backlogIndexes != 1 || retainedRows != 1 || version != systemBootstrapVersion || tenantSequenceTables != 1 || receiptRows != 1 || receiptSequenceColumns != 1 || invitationJournalTables != 1 {
		t.Fatalf("LibSQL outbox migration sequence/index/row/bootstrap/singleton/receipt/receipt-sequence/invitation-journal = %d/%d/%d/%d/%d/%d/%d/%d; want 0/1/1/%d/1/1/1/1", sourceSequence, backlogIndexes, retainedRows, version, tenantSequenceTables, receiptRows, receiptSequenceColumns, invitationJournalTables, systemBootstrapVersion)
	}
}

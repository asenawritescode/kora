package analytics

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestBootstrapTablesContextDoesNotReachDatabaseAfterCancellation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create database:", err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := BootstrapTablesContext(ctx, database, db.Resolve("mysql")); !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap error = %v, want context canceled", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("canceled bootstrap reached database: %v", err)
	}
}

func TestInsertEventReceiptPersistsCanonicalSourceSequence(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_analytics_event_receipt (site, event_id, source_sequence, processed_at) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE event_id = event_id`)).
		WithArgs("tenant", "event-1", int64(17), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := insertEventReceiptTx(context.Background(), tx, db.Resolve("mysql"), "tenant", "event-1", "17")
	if err != nil || !inserted {
		t.Fatalf("insert receipt = (%t, %v), want inserted", inserted, err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInsertEventReceiptRejectsNoncanonicalSourceSequence(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertEventReceiptTx(context.Background(), tx, db.Resolve("mysql"), "tenant", "event-1", "017"); err == nil {
		t.Fatal("noncanonical source sequence accepted")
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsDDLUsesDialectCompatibleTypesAndIndexes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dialect   db.Dialect
		forbidden []string
		required  []string
	}{
		{"postgres", db.Resolve("postgres"), []string{"AUTO_INCREMENT", "DATETIME", "INDEX idx"}, []string{"BIGSERIAL PRIMARY KEY", "DOUBLE PRECISION", "CREATE INDEX IF NOT EXISTS"}},
		{"libsql", db.Resolve("libsql"), []string{"AUTO_INCREMENT", "INDEX idx"}, []string{"INTEGER PRIMARY KEY AUTOINCREMENT", "CREATE INDEX IF NOT EXISTS"}},
		{"mysql", db.Resolve("mysql"), []string{}, []string{"AUTO_INCREMENT", "DATETIME", "INDEX idx_analytics"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(rollupTableDDL(tc.dialect), "\n")
			for _, token := range tc.forbidden {
				if strings.Contains(joined, token) {
					t.Errorf("DDL contains unsupported token %q", token)
				}
			}
			for _, token := range tc.required {
				if !strings.Contains(joined, token) {
					t.Errorf("DDL is missing %q", token)
				}
			}
			if !strings.Contains(joined, "_kora_analytics_event_receipt") || !strings.Contains(joined, "PRIMARY KEY (site, event_id)") {
				t.Error("DDL is missing the tenant-scoped durable event receipt")
			}
			if !strings.Contains(joined, "source_sequence BIGINT NULL") {
				t.Error("analytics receipts do not preserve the optional source sequence")
			}
			for _, token := range []string{"scope_kind VARCHAR(32)", "legal_entity_id VARCHAR(140)", "_kora_analytics_module_projection", "snapshot_key CHAR(64)", "source_cursor VARCHAR(2048)"} {
				if !strings.Contains(joined, token) {
					t.Errorf("fresh analytics schema is missing %q", token)
				}
			}
			for _, table := range []string{"_kora_analytics_rebuild_lock", "_kora_analytics_rebuild_checkpoint", "_kora_analytics_rebuild_lease"} {
				if !strings.Contains(joined, table) {
					t.Errorf("DDL is missing rebuild coordination table %s", table)
				}
			}
		})
	}
}

func TestBootstrapRejectsOlderAnalyticsSchemaWithoutMigratingIt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS _kora_analytics_bootstrap (\n\t\tid INTEGER PRIMARY KEY,\n\t\tversion INTEGER NOT NULL\n\t)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT version FROM _kora_analytics_bootstrap WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(6))
	if err := BootstrapTables(database, db.Resolve("mysql")); err == nil || !strings.Contains(err.Error(), "create a fresh tenant database") {
		t.Fatalf("older schema error = %v, want fresh-database guidance", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

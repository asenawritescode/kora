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
			for _, table := range []string{"_kora_analytics_rebuild_lock", "_kora_analytics_rebuild_checkpoint", "_kora_analytics_rebuild_lease"} {
				if !strings.Contains(joined, table) {
					t.Errorf("DDL is missing rebuild coordination table %s", table)
				}
			}
		})
	}
}

func TestBootstrapUpgradesExistingAnalyticsSchemaToReceipts(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS _kora_analytics_bootstrap (\n\t\tid INTEGER PRIMARY KEY,\n\t\tversion INTEGER NOT NULL\n\t)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT version FROM _kora_analytics_bootstrap WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	for _, statement := range rollupTableDDL(db.Resolve("mysql")) {
		mock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = '_kora_analytics_event_receipt' AND COLUMN_NAME = 'source_sequence'`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_analytics_bootstrap SET version = 5 WHERE id = 1`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := BootstrapTables(database, db.Resolve("mysql")); err != nil {
		t.Fatalf("upgrade analytics schema: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapVersionFourAddsReceiptSourceSequence(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS _kora_analytics_bootstrap (\n\t\tid INTEGER PRIMARY KEY,\n\t\tversion INTEGER NOT NULL\n\t)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT version FROM _kora_analytics_bootstrap WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
	for _, statement := range rollupTableDDL(db.Resolve("mysql")) {
		mock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = '_kora_analytics_event_receipt' AND COLUMN_NAME = 'source_sequence'`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta("ALTER TABLE `_kora_analytics_event_receipt` ADD COLUMN `source_sequence` BIGINT NULL")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_analytics_bootstrap SET version = 5 WHERE id = 1`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := BootstrapTables(database, db.Resolve("mysql")); err != nil {
		t.Fatalf("upgrade analytics schema from version four: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsReceiptSequenceMigrationAddsMissingColumnForEachDialect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect db.Dialect
		probe   string
		alter   string
	}{
		{
			name: "mysql", dialect: db.Resolve("mysql"),
			probe: `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = '_kora_analytics_event_receipt' AND COLUMN_NAME = 'source_sequence'`,
			alter: "ALTER TABLE `_kora_analytics_event_receipt` ADD COLUMN `source_sequence` BIGINT NULL",
		},
		{
			name: "postgres", dialect: db.Resolve("postgres"),
			probe: `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = '_kora_analytics_event_receipt' AND column_name = 'source_sequence'`,
			alter: `ALTER TABLE "_kora_analytics_event_receipt" ADD COLUMN "source_sequence" BIGINT NULL`,
		},
		{
			name: "libsql", dialect: db.Resolve("libsql"),
			probe: `PRAGMA table_info('_kora_analytics_event_receipt')`,
			alter: `ALTER TABLE "_kora_analytics_event_receipt" ADD COLUMN "source_sequence" BIGINT NULL`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if tc.name == "libsql" {
				mock.ExpectQuery(regexp.QuoteMeta(tc.probe)).WillReturnRows(sqlmock.NewRows([]string{"cid", "name", "type", "notnull", "dflt_value", "pk"}).AddRow(0, "site", "TEXT", 1, nil, 0))
			} else {
				mock.ExpectQuery(regexp.QuoteMeta(tc.probe)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			}
			mock.ExpectExec(regexp.QuoteMeta(tc.alter)).WillReturnResult(sqlmock.NewResult(0, 0))
			if err := ensureReceiptSequenceColumn(context.Background(), database, tc.dialect); err != nil {
				t.Fatalf("ensure receipt sequence: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

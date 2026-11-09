//go:build integration

package doctype_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLRemovedFieldDataIsQuarantined(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	if !strings.Contains(dsn, "tcp(127.0.0.1:") && !strings.Contains(dsn, "tcp(localhost:") {
		t.Fatalf("refusing non-loopback MySQL DSN for destructive integration test")
	}

	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	schema := fmt.Sprintf("kora_config_quarantine_%d", os.Getpid())
	if _, err := database.Exec("CREATE DATABASE `" + schema + "`"); err != nil {
		t.Fatal(err)
	}
	defer database.Exec("DROP DATABASE `" + schema + "`")
	if _, err := database.Exec("USE `" + schema + "`"); err != nil {
		t.Fatal(err)
	}

	table := "tab_quarantine_test"
	quarantine := doctype.QuarantineNameForRevision("legacy_value", "integration-revision")
	if _, err := database.Exec("DROP TABLE IF EXISTS `" + table + "`"); err != nil {
		t.Fatal(err)
	}
	defer database.Exec("DROP TABLE IF EXISTS `" + table + "`")
	if _, err := database.Exec("CREATE TABLE `" + table + "` (id VARCHAR(36) PRIMARY KEY, legacy_value TEXT NOT NULL) ENGINE=InnoDB"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO `"+table+"` (id, legacy_value) VALUES (?, ?)", "r1", "preserve-me"); err != nil {
		t.Fatal(err)
	}
	statements, err := doctype.GenerateDDLFromDiff([]doctype.Change{{
		Type: "remove-field", Entity: "_quarantine_test", Field: "legacy_value",
		Attrs: map[string]any{":quarantine-name": quarantine},
	}}, db.MySQL())
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 {
		t.Fatalf("got %d DDL statements, want one", len(statements))
	}
	// The generator prefixes entities with "tab"; use a quoted identifier
	// from the generated statement while keeping this disposable table unique.
	stmt := strings.ReplaceAll(statements[0], "`tab_quarantine_test`", "`"+table+"`")
	if _, err := database.Exec(stmt); err != nil {
		t.Fatalf("apply quarantine DDL %q: %v", stmt, err)
	}
	var preserved string
	query := fmt.Sprintf("SELECT `%s` FROM `%s` WHERE id = ?", quarantine, table)
	if err := database.QueryRow(query, "r1").Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if preserved != "preserve-me" {
		t.Fatalf("quarantined value = %q, want original value", preserved)
	}
}

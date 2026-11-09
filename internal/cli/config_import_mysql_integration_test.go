//go:build integration

package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLConfigImportRetriesJournaledPackWithoutDuplicateVersion(t *testing.T) {
	rawDSN := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
	if rawDSN == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	adminConfig, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN")
	}
	adminConfig.DBName = ""
	adminDB, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL admin connection")
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping disposable MySQL")
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate disposable schema name:", err)
	}
	databaseName := "kora_import_test_" + hex.EncodeToString(random[:])
	if _, err := adminDB.Exec("CREATE DATABASE `" + databaseName + "`"); err != nil {
		t.Fatal("create disposable import schema:", err)
	}
	t.Cleanup(func() { _, _ = adminDB.Exec("DROP DATABASE `" + databaseName + "`") })

	tenantConfig := *adminConfig
	tenantConfig.DBName = databaseName
	dsn := tenantConfig.FormatDSN()
	t.Setenv("KORA_DB_TYPE", "mysql")
	t.Setenv("KORA_DB_USER", "")
	t.Setenv("KORA_DB_PASSWORD", "")
	t.Setenv("DB_DSN", dsn)
	t.Setenv("KORA_DB_MAX_OPEN", "4")
	pack := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pack, "doctypes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pack, "workflows"), 0755); err != nil {
		t.Fatal(err)
	}
	config := "name: Task\nmodule: Test\nfields:\n  - fieldname: title\n    fieldtype: Data\n    label: Title\n"
	if err := os.WriteFile(filepath.Join(pack, "doctypes", "task.yaml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	workflow := `{"id":"imported-task-created","version":1,"entry":"done","trigger":{"kind":"record.created","table":"Task"},"steps":[{"id":"done","kind":"end","end":{"outcome":"complete"}}]}`
	if err := os.WriteFile(filepath.Join(pack, "workflows", "task-created.json"), []byte(workflow), 0644); err != nil {
		t.Fatal(err)
	}

	tenantDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable tenant schema")
	}
	tenantDB.SetMaxOpenConns(2)
	if err := site.BootstrapSystemTables(context.Background(), tenantDB, db.MySQL()); err != nil {
		tenantDB.Close()
		t.Fatal("bootstrap config import system tables:", err)
	}
	if _, err := tenantDB.Exec(`CREATE TRIGGER fail_import_version BEFORE INSERT ON _kora_config_version FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected config import version failure'`); err != nil {
		tenantDB.Close()
		t.Fatal("install disposable failure trigger:", err)
	}
	if err := tenantDB.Close(); err != nil {
		t.Fatal("close setup connection:", err)
	}

	if err := runConfigImport(context.Background(), "import-test.example", pack, databaseName); err == nil || !strings.Contains(err.Error(), "injected config import version failure") {
		t.Fatalf("first import error = %v, want injected config-version failure", err)
	}
	var failedPhase, failureMessage string
	if err := adminDB.QueryRow("SELECT phase, last_error FROM `"+databaseName+"`._kora_config_operation WHERE site = ? AND kind = 'import'", "import-test.example").Scan(&failedPhase, &failureMessage); err != nil {
		t.Fatal("read failed import state:", err)
	}
	if failedPhase != configstore.ActivationApplying || !strings.Contains(failureMessage, "injected config import version failure") {
		t.Fatalf("failed import journal = phase %q error %q", failedPhase, failureMessage)
	}
	var revisionAfterRollback uint64
	if err := adminDB.QueryRow("SELECT revision FROM `" + databaseName + "`._kora_config_revision WHERE id = 1").Scan(&revisionAfterRollback); err != nil {
		t.Fatal("read revision after rolled-back activation:", err)
	}
	if revisionAfterRollback != 1 {
		t.Fatalf("config revision after rejected activation = %d, want unchanged revision 1", revisionAfterRollback)
	}
	if _, err := adminDB.Exec("DROP TRIGGER `" + databaseName + "`.fail_import_version"); err != nil {
		t.Fatal("remove disposable failure trigger:", err)
	}
	if err := runConfigImport(context.Background(), "import-test.example", pack, databaseName); err != nil {
		t.Fatalf("retry journaled import: %v", err)
	}
	if err := runConfigImport(context.Background(), "import-test.example", pack, databaseName); err != nil {
		t.Fatalf("repeat completed import: %v", err)
	}

	var operationCount, activeVersionCount int
	if err := adminDB.QueryRow("SELECT COUNT(*) FROM `"+databaseName+"`._kora_config_operation WHERE site = ? AND kind = 'import'", "import-test.example").Scan(&operationCount); err != nil {
		t.Fatal("read import operation journal:", err)
	}
	if err := adminDB.QueryRow("SELECT COUNT(*) FROM `"+databaseName+"`._kora_config_version WHERE site = ? AND status = 'Active'", "import-test.example").Scan(&activeVersionCount); err != nil {
		t.Fatal("count imported active versions:", err)
	}
	if operationCount != 1 || activeVersionCount != 1 {
		t.Fatalf("journal/version counts = %d/%d, want 1/1", operationCount, activeVersionCount)
	}
	var phase, lastError string
	if err := adminDB.QueryRow("SELECT phase, last_error FROM `"+databaseName+"`._kora_config_operation WHERE site = ? AND kind = 'import'", "import-test.example").Scan(&phase, &lastError); err != nil {
		t.Fatal("read completed import state:", err)
	}
	if phase != configstore.ActivationCompleted || lastError != "" {
		t.Fatalf("import operation = phase %q error %q, want completed without error", phase, lastError)
	}
	var workflowCount int
	if err := adminDB.QueryRow("SELECT COUNT(*) FROM `"+databaseName+"`._kora_workflow_definition WHERE tenant_id = ? AND definition_id = ?", "import-test.example", "imported-task-created").Scan(&workflowCount); err != nil {
		t.Fatal("count imported workflow definitions:", err)
	}
	if workflowCount != 1 {
		t.Fatalf("imported workflow versions = %d, want one idempotent definition", workflowCount)
	}
	var finalRevision uint64
	if err := adminDB.QueryRow("SELECT revision FROM `" + databaseName + "`._kora_config_revision WHERE id = 1").Scan(&finalRevision); err != nil {
		t.Fatal("read final config revision:", err)
	}
	if finalRevision != 2 {
		t.Fatalf("config revision after retry/replay = %d, want 2", finalRevision)
	}
}

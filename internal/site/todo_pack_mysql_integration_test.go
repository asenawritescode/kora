//go:build integration

package site

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/go-sql-driver/mysql"
)

// TestMySQLTodoPackInstallsWithoutLegacyWorkflowConfig runs only with an
// explicitly supplied disposable MySQL service. Its historical package is
// captured from published 1.1.0 (Cloud artifact digest
// 0f39c3d2a06709ab0c7d841ee3e3e2970a3f7753d3527b805ecb8cbfd041122d); the
// fixture's own JSON bytes are pinned below.
func TestMySQLTodoPackInstallsWithoutLegacyWorkflowConfig(t *testing.T) {
	rawDSN := strings.TrimSpace(os.Getenv("KORA_TODO_DISPOSABLE_MYSQL_DSN"))
	if rawDSN == "" {
		t.Skip("set KORA_TODO_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	adminConfig, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	adminConfig.DBName = ""
	adminDB, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL admin connection:", err)
	}
	adminDB.SetMaxOpenConns(2)
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping disposable MySQL:", err)
	}
	t.Cleanup(func() {
		if err := adminDB.Close(); err != nil {
			t.Errorf("close disposable MySQL admin connection: %v", err)
		}
	})

	install := func(name string, files []PackFile) (*sql.DB, string) {
		t.Helper()
		var randomBytes [8]byte
		if _, err := rand.Read(randomBytes[:]); err != nil {
			t.Fatal("generate disposable schema name:", err)
		}
		databaseName := "kora_todo_" + name + "_" + hex.EncodeToString(randomBytes[:])
		if _, err := adminDB.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatal("create disposable tenant schema:", err)
		}
		t.Cleanup(func() {
			if _, err := adminDB.Exec("DROP DATABASE `" + databaseName + "`"); err != nil {
				t.Errorf("drop disposable schema %q: %v", databaseName, err)
			}
		})
		tenantConfig := *adminConfig
		tenantConfig.DBName = databaseName
		tenantDB, err := sql.Open("mysql", tenantConfig.FormatDSN())
		if err != nil {
			t.Fatal("open disposable tenant schema:", err)
		}
		tenantDB.SetMaxOpenConns(4)
		t.Cleanup(func() {
			if err := tenantDB.Close(); err != nil {
				t.Errorf("close disposable tenant schema: %v", err)
			}
		})
		if err := tenantDB.Ping(); err != nil {
			t.Fatal("ping disposable tenant schema:", err)
		}
		dialect := db.Resolve("mysql")
		if err := BootstrapSystemTables(tenantDB, dialect); err != nil {
			t.Fatal("bootstrap Engine system tables:", err)
		}
		if err := ImportConfigFromPack(tenantDB, doctype.NewRegistry(), databaseName, name+".example.test", "todo", files, dialect); err != nil {
			t.Fatalf("import %s Todo pack: %v", name, err)
		}
		return tenantDB, databaseName
	}

	currentFiles := loadConfigProfilePackFiles(t, "todo")
	freshDB, freshName := install("fresh", currentFiles)
	assertTodoInstall := func(tenantDB *sql.DB, databaseName, label string) {
		t.Helper()
		for _, column := range []string{"assignee", "blocked_reason", "cancellation_reason", "reopen_reason"} {
			var count int
			if err := tenantDB.QueryRow("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'tabTodo' AND column_name = ?", databaseName, column).Scan(&count); err != nil {
				t.Fatalf("%s check Todo.%s column: %v", label, column, err)
			}
			if count != 1 {
				t.Errorf("%s Todo.%s column count = %d, want 1", label, column, count)
			}
		}
		var workflowRows int
		if err := tenantDB.QueryRow("SELECT COUNT(*) FROM _kora_workflow_definition WHERE tenant_id = ?", "todo").Scan(&workflowRows); err != nil {
			t.Fatalf("%s check retired workflow config: %v", label, err)
		}
		if workflowRows != 0 {
			t.Errorf("%s imported %d retired lifecycle workflow definitions", label, workflowRows)
		}
		var titleIndex int
		if err := tenantDB.QueryRow("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = ? AND table_name = 'tabTodo' AND column_name = 'title' AND non_unique = 1", databaseName).Scan(&titleIndex); err != nil {
			t.Fatalf("%s check searchable title index: %v", label, err)
		}
		if titleIndex != 1 {
			t.Errorf("%s Todo title non-unique index count = %d, want 1", label, titleIndex)
		}
	}
	assertTodoInstall(freshDB, freshName, "fresh")

}

//go:build integration

package mcp

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	_ "github.com/go-sql-driver/mysql"
)

// This uses connection-scoped temporary tables so it cannot alter a tenant
// schema. KORA_MCP_READ_MYSQL_DSN must point to a disposable MySQL service.
func TestExtensionReadExecutorMySQLOwnerScopedRead(t *testing.T) {
	dsn := os.Getenv("KORA_MCP_READ_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_MCP_READ_MYSQL_DSN to a disposable MySQL service")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	defer database.Close()
	if err := database.PingContext(context.Background()); err != nil {
		t.Fatal("connect disposable MySQL:", err)
	}

	for _, statement := range []string{
		`CREATE TEMPORARY TABLE _kora_extension (name VARCHAR(191), api_permissions TEXT, site VARCHAR(191), access_token VARCHAR(255), access_token_hash CHAR(64) NOT NULL DEFAULT '', is_active TINYINT NOT NULL)`,
		`CREATE TEMPORARY TABLE tabTask (title VARCHAR(255), name VARCHAR(191) PRIMARY KEY, owner VARCHAR(191), creation DATETIME, modified DATETIME, modified_by VARCHAR(191), doc_status INT NOT NULL DEFAULT 0)`,
		`INSERT INTO _kora_extension (name, api_permissions, site, access_token_hash, is_active) VALUES ('reader', '[{"doctype":"Task","read":true,"if_owner":true}]', 'tenant.example', '4c5dc9b7708905f77f5e5d16316b5dfb425e68cb326dcd55a860e90a7707031e', 1)`,
		`INSERT INTO tabTask (title, name, owner) VALUES ('mine', 'TASK-1', 'extension:reader'), ('private', 'TASK-2', 'another-user')`,
	} {
		if _, err := database.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("prepare disposable MCP fixture: %v", err)
		}
	}

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", SortField: "name", SortOrder: "ASC", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}})
	executor, err := NewExtensionReadExecutor(database, kdb.Resolve("mysql"), registry, contract.TenantID("stable-tenant"), "tenant.example", "test-token")
	if err != nil {
		t.Fatal("create MCP read executor:", err)
	}
	result, err := executor(context.Background(), ai.ToolDescriptor{
		ID: "tenant.task_list", Source: "tenant", Name: "task_list", Operation: "list", Doctype: "Task",
	}, map[string]any{"limit": 10})
	if err != nil {
		t.Fatal("execute authorized tenant read:", err)
	}
	if !strings.Contains(result, "mine") || strings.Contains(result, "private") || strings.Contains(result, "TASK-2") {
		t.Fatalf("owner-scoped read result leaked/omitted records: %s", result)
	}

	result, err = executor(context.Background(), ai.ToolDescriptor{
		ID: "tenant.task_find", Source: "tenant", Name: "task_find", Operation: "find", Doctype: "Task",
	}, map[string]any{
		"filters": []any{map[string]any{"field": "title", "op": "=", "value": "mine"}},
		"limit":   10,
	})
	if err != nil {
		t.Fatal("execute filtered tenant read:", err)
	}
	if !strings.Contains(result, "mine") || strings.Contains(result, "private") {
		t.Fatalf("filtered owner-scoped read leaked/omitted records: %s", result)
	}
}

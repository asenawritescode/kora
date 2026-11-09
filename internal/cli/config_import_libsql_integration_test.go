//go:build integration

package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLibSQLConfigImportUsesRemoteTransportAndReusesOperation(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_LIBSQL_DSN to a disposable loopback LibSQL HTTP endpoint")
	}
	t.Setenv("KORA_DB_TYPE", "libsql")
	t.Setenv("KORA_DB_USER", "")
	t.Setenv("KORA_DB_PASSWORD", "")
	t.Setenv("DB_DSN", dsn)
	pack := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pack, "doctypes"), 0755); err != nil {
		t.Fatal(err)
	}
	config := "name: Task\nmodule: Test\nfields:\n  - fieldname: title\n    fieldtype: Data\n    label: Title\n"
	if err := os.WriteFile(filepath.Join(pack, "doctypes", "task.yaml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := runConfigImport(context.Background(), "libsql-import-test.example", pack, "libsql_import_test"); err != nil {
			t.Fatalf("config import attempt %d through remote LibSQL transport: %v", attempt+1, err)
		}
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable remote LibSQL endpoint")
	}
	defer database.Close()
	var operationCount, activeVersionCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_config_operation WHERE site = ? AND kind = 'import'", "libsql-import-test.example").Scan(&operationCount); err != nil {
		t.Fatal("read LibSQL import journal:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND status = 'Active'", "libsql-import-test.example").Scan(&activeVersionCount); err != nil {
		t.Fatal("count LibSQL imported versions:", err)
	}
	if operationCount != 1 || activeVersionCount != 1 {
		t.Fatalf("LibSQL journal/version counts = %d/%d, want 1/1", operationCount, activeVersionCount)
	}
}

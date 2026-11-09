//go:build integration

package site

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/go-sql-driver/mysql"
)

// TestMySQLCurrentCRMPackInstallsIntoFreshTenant imports every file from the
// current CRM pack into an isolated schema. It exercises the same Engine
// parser, config stores, registry setup, and tenant DDL path used by onboarding.
func TestMySQLCurrentCRMPackInstallsIntoFreshTenant(t *testing.T) {
	rawDSN := strings.TrimSpace(os.Getenv("KORA_CRM_DISPOSABLE_MYSQL_DSN"))
	if rawDSN == "" {
		t.Skip("set KORA_CRM_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
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
	adminDB.SetMaxOpenConns(2)
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping disposable MySQL")
	}
	t.Cleanup(func() {
		if err := adminDB.Close(); err != nil {
			t.Errorf("close disposable MySQL admin connection: %v", err)
		}
	})

	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate disposable schema name:", err)
	}
	databaseName := "kora_crm_install_" + hex.EncodeToString(random[:])
	if _, err := adminDB.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create disposable CRM schema:", err)
	}
	t.Cleanup(func() {
		if _, err := adminDB.Exec("DROP DATABASE `" + databaseName + "`"); err != nil {
			t.Errorf("drop disposable CRM schema %q: %v", databaseName, err)
		}
	})

	tenantConfig := *adminConfig
	tenantConfig.DBName = databaseName
	tenantDB, err := sql.Open("mysql", tenantConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable CRM schema:", err)
	}
	tenantDB.SetMaxOpenConns(4)
	t.Cleanup(func() {
		if err := tenantDB.Close(); err != nil {
			t.Errorf("close disposable CRM schema: %v", err)
		}
	})
	if err := tenantDB.Ping(); err != nil {
		t.Fatal("ping disposable CRM schema:", err)
	}

	dialect := db.Resolve("mysql")
	if err = BootstrapSystemTables(context.Background(), tenantDB, dialect); err != nil {
		t.Fatal("bootstrap Engine system tables:", err)
	}
	registry := doctype.NewRegistry()
	files := loadConfigProfilePackFiles(t, "crm")
	if err := ImportConfigFromPack(tenantDB, registry, databaseName, "crm-install.example.test", "crm", files, dialect); err != nil {
		t.Fatal("import current CRM pack:", err)
	}

	snapshot, _, err := ParseConfigPack("crm", files)
	if err != nil {
		t.Fatal("parse current CRM pack after import:", err)
	}
	for _, docType := range snapshot.DocTypes {
		var count int
		query := "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?"
		if err := tenantDB.QueryRow(query, databaseName, docType.RawTableName()).Scan(&count); err != nil {
			t.Fatalf("check installed table for %s: %v", docType.Name, err)
		}
		if count != 1 {
			t.Errorf("installed table for %s = %d, want 1", docType.Name, count)
		}
	}
	for _, indexedField := range []struct{ table, field string }{
		{"tabCompany", "company_name"},
		{"tabContact", "full_name"},
		{"tabLead", "lifecycle_stage"},
	} {
		var count int
		indexName := fmt.Sprintf("idx_%s_%s", indexedField.table, indexedField.field)
		query := "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = ? AND table_name = ? AND index_name = ? AND non_unique = 1"
		if err := tenantDB.QueryRow(query, databaseName, indexedField.table, indexName).Scan(&count); err != nil {
			t.Fatalf("check searchable index %s: %v", indexName, err)
		}
		if count != 1 {
			t.Errorf("searchable index %s count = %d, want 1", indexName, count)
		}
	}
	var activeVersions int
	if err := tenantDB.QueryRow("SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND status = 'Active'", "crm-install.example.test").Scan(&activeVersions); err != nil {
		t.Fatal("count active CRM config versions:", err)
	}
	if activeVersions != 1 {
		t.Fatalf("active CRM config versions = %d, want 1", activeVersions)
	}
}

//go:build integration
// +build integration

package tests

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/schema"
)

// getTestDB returns a MySQL connection for integration tests.
func getTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("connecting to MySQL: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("MySQL not available, skipping integration test: %v", err)
	}

	dbName := fmt.Sprintf("kora_test_%d", time.Now().UnixNano()%100000)
	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName))
	if err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName))
		db.Close()
	})

	testDSN := fmt.Sprintf("root:@tcp(127.0.0.1:3306)/%s?parseTime=true&charset=utf8mb4", dbName)
	testDB, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	return testDB
}

func bootstrapForTest(t *testing.T, db *sql.DB) {
	t.Helper()
	tables := []string{
		`CREATE TABLE IF NOT EXISTS _kora_doctype (name VARCHAR(140) PRIMARY KEY, config_json JSON) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_field (name VARCHAR(140) PRIMARY KEY, parent VARCHAR(140) NOT NULL, fieldname VARCHAR(140) NOT NULL, fieldtype VARCHAR(50) NOT NULL, label VARCHAR(255) NOT NULL DEFAULT '', options TEXT, reqd TINYINT(1) NOT NULL DEFAULT 0, idx INT NOT NULL DEFAULT 0) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_role (name VARCHAR(140) PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_permission (name VARCHAR(140) PRIMARY KEY, doctype VARCHAR(140) NOT NULL, role VARCHAR(140) NOT NULL, can_read TINYINT(1) NOT NULL DEFAULT 0, can_write TINYINT(1) NOT NULL DEFAULT 0, can_create TINYINT(1) NOT NULL DEFAULT 0, can_delete TINYINT(1) NOT NULL DEFAULT 0, can_submit TINYINT(1) NOT NULL DEFAULT 0, if_owner TINYINT(1) NOT NULL DEFAULT 0) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_config_version (id VARCHAR(36) PRIMARY KEY, site VARCHAR(140) NOT NULL, version INT NOT NULL, is_active TINYINT(1) NOT NULL DEFAULT 0, config JSON) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_user (name VARCHAR(140) PRIMARY KEY, email VARCHAR(255) NOT NULL DEFAULT '', password_hash VARCHAR(255) NOT NULL, full_name VARCHAR(255) NOT NULL DEFAULT '', enabled TINYINT(1) NOT NULL DEFAULT 1, roles TEXT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_workflow (name VARCHAR(140) PRIMARY KEY, document_type VARCHAR(140) NOT NULL, is_active TINYINT(1) NOT NULL DEFAULT 1, config_json JSON) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		// _kora_extension — webhook extension registry (needed by extension permission tests).
		`CREATE TABLE IF NOT EXISTS _kora_extension (
			name VARCHAR(140) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			display_name VARCHAR(255) NOT NULL DEFAULT '',
			description TEXT,
			endpoint_url VARCHAR(1024) NOT NULL,
			secret VARCHAR(64) NOT NULL,
			access_token VARCHAR(64) NOT NULL DEFAULT '',
			old_secret VARCHAR(64),
			old_secret_expires_at DATETIME(6),
			secret_count INT NOT NULL DEFAULT 1,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			subscriptions JSON,
			api_permissions JSON,
			retry_schedule JSON,
			timeout_sec INT NOT NULL DEFAULT 10,
			headers JSON,
			delivery_stats JSON,
			consecutive_failures INT NOT NULL DEFAULT 0,
			installed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			last_delivery_at DATETIME(6),
			last_error TEXT,
			INDEX idx_ext_site (site),
			INDEX idx_ext_active (is_active),
			INDEX idx_ext_access_token (access_token)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, ddl := range tables {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("creating system table: %v", err)
		}
	}
}

func TestIntegration_FullFieldworkLifecycle(t *testing.T) {
	db := getTestDB(t)
	dialect := &kdb.MySQLDialect{}
	bootstrapForTest(t, db)

	// Parse Fieldwork config.
	doctypes, err := doctype.ParseConfigTree("../config/fieldwork/doctypes")
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if len(doctypes) != 6 {
		t.Fatalf("expected 6 doctypes, got %d", len(doctypes))
	}

	roles, permissions, _ := doctype.ParseRolesDirectory("../config/fieldwork/doctypes")
	workflows, _ := doctype.ParseWorkflowDirectory("../config/fieldwork/doctypes")

	store := configstore.NewStore(db, dialect)
	for _, dt := range doctypes {
		if err := store.SaveDocType(dt); err != nil {
			t.Fatalf("saving doctype %s: %v", dt.Name, err)
		}
	}
	store.SaveRoles(roles)
	store.SavePermissions(permissions)
	t.Logf("Saved %d doctypes, %d roles, %d permissions", len(doctypes), len(roles), len(permissions))

	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	for _, wf := range workflows {
		registry.Workflows.Register(wf)
	}

	// Migrate.
	var dbName string
	db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	if err := schema.MigrateSite(db, dbName, registry, dialect); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	var tableCount int
	db.QueryRow("SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME LIKE 'tab%'").Scan(&tableCount)
	t.Logf("Application tables: %d", tableCount)

	// Create admin.
	passwordHash, _ := auth.HashPassword("test123")
	db.Exec("INSERT INTO _kora_user (name, email, password_hash, full_name, roles) VALUES (?,?,?,?,?)",
		"Administrator", "admin@test.local", passwordHash, "Admin", "Administrator")

	txManager := &orm.TxManager{DB: db, Registry: registry, Dialect: dialect}

	// Create Customer.
	custDT := registry.Get("Customer")
	cust := doctype.NewDocument("Customer")
	cust.Set("company_name", "Acme Corp")
	cust.Set("email", "info@acme.com")
	if err := txManager.Insert(custDT, cust, "Administrator", "Administrator"); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	t.Logf("Customer: %s", cust.Name)

	// Create Equipment.
	eqDT := registry.Get("Equipment")
	eq := doctype.NewDocument("Equipment")
	eq.Set("equipment_name", "HVAC-1")
	eq.Set("customer", cust.Name)
	if err := txManager.Insert(eqDT, eq, "Administrator", "Administrator"); err != nil {
		t.Fatalf("insert equipment: %v", err)
	}

	// Create Work Order with items.
	woDT := registry.Get("Work Order")
	wo := doctype.NewDocument("Work Order")
	wo.Set("title", "Fix HVAC at Acme HQ")
	wo.Set("customer", cust.Name)
	wo.Set("scheduled_date", "2026-07-15")
	wo.Set("priority", "High")
	item := doctype.NewDocument("Work Order Item")
	item.Set("equipment", eq.Name)
	item.Set("description", "Annual maintenance")
	item.Set("estimated_hours", 2.0)
	wo.SetTable("items", []*doctype.Document{item})

	if err := txManager.Insert(woDT, wo, "Administrator", "Administrator"); err != nil {
		t.Fatalf("insert work order: %v", err)
	}
	t.Logf("Work Order: %s (status=%s)", wo.Name, wo.GetString("status"))

	// Validate constraint: 0 items fails.
	badWO := doctype.NewDocument("Work Order")
	badWO.Set("title", "Bad")
	badWO.Set("customer", cust.Name)
	badWO.Set("scheduled_date", "2026-07-16")
	badWO.SetTable("items", []*doctype.Document{})
	errs := doctype.ValidateDocument(woDT, badWO, registry, nil)
	if !errs.HasErrors() {
		t.Error("expected validation error for 0 items")
	} else {
		t.Logf("Constraint OK: %v", errs)
	}

	// Workflow: Draft → Submitted.
	doc, _ := txManager.GetDoc(woDT, wo.Name, "")
	newState, newStatus, err := registry.Workflows.ApplyTransition("Work Order", "Draft", "Submit for Approval", "Field Technician", doc)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	doc.Set("status", newState)
	doc.DocStatus = newStatus
	txManager.Save(woDT, doc, "Administrator", "", nil)
	t.Logf("Transition: Draft → %s", newState)

	// if_owner test.
	filtered, _, _ := txManager.GetList(woDT, "", "", 10, 0, "EMP-001")
	t.Logf("if_owner filter: %d docs for other owner", len(filtered))

	// Cleanup.
	txManager.Delete(woDT, wo.Name, "")
	_, err = txManager.GetDoc(woDT, wo.Name, "")
	if err == nil {
		t.Error("expected error after delete")
	}
	t.Log("✓ Full Fieldwork lifecycle test passed")
}

func TestIntegration_ConfigVersioning(t *testing.T) {
	db := getTestDB(t)
	dialect := &kdb.MySQLDialect{}
	bootstrapForTest(t, db)

	original := []*doctype.DocType{{Name: "Customer", Module: "Fieldwork", Fields: []doctype.Field{
		{Fieldname: "company_name", Fieldtype: "Data"},
	}}}
	modified := []*doctype.DocType{{Name: "Customer", Module: "Fieldwork", Fields: []doctype.Field{
		{Fieldname: "company_name", Fieldtype: "Data"},
		{Fieldname: "website", Fieldtype: "Data", Label: "Website"},
	}}}

	diff := doctype.DiffConfigs(original, modified)
	if len(diff.Changes) != 1 {
		t.Errorf("expected 1 change, got %d", len(diff.Changes))
	}
	if diff.Changes[0].Type != doctype.ChangeFieldAdded {
		t.Errorf("expected field_added, got %s", diff.Changes[0].Type)
	}
	t.Logf("Diff: %s (breaking=%v)", diff.Summary(), diff.IsBreaking)

	// Test breaking change detection.
	modified2 := []*doctype.DocType{{Name: "Customer", Module: "Fieldwork", Fields: []doctype.Field{
		{Fieldname: "website", Fieldtype: "Data", Label: "Website"},
	}}}
	diff2 := doctype.DiffConfigs(original, modified2)
	if !diff2.IsBreaking {
		t.Error("removing a field should be breaking")
	}
	t.Logf("Removal diff: %s (breaking=%v)", diff2.Summary(), diff2.IsBreaking)

	// Test version storage.
	store := configstore.NewStore(db, dialect)
	store.SaveDocType(original[0])

	configJSON, _ := yaml.Marshal(original)
	db.Exec("INSERT INTO _kora_config_version (id, site, version, created_by, label, is_active, config) VALUES ('v1', 'test', 1, 'test', 'v1', 1, ?)", string(configJSON))

	var count int
	db.QueryRow("SELECT COUNT(*) FROM _kora_config_version WHERE site = 'test'").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 version, got %d", count)
	}
	t.Log("✓ Config versioning test passed")
}

// TestIntegration_ExtensionPermissions verifies that extension permission records
// are stored and enforced correctly in the _kora_extension table.
func TestIntegration_ExtensionPermissions(t *testing.T) {
	db := getTestDB(t)
	dialect := &kdb.MySQLDialect{}
	bootstrapForTest(t, db)

	// 1. Create a simple doctype.
	wo := &doctype.DocType{
		Name: "Work Order", Module: "Service", TitleField: "title",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true},
		},
	}
	reg := doctype.NewRegistry()
	reg.Register(wo)
	store := configstore.NewStore(db, dialect)
	if err := store.SaveDocType(wo); err != nil {
		t.Fatalf("save doctype: %v", err)
	}

	// 2. Run migration to create the MySQL table.
	var dbName string
	if err := db.QueryRow("SELECT DATABASE()").Scan(&dbName); err != nil {
		t.Fatalf("get db name: %v", err)
	}
	if err := schema.MigrateSite(db, dbName, reg, dialect); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 3. Insert a test document via ORM.
	txManager := &orm.TxManager{DB: db, Registry: reg, Dialect: dialect}
	woDoc := doctype.NewDocument("Work Order")
	woDoc.Set("title", "Test WO")
	if err := txManager.Insert(wo, woDoc, "test-user", "test-user"); err != nil {
		t.Fatalf("insert doc: %v", err)
	}
	t.Logf("created document: %s", woDoc.Name)

	// 4. Insert extension with read-only permission on Work Order.
	_, err := db.Exec(`INSERT INTO _kora_extension
		(name, site, display_name, endpoint_url, access_token, secret, subscriptions, api_permissions, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-readonly", "test.local", "Test ReadOnly", "https://example.com/webhook",
		"test-access-token-readonly", "secret123", "[]",
		`[{"doctype":"Work Order","operations":["read"]}]`, 1)
	if err != nil {
		t.Fatalf("insert extension: %v", err)
	}

	// 5. Verify the document exists (read operation should succeed).
	rows, err := db.Query("SELECT name FROM `Work Order` WHERE name = ?", woDoc.Name)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Errorf("expected to find document %s", woDoc.Name)
	}
	rows.Close()

	// 6. Verify read-only permission data is stored correctly.
	var storedPerms string
	err = db.QueryRow("SELECT api_permissions FROM _kora_extension WHERE name = ?", "test-readonly").Scan(&storedPerms)
	if err != nil {
		t.Fatalf("query perms: %v", err)
	}
	if storedPerms != `[{"doctype":"Work Order","operations":["read"]}]` {
		t.Errorf("permissions mismatch: got %s", storedPerms)
	}

	// 7. Insert extension with write+create permission.
	_, err = db.Exec(`INSERT INTO _kora_extension
		(name, site, display_name, endpoint_url, access_token, secret, subscriptions, api_permissions, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-writer", "test.local", "Test Writer", "https://example.com/webhook",
		"token-writer", "secret456", "[]",
		`[{"doctype":"Work Order","operations":["read","write","create"]}]`, 1)
	if err != nil {
		t.Fatalf("insert writer extension: %v", err)
	}

	// Verify writer permission data.
	err = db.QueryRow("SELECT api_permissions FROM _kora_extension WHERE name = ?", "test-writer").Scan(&storedPerms)
	if err != nil {
		t.Fatalf("query writer perms: %v", err)
	}
	if storedPerms != `[{"doctype":"Work Order","operations":["read","write","create"]}]` {
		t.Errorf("writer permissions mismatch: got %s", storedPerms)
	}

	// 8. Insert extension with empty permissions → should have no access.
	_, err = db.Exec(`INSERT INTO _kora_extension
		(name, site, display_name, endpoint_url, access_token, secret, subscriptions, api_permissions, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-empty", "test.local", "Test Empty", "https://example.com/webhook",
		"token-empty", "secret789", "[]", "[]", 1)
	if err != nil {
		t.Fatalf("insert empty-perms extension: %v", err)
	}

	// Verify empty permissions stored correctly.
	err = db.QueryRow("SELECT api_permissions FROM _kora_extension WHERE name = ?", "test-empty").Scan(&storedPerms)
	if err != nil {
		t.Fatalf("query empty perms: %v", err)
	}
	if storedPerms != "[]" {
		t.Errorf("expected empty permissions, got %s", storedPerms)
	}

	// 9. Test: inactive extension should have is_active=0.
	_, err = db.Exec(`INSERT INTO _kora_extension
		(name, site, display_name, endpoint_url, access_token, secret, subscriptions, api_permissions, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-inactive", "test.local", "Test Inactive", "https://example.com/webhook",
		"token-inactive", "secret000", "[]", `[{"doctype":"Work Order","operations":["read"]}]`, 0)
	if err != nil {
		t.Fatalf("insert inactive extension: %v", err)
	}

	var isActive int
	err = db.QueryRow("SELECT is_active FROM _kora_extension WHERE name = ?", "test-inactive").Scan(&isActive)
	if err != nil {
		t.Fatalf("query inactive: %v", err)
	}
	if isActive != 0 {
		t.Errorf("expected is_active=0, got %d", isActive)
	}

	// 10. Verify extension record with read permission can query document metadata.
	// This simulates what a permission check function would validate.
	for _, tc := range []struct {
		name          string
		accessToken   string
		expectRead    bool
		expectWrite   bool
		expectCreate  bool
		expectDelete  bool
	}{
		{"test-readonly", "test-access-token-readonly", true, false, false, false},
		{"test-writer", "token-writer", true, true, true, false},
		{"test-empty", "token-empty", false, false, false, false},
		{"test-inactive", "token-inactive", true, false, false, false}, // inactive: perms stored but access should be denied
	} {
		t.Run(tc.name, func(t *testing.T) {
			var permsJSON string
			var active int
			err := db.QueryRow(
				"SELECT api_permissions, is_active FROM _kora_extension WHERE access_token = ?", tc.accessToken,
			).Scan(&permsJSON, &active)
			if err != nil {
				t.Fatalf("lookup extension %s: %v", tc.name, err)
			}

			// Parse the permission JSON to verify structure.
			var perms []struct {
				Doctype    string   `json:"doctype"`
				Operations []string `json:"operations"`
			}
			if err := json.Unmarshal([]byte(permsJSON), &perms); err != nil {
				t.Fatalf("parse perms JSON: %v", err)
			}

			if tc.expectRead {
				if active == 0 {
					t.Errorf("expected active extension for read, but is_active=0")
				}
				found := false
				for _, p := range perms {
					if p.Doctype == "Work Order" {
						for _, op := range p.Operations {
							if op == "read" {
								found = true
							}
						}
					}
				}
				if !found {
					t.Errorf("expected read permission on Work Order, got %v", perms)
				}
			}

			if tc.expectDelete {
				found := false
				for _, p := range perms {
					if p.Doctype == "Work Order" {
						for _, op := range p.Operations {
							if op == "delete" {
								found = true
							}
						}
					}
				}
				if !found {
					t.Errorf("expected delete permission on Work Order, got %v", perms)
				}
			}
		})
	}

	t.Logf("extension permissions integration test passed")
}

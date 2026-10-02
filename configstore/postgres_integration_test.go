package configstore_test

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/site"
	_ "github.com/lib/pq"
)

// TestLivePostgresConfigStoreLifecycle runs representative config-store
// reads and writes against PostgreSQL. Set KORA_API_LIVE_POSTGRES_DSN to a
// disposable database; the test creates and drops an isolated schema.
func TestLivePostgresConfigStoreLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test config storage against disposable PostgreSQL")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_configstore_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	dialect := kdb.Resolve("postgres")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap PostgreSQL system tables:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_analytics_metric (
		id BIGSERIAL PRIMARY KEY, site VARCHAR(140) NOT NULL,
		name VARCHAR(140) NOT NULL, label VARCHAR(255) NOT NULL DEFAULT '',
		type VARCHAR(50) NOT NULL, doctype VARCHAR(140) NOT NULL,
		field_name VARCHAR(140) NOT NULL DEFAULT '', link_field VARCHAR(140) NOT NULL DEFAULT '',
		group_by_field VARCHAR(140) NOT NULL DEFAULT '', UNIQUE (site, name)
	)`); err != nil {
		t.Fatal("create analytics metric fixture:", err)
	}
	store := configstore.NewStore(database, dialect)

	dt := &doctype.DocType{
		Name: "Inventory Item", Module: "Stock", Description: "Tracked inventory",
		Fields: []doctype.Field{{Fieldname: "sku", Fieldtype: "Data", Label: "SKU", Reqd: true}},
	}
	if err := store.SaveDocType(dt, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL doctype:", err)
	}
	loadedTypes, err := store.LoadAll("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL doctypes:", err)
	}
	if len(loadedTypes) != 1 || loadedTypes[0].Name != dt.Name || len(loadedTypes[0].Fields) != 1 || loadedTypes[0].Fields[0].Fieldname != "sku" {
		t.Fatalf("loaded doctype config = %#v, want Inventory Item with sku field", loadedTypes)
	}

	roles := []*doctype.Role{{Name: "Stock Manager", WorkspaceAccess: true, Description: "Manages stock"}}
	if err := store.SaveRoles(roles, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL roles:", err)
	}
	loadedRoles, err := store.LoadRoles("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL roles:", err)
	}
	roleFound := false
	for _, role := range loadedRoles {
		if role.Name == roles[0].Name {
			roleFound = role.WorkspaceAccess
		}
	}
	if !roleFound {
		t.Fatalf("loaded roles = %#v, want Stock Manager with workspace access", loadedRoles)
	}

	permissions := []*doctype.Permission{{Doctype: dt.Name, Role: roles[0].Name, Read: true, Create: true}}
	if err := store.SavePermissions(permissions, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL permissions:", err)
	}
	loadedPermissions, err := store.LoadPermissions("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL permissions:", err)
	}
	if len(loadedPermissions) != 1 || loadedPermissions[0].Doctype != dt.Name || !loadedPermissions[0].Read || !loadedPermissions[0].Create {
		t.Fatalf("loaded permissions = %#v, want read/create access for Inventory Item", loadedPermissions)
	}

	workflow := &doctype.Workflow{Name: "Inventory Review", DocumentType: dt.Name, IsActive: true}
	if err := store.SaveWorkflows([]*doctype.Workflow{workflow}, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL workflow:", err)
	}
	loadedWorkflows, err := store.LoadWorkflows("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL workflows:", err)
	}
	if len(loadedWorkflows) != 1 || loadedWorkflows[0].Name != workflow.Name {
		t.Fatalf("loaded workflows = %#v, want Inventory Review", loadedWorkflows)
	}

	view := &doctype.View{Name: "Inventory", Route: "/inventory", Type: "list", SourceDocType: dt.Name}
	if err := store.SaveViews([]*doctype.View{view}, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL views:", err)
	}
	loadedViews, err := store.LoadViews("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL views:", err)
	}
	if len(loadedViews) != 1 || loadedViews[0].Name != view.Name || loadedViews[0].Route != view.Route {
		t.Fatalf("loaded views = %#v, want Inventory at /inventory", loadedViews)
	}

	metric := &doctype.AnalyticsMetricConfig{Name: "stock_count", Label: "Stock Count", Type: "count", DocType: dt.Name}
	if err := store.SaveAnalyticsMetrics([]*doctype.AnalyticsMetricConfig{metric}, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL analytics metric:", err)
	}
	loadedMetrics, err := store.LoadAnalyticsMetrics("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL analytics metrics:", err)
	}
	if len(loadedMetrics) != 1 || loadedMetrics[0].Name != metric.Name {
		t.Fatalf("loaded analytics metrics = %#v, want stock_count", loadedMetrics)
	}

	script := &doctype.ScriptSnapshot{Name: "inventory_check", ScriptType: "api_method", MethodPath: "inventory.check", IsActive: true, Source: "return true"}
	if err := store.SaveScripts([]*doctype.ScriptSnapshot{script}, map[string]string{script.ScriptHash: script.Source}, "tenant.example.test"); err != nil {
		t.Fatal("save PostgreSQL script:", err)
	}
	loadedScripts, err := store.LoadScriptSnapshots("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL scripts:", err)
	}
	if len(loadedScripts) != 1 || loadedScripts[0].Name != script.Name || loadedScripts[0].Source != script.Source {
		t.Fatalf("loaded scripts = %#v, want inventory_check source", loadedScripts)
	}

	snapshot, err := store.CollectSnapshot(doctype.NewRegistry(), "tenant.example.test")
	if err != nil {
		t.Fatal("collect PostgreSQL config snapshot:", err)
	}
	versionID, _, err := store.CreateConfigVersion("tenant.example.test", "tester", "Inventory draft", "Draft", snapshot)
	if err != nil {
		t.Fatal("create PostgreSQL draft version:", err)
	}
	loadedSnapshot, gotID, err := store.LoadDraftHeadSnapshot("tenant.example.test")
	if err != nil {
		t.Fatal("load PostgreSQL draft head:", err)
	}
	if gotID != versionID || loadedSnapshot == nil {
		t.Fatalf("draft head = (%q, %#v), want %q and non-nil snapshot", gotID, loadedSnapshot, versionID)
	}
}

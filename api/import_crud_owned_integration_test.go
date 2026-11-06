//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
)

// TestImportIntoCrudOwnedItemWithLinkNames loads the shipped demo catalogue
// into the inventory pack's module-owned (crud) Item on MySQL. Set
// KORA_API_LIVE_MYSQL_DSN to a disposable server.
func TestImportIntoCrudOwnedItemWithLinkNames(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_MYSQL_DSN")
	}
	database, dialect := openKernelDialectDatabase(t, "mysql", dsn)
	if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	site := fmt.Sprintf("crud-import-%x", time.Now().UnixNano())
	item, err := doctype.ParseFile("../config/inventory/doctypes/item.yaml")
	if err != nil {
		t.Fatal(err)
	}
	category, err := doctype.ParseFile("../config/inventory/doctypes/item_category.yaml")
	if err != nil {
		t.Fatal(err)
	}
	locked := &doctype.DocType{Name: "Locked Sale", ModuleOwnerID: "inventory.core", ModuleOwnershipMode: string(contract.MutationCommands), Fields: []doctype.Field{{Fieldname: "ref", Fieldtype: "Data"}}}
	// The Item and Item Category grants are the shipped packs' (#397), not
	// hand-written ones: Inventory Manager imports through config/inventory/permissions.yaml.
	var shipped []*doctype.Permission
	for _, file := range []string{"../config/inventory/permissions.yaml", "../config/pos/permissions.yaml"} {
		perms, err := doctype.ParsePermissionsFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range perms {
			if p.Doctype == "Item" || p.Doctype == "Item Category" {
				shipped = append(shipped, p)
			}
		}
	}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{item, category, locked}, []*doctype.Role{{Name: "Inventory Manager"}, {Name: "Cashier"}, {Name: "Manager"}, {Name: "Owner"}},
		append(shipped, &doctype.Permission{Doctype: "Locked Sale", Role: "Inventory Manager", Read: true, Write: true, Create: true, Import: true}))
	cleanupKernelDialectRecords(t, database, dialect, site, "Item", "Item Category", "Locked Sale")
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := configstore.NewModuleStore(database, dialect)
	if err := store.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	snap, _ := json.Marshal(contract.ModuleSnapshot{TenantID: contract.TenantID(site), Revision: 1, Installations: []contract.ModuleInstallation{
		{TenantID: contract.TenantID(site), ModuleID: "inventory.core", State: string(contract.ModuleActive), Revision: 1}}})
	if _, err := database.Exec(kdb.Rebind(dialect, "INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)"), site, 1, string(snap)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM _kora_module_snapshot WHERE tenant_id = ?"), site)
	})
	writer := outbox.NewSQLWriter(dialect)
	h := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
	h.SiteOutboxes = map[string]outbox.Writer{site: writer}

	post := func(role string, spec map[string]any, csv string) (int, map[string]any) {
		t.Helper()
		sb, _ := json.Marshal(spec)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("spec", string(sb))
		fw, _ := mw.CreateFormFile("file", "x.csv")
		_, _ = fw.Write([]byte(csv))
		_ = mw.Close()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/import", &buf)
		c.Request.Header.Set("Content-Type", mw.FormDataContentType())
		c.Set("site_name", site)
		c.Set("site_db", database)
		c.Set("site_registry", reg)
		c.Set("site_db_type", "mysql")
		c.Set("user", "u@example.test")
		c.Set("user_roles", []string{role})
		c.Set("policy_execution", integrationTestExecution(t, site, "u@example.test", []string{role}))
		h.HandleImport(c)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	count := func(table string) int {
		return countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(table))
	}
	catalogue, categories := read("../config/pos/demo_catalogue.csv"), read("../config/pos/demo_categories.csv")
	itemSpec := func(id string, extra map[string]any) map[string]any {
		spec := map[string]any{"import_id": id + site, "target": map[string]any{"doctype": "Item"},
			"mapping": map[string]string{"item_name": "item_name", "barcode": "barcode", "uom": "uom", "price": "price", "cost": "cost", "category": "category"}}
		for k, v := range extra {
			spec[k] = v
		}
		return spec
	}

	catSpec := map[string]any{"import_id": "cats" + site, "target": map[string]any{"doctype": "Item Category"}, "mapping": map[string]string{"category_name": "category_name"}}
	for _, role := range []string{"Cashier", "Manager"} {
		if status, body := post(role, itemSpec("deny"+role, nil), catalogue); status != http.StatusForbidden {
			t.Fatalf("%s importing items: %d %v", role, status, body)
		}
		if status, body := post(role, catSpec, categories); status != http.StatusForbidden {
			t.Fatalf("%s importing categories: %d %v", role, status, body)
		}
	}
	if count("tabItem") != 0 || count("tabItem Category") != 0 {
		t.Fatal("a refused import wrote rows")
	}
	if status, body := post("Inventory Manager", map[string]any{"import_id": "locked", "target": map[string]any{"doctype": "Locked Sale"}, "mapping": map[string]string{"ref": "ref"}}, "ref\na\n"); status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body), "import.target_owned") {
		t.Fatalf("commands_only target: %d %v", status, body)
	}
	// Categories do not exist yet: every row fails clearly and nothing is written.
	_, body := post("Inventory Manager", itemSpec("pre", nil), catalogue)
	if body["failed"] != float64(8) || !strings.Contains(fmt.Sprint(body["rows"]), "import.link_unresolved") || count("tabItem") != 0 {
		t.Fatalf("unknown categories: %v items=%d", body, count("tabItem"))
	}
	if _, body = post("Inventory Manager", catSpec, categories); body["succeeded"] != float64(5) {
		t.Fatalf("categories: %v", body)
	}
	_, body = post("Inventory Manager", itemSpec("dry", map[string]any{"dry_run": true}), catalogue)
	if body["succeeded"] != float64(8) || count("tabItem") != 0 {
		t.Fatalf("dry run: %v items=%d", body, count("tabItem"))
	}
	_, body = post("Inventory Manager", itemSpec("run", nil), catalogue)
	if body["succeeded"] != float64(8) || body["failed"] != float64(0) || count("tabItem") != 8 {
		t.Fatalf("import: %v items=%d", body, count("tabItem"))
	}
	var link, owner string
	q := "SELECT i.category, i.owner FROM `tabItem` i WHERE i.barcode = ?"
	if err := database.QueryRow(q, "6001000000011").Scan(&link, &owner); err != nil || owner != "u@example.test" {
		t.Fatalf("item row: %v owner=%q", err, owner)
	}
	var title string
	if err := database.QueryRow("SELECT category_name FROM `tabItem Category` WHERE name = ?", link).Scan(&title); err != nil || title != "Grocery" {
		t.Fatalf("category link %q resolves to %q (%v)", link, title, err)
	}
	// Upsert by barcode updates in place and keeps the resolved link.
	changed := strings.Replace(catalogue, "Maize Flour 2kg,6001000000011,Each,180,150,Grocery", "Maize Flour 2kg,6001000000011,Each,199,150,Dairy", 1)
	_, body = post("Inventory Manager", itemSpec("up", map[string]any{"target": map[string]any{"doctype": "Item", "key_fields": []string{"barcode"}}}), changed)
	var price float64
	_ = database.QueryRow("SELECT price FROM `tabItem` WHERE barcode = ?", "6001000000011").Scan(&price)
	if body["succeeded"] != float64(8) || count("tabItem") != 8 || price != 199 {
		t.Fatalf("upsert: %v items=%d price=%v", body, count("tabItem"), price)
	}
}

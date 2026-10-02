//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// TestMVPPOSCheckoutAgainstPackSchema exercises the checkout action with the
// real MVP DocType and view YAML, rather than simplified integration fixtures.
func TestMVPPOSCheckoutAgainstPackSchema(t *testing.T) {
	docs, err := doctype.ParseConfigTree(filepath.Join("..", "config", "kora-mvp"))
	if err != nil {
		t.Fatalf("parse MVP DocTypes: %v", err)
	}
	reg := doctype.NewRegistry()
	for _, dt := range docs {
		reg.Register(dt)
		reg.Permissions.SetPermission(&doctype.Permission{
			Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Write: true,
			Create: true, Delete: true, Submit: true,
		})
	}
	viewBytes, err := os.ReadFile(filepath.Join("..", "config", "kora-mvp", "views", "point_of_sale.yaml"))
	if err != nil {
		t.Fatal("read MVP POS view:", err)
	}
	var view doctype.View
	if err := yaml.Unmarshal(viewBytes, &view); err != nil {
		t.Fatal("parse MVP POS view:", err)
	}
	reg.Views.Register(&view)

	database, site := newResourceIntegrationDB(t)
	dialect := kdb.Resolve("mysql")
	statements := [][]string{kdb.OutboxTablesMySQL(), kdb.KernelTablesMySQL()}
	for _, statement := range dialect.SystemTableSQL() {
		if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE IF NOT EXISTS") {
			statements = append(statements, []string{statement})
		}
	}
	for _, group := range statements {
		for _, statement := range group {
			if _, err := database.Exec(statement); err != nil {
				t.Fatalf("create platform table: %v", err)
			}
		}
	}
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
		t.Fatalf("migrate MVP schema: %v", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	tx := &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site}
	handler := NewHandler(reg, tx)
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	t.Cleanup(func() { _ = database.Close() })

	// Seed the same product identity used by the supplied browser request.
	now := time.Now()
	if _, err := database.Exec(`INSERT INTO `+dialect.QuoteIdent(reg.Get("Product").RawTableName())+
		` (name, owner, creation, modified, modified_by, doc_status, idx, product_name, item_type, selling_price, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"PROD-0001", "admin@example.test", now, now, "admin@example.test", 0, 1,
		"Cocacola", "Product", 100, "Active"); err != nil {
		t.Fatal("seed POS product:", err)
	}

	gin.SetMode(gin.TestMode)
	requestBody, err := json.Marshal(map[string]any{
		"view": view.Name, "component": "payment", "context": map[string]any{
			"cart": []any{map[string]any{
				"name": "PROD-0001", "product": "PROD-0001", "product_name": "Cocacola",
				"rate": 100, "quantity": 9, "amount": 900,
				"row": map[string]any{"category": "PC-0001", "doc_status": 0, "item_type": "Product",
					"name": "PROD-0001", "product_name": "Cocacola", "selling_price": 100, "status": "Active"},
				"_component": "products",
			}},
			"customer": "", "cashier": "pos-review-admin@kora.local", "register": "", "shift": "",
			"till_session": "", "invoice_date": "2026-10-02", "due_date": "2026-10-02",
			"customer_name": "", "payment_status": "", "payment_method": "Cash", "total": 900,
		},
	})
	if err != nil {
		t.Fatal("encode MVP checkout request:", err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/view/action/complete_sale", strings.NewReader(string(requestBody)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "actionId", Value: "complete_sale"}}
	ctx.Set("site_name", site)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	ctx.Set("user", "pos-review-admin@kora.local")
	ctx.Set("user_role", doctype.AdminRole)
	ctx.Set("user_roles", []string{doctype.AdminRole})
	handler.HandleViewAction(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("MVP checkout returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal("decode checkout response:", err)
	}
	doc, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("checkout response data has type %T", response.Data)
	}
	saleName, _ := doc["name"].(string)
	if saleName == "" || doc["total"] != float64(900) {
		t.Fatalf("checkout response lost sale name or total: %#v", doc)
	}
	var saleCount, itemCount, auditCount, outboxCount int
	checks := []struct {
		query  string
		args   []any
		target *int
	}{
		{"SELECT COUNT(*) FROM `tabSale` WHERE name = ?", []any{saleName}, &saleCount},
		{"SELECT COUNT(*) FROM `tabSale__items` WHERE parent = ?", []any{saleName}, &itemCount},
		{"SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doctype = 'Sale' AND doc_name = ?", []any{site, saleName}, &auditCount},
		{"SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", []any{site, saleName}, &outboxCount},
	}
	for _, check := range checks {
		if err := database.QueryRow(check.query, check.args...).Scan(check.target); err != nil {
			t.Fatalf("verify checkout with %q: %v", check.query, err)
		}
	}
	if saleCount != 1 || itemCount != 1 || auditCount != 1 || outboxCount != 1 {
		t.Fatalf("checkout side effects sale=%d items=%d audits=%d outbox=%d", saleCount, itemCount, auditCount, outboxCount)
	}
}

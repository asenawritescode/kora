//go:build integration

package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
)

// newPOSBaseHandler loads the real config/pos-base pack (record types and the
// register view) into a throwaway MySQL database, so the tests below exercise
// the pack as shipped and not a copy of it.
func newPOSBaseHandler(t *testing.T) (*Handler, *sql.DB, *doctype.Registry, string) {
	t.Helper()
	database, site := newResourceIntegrationDB(t)
	dialect := kdb.Resolve("mysql")
	for _, ddl := range dialect.SystemTableSQL() {
		if strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
			if _, err := database.Exec(ddl); err != nil {
				t.Fatalf("create system tables: %v", err)
			}
		}
	}
	for _, ddl := range kdb.OutboxTablesMySQL() {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create outbox tables: %v", err)
		}
	}
	for _, ddl := range kdb.KernelTablesMySQL() {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create kernel tables: %v", err)
		}
	}
	for _, ddl := range configstore.ConfigRevisionSchema(dialect) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(t.Context(), database, dialect); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}

	pack := filepath.Join("..", "config", "pos-base")
	docTypes, err := doctype.ParseConfigTree(pack)
	if err != nil {
		t.Fatalf("parse pos-base: %v", err)
	}
	views, err := doctype.ParseViewsDirectory(filepath.Join(pack, "views"))
	if err != nil {
		t.Fatalf("parse pos-base views: %v", err)
	}
	permissions := make([]*doctype.Permission, 0, len(docTypes))
	for _, recordType := range docTypes {
		permissions = append(permissions, &doctype.Permission{
			Doctype: recordType.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true, Submit: true,
		})
	}
	reg := doctype.NewRegistry()
	reg.LoadFull(docTypes, []*doctype.Role{{Name: doctype.AdminRole}}, permissions)
	for _, view := range views {
		reg.Views.Register(view)
	}
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
		t.Fatalf("migrate pos-base schema: %v", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	t.Cleanup(func() { _ = database.Close() })
	return handler, database, reg, site
}

func seedPOSBaseShop(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		"INSERT INTO `tabRegister` (name, reference, register_name, status) VALUES ('REG-001','REG-001','Main till','Active')",
		"INSERT INTO `tabProduct` (name, reference, product_name, selling_price, stock_qty, status) VALUES ('PROD-001','P-1','Soda',100,1000,'Active')",
		"INSERT INTO `tabCurrency` (name, code, currency_name, is_base, rate_to_base, enabled) VALUES ('KES','KES','Base money',1,1,1)",
		"INSERT INTO `tabCurrency` (name, code, currency_name, is_base, rate_to_base, enabled) VALUES ('USD','USD','Dollar',0,129.5,1)",
		"INSERT INTO `tabCurrency` (name, code, currency_name, is_base, rate_to_base, enabled) VALUES ('UGX','UGX','Small rate money',0,0.0345,1)",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

// What the register panel sends today when the cashier takes cash.
func posBasePanelCheckout(reference string, total float64, extra map[string]any) map[string]any {
	checkout := map[string]any{
		"reference": reference, "cart": []any{map[string]any{"product": "PROD-001", "rate": 100.0, "quantity": total / 100}},
		"customer": "", "customer_name": "", "invoice_date": "2026-11-08", "due_date": "2026-11-08",
		"register": "REG-001", "payment_status": "Paid", "payment_method": "Cash", "status": "Paid", "total": total,
	}
	for key, value := range extra {
		checkout[key] = value
	}
	return checkout
}

func TestPOSBaseRegisterSaleInBaseCurrencyRecordsSaleAndPaymentAndStock(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)

	result := serveManifestAction(t, handler, site, db, reg, "complete", posBasePanelCheckout("POS-BASE-1", 300, nil))
	if result.Code != http.StatusOK {
		t.Fatalf("register sale status = %d: %s", result.Code, result.Body.String())
	}
	sale := requireActionDocument(t, result.Body.Bytes())
	saleName, _ := sale["name"].(string)

	var total, baseTotal, rate float64
	var currency sql.NullString
	if err := db.QueryRow("SELECT total, base_total, exchange_rate, currency FROM `tabSale` WHERE name = ?", saleName).Scan(&total, &baseTotal, &rate, &currency); err != nil {
		t.Fatal(err)
	}
	if total != 300 || baseTotal != 300 || rate != 1 || currency.String != "" {
		t.Fatalf("base-currency sale: total=%v base_total=%v rate=%v currency=%q, want 300/300/1/empty", total, baseTotal, rate, currency.String)
	}

	var amount, baseAmount float64
	var status, method string
	if err := db.QueryRow("SELECT amount, base_amount, status, method FROM `tabPayment` WHERE sale = ?", saleName).Scan(&amount, &baseAmount, &status, &method); err != nil {
		t.Fatalf("payment row: %v", err)
	}
	if amount != 300 || baseAmount != 300 || status != "Received" || method != "Cash" {
		t.Fatalf("payment: amount=%v base=%v status=%q method=%q, want 300/300/Received/Cash", amount, baseAmount, status, method)
	}

	var stock float64
	if err := db.QueryRow("SELECT stock_qty FROM `tabProduct` WHERE name = 'PROD-001'").Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 997 {
		t.Fatalf("stock after selling 3 = %v, want 997", stock)
	}
}

// A sale made in another currency keeps that currency and a frozen rate, and
// its base total is the sale total times the rate.
func TestPOSBaseRegisterSaleInAnotherCurrencyKeepsFrozenRate(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)

	result := serveManifestAction(t, handler, site, db, reg, "complete",
		posBasePanelCheckout("POS-BASE-USD", 200, map[string]any{"currency": "USD", "exchange_rate": 129.5}))
	if result.Code != http.StatusOK {
		t.Fatalf("foreign-currency sale status = %d: %s", result.Code, result.Body.String())
	}
	saleName, _ := requireActionDocument(t, result.Body.Bytes())["name"].(string)

	var total, baseTotal, rate float64
	var currency string
	if err := db.QueryRow("SELECT total, base_total, exchange_rate, currency FROM `tabSale` WHERE name = ?", saleName).Scan(&total, &baseTotal, &rate, &currency); err != nil {
		t.Fatal(err)
	}
	if currency != "USD" || total != 200 || rate != 129.5 || baseTotal != 25900 {
		t.Fatalf("USD sale: currency=%q total=%v rate=%v base_total=%v, want USD/200/129.5/25900", currency, total, rate, baseTotal)
	}

	// Changing today's rate on the Currency record must not move a past sale.
	if _, err := db.Exec("UPDATE `tabCurrency` SET rate_to_base = 140 WHERE name = 'USD'"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT base_total FROM `tabSale` WHERE name = ?", saleName).Scan(&baseTotal); err != nil {
		t.Fatal(err)
	}
	if baseTotal != 25900 {
		t.Fatalf("a rate change moved a past sale: base_total=%v, want 25900", baseTotal)
	}
}

// Rates below one must survive storage; a shilling-sized currency is worth a
// fraction of a base unit and rounding it to two places would erase it.
func TestPOSBaseKeepsSmallExchangeRates(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)

	result := serveManifestAction(t, handler, site, db, reg, "complete",
		posBasePanelCheckout("POS-BASE-UGX", 10000, map[string]any{"currency": "UGX", "exchange_rate": 0.0345}))
	if result.Code != http.StatusOK {
		t.Fatalf("small-rate sale status = %d: %s", result.Code, result.Body.String())
	}
	saleName, _ := requireActionDocument(t, result.Body.Bytes())["name"].(string)
	var rate, baseTotal float64
	if err := db.QueryRow("SELECT exchange_rate, base_total FROM `tabSale` WHERE name = ?", saleName).Scan(&rate, &baseTotal); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%.4f", rate) != "0.0345" || baseTotal != 345 {
		t.Fatalf("small rate: rate=%v base_total=%v, want 0.0345 and 345", rate, baseTotal)
	}
}

func TestPOSBaseRejectsASaleWithAnImpossibleRate(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)

	result := serveManifestAction(t, handler, site, db, reg, "complete",
		posBasePanelCheckout("POS-BASE-BAD", 100, map[string]any{"currency": "USD", "exchange_rate": 0}))
	if result.Code == http.StatusOK {
		t.Fatalf("a sale with exchange rate 0 was accepted: %s", result.Body.String())
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabSale`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected sale left %d rows behind", count)
	}
}

//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

// These tests run the real packs on MySQL and create records the ordinary way
// (the REST create handler). Stock must change as part of the save, from any
// surface, and never without it.

func effectHarness(t *testing.T) (*Handler, *sql.DB, *doctype.Registry, string) {
	t.Helper()
	clerk := &doctype.Role{Name: "Clerk", WorkspaceAccess: true}
	perms := []*doctype.Permission{
		// A clerk may record adjustments and receipts and read products,
		// but may not edit a product directly.
		{Doctype: "Stock Adjustment", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Goods Receipt", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Goods Receipt Item", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Product", Role: "Clerk", Read: true},
	}
	handler, db, reg, site := newPackLayersHandlerWithRoles(t, []*doctype.Role{clerk}, perms, "pos-base", "erp-purchasing", "erp-inventory")
	for _, statement := range []string{
		"INSERT INTO `tabProduct` (name, reference, product_name, selling_price, stock_qty, status) VALUES ('SODA','P-1','Soda',100,10,'Active')",
		"INSERT INTO `tabProduct` (name, reference, product_name, selling_price, stock_qty, status) VALUES ('WATER','P-2','Water',50,0,'Active')",
		"INSERT INTO `tabSupplier` (name, supplier_name, status) VALUES ('SUP-1','Dar Traders','Active')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return handler, db, reg, site
}

func stockOf(t *testing.T, db *sql.DB, product string) float64 {
	t.Helper()
	var qty float64
	if err := db.QueryRow("SELECT stock_qty FROM `tabProduct` WHERE name = ?", product).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	return qty
}

func countOf(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func post(t *testing.T, h *Handler, db *sql.DB, reg *doctype.Registry, site, recordType string, body any, key string, roles []string) (int, string) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	rec := serveResourceMutationBody(t, h, db, reg, site, httpMethodPost, "/api/resource/"+recordType, string(encoded), key, roles)
	return rec.Code, rec.Body.String()
}

var admin = []string{doctype.AdminRole}

func TestStockAdjustmentChangesStockWhenSavedThroughTheNormalCreate(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	code, body := post(t, h, db, reg, site, "Stock%20Adjustment", map[string]any{"reference": "ADJ-1", "product": "SODA", "delta": 5, "reason": "Found"}, "", admin)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, body)
	}
	if got := stockOf(t, db, "SODA"); got != 15 {
		t.Fatalf("stock = %v, want 15", got)
	}
	var doc struct{ Data map[string]any }
	if err := json.Unmarshal([]byte(body), &doc); err != nil || doc.Data["reference"] != "ADJ-1" {
		t.Fatalf("the response must still be the created adjustment: %s", body)
	}
}

func TestStockAdjustmentCanTakeStockOut(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	if code, body := post(t, h, db, reg, site, "Stock%20Adjustment", map[string]any{"reference": "ADJ-2", "product": "SODA", "delta": -4, "reason": "Damaged"}, "", admin); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, body)
	}
	if got := stockOf(t, db, "SODA"); got != 6 {
		t.Fatalf("stock = %v, want 6", got)
	}
}

func TestAdjustmentBelowZeroIsRefusedAndNothingIsSaved(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	code, body := post(t, h, db, reg, site, "Stock%20Adjustment", map[string]any{"reference": "ADJ-3", "product": "SODA", "delta": -11, "reason": "Lost or stolen"}, "", admin)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, body)
	}
	if got := stockOf(t, db, "SODA"); got != 10 {
		t.Fatalf("stock = %v, want it unchanged at 10", got)
	}
	if n := countOf(t, db, "tabStock Adjustment"); n != 0 {
		t.Fatalf("a refused adjustment left %d record(s) behind", n)
	}
}

func TestRetryWithTheSameKeyDoesNotChangeStockTwice(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	body := map[string]any{"reference": "ADJ-4", "product": "SODA", "delta": 5, "reason": "Found"}
	for i := 0; i < 2; i++ {
		if code, out := post(t, h, db, reg, site, "Stock%20Adjustment", body, "adj-key-4", admin); code != http.StatusCreated {
			t.Fatalf("attempt %d status = %d: %s", i+1, code, out)
		}
	}
	if got := stockOf(t, db, "SODA"); got != 15 {
		t.Fatalf("stock = %v after a retry, want 15 (added once)", got)
	}
	if n := countOf(t, db, "tabStock Adjustment"); n != 1 {
		t.Fatalf("adjustments = %d, want 1", n)
	}
}

func TestSomeoneWhoMayOnlyCreateTheRecordCanStillRaiseStockButNotEditTheProduct(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	clerk := []string{"Clerk"}
	if code, body := post(t, h, db, reg, site, "Stock%20Adjustment", map[string]any{"reference": "ADJ-5", "product": "SODA", "delta": 2, "reason": "Found"}, "", clerk); code != http.StatusCreated {
		t.Fatalf("a clerk who may create adjustments was refused: %d %s", code, body)
	}
	if got := stockOf(t, db, "SODA"); got != 12 {
		t.Fatalf("stock = %v, want 12", got)
	}
	rec := serveResourceMutationBody(t, h, db, reg, site, httpMethodPut, "/api/resource/Product/SODA", `{"selling_price":1}`, "", clerk)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a clerk edited a product directly: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSomeoneWithoutCreatePermissionOnTheRecordCannotChangeStock(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	code, body := post(t, h, db, reg, site, "Stock%20Adjustment", map[string]any{"reference": "ADJ-6", "product": "SODA", "delta": 2, "reason": "Found"}, "", []string{"Nobody"})
	if code == http.StatusCreated {
		t.Fatalf("an unauthorized create changed stock: %s", body)
	}
	if got := stockOf(t, db, "SODA"); got != 10 {
		t.Fatalf("stock = %v, want 10", got)
	}
}

func TestGoodsReceiptRaisesStockForEveryLineInOneSave(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	receipt := map[string]any{"reference": "GR-1", "received_on": "2026-11-08", "supplier": "SUP-1", "items": []map[string]any{
		{"product": "SODA", "quantity": 24, "unit_cost": 70}, {"product": "WATER", "quantity": 12, "unit_cost": 30}, {"product": "SODA", "quantity": 6, "unit_cost": 70},
	}}
	if code, body := post(t, h, db, reg, site, "Goods%20Receipt", receipt, "", []string{"Clerk"}); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, body)
	}
	if soda, water := stockOf(t, db, "SODA"), stockOf(t, db, "WATER"); soda != 40 || water != 12 {
		t.Fatalf("stock soda=%v water=%v, want 40 and 12", soda, water)
	}
}

func TestGoodsReceiptWithAnUnknownProductSavesNothing(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	receipt := map[string]any{"reference": "GR-2", "received_on": "2026-11-08", "supplier": "SUP-1", "items": []map[string]any{
		{"product": "SODA", "quantity": 5, "unit_cost": 70}, {"product": "GHOST", "quantity": 5, "unit_cost": 70},
	}}
	code, body := post(t, h, db, reg, site, "Goods%20Receipt", receipt, "", admin)
	if code == http.StatusCreated {
		t.Fatalf("a receipt with an unknown product was saved: %s", body)
	}
	if got := stockOf(t, db, "SODA"); got != 10 {
		t.Fatalf("soda stock = %v, want unchanged 10 (the whole save must roll back)", got)
	}
	if n := countOf(t, db, "tabGoods Receipt"); n != 0 {
		t.Fatalf("a refused receipt left %d record(s) behind", n)
	}
}

func TestStockCannotBeTypedDirectlyOnTheProduct(t *testing.T) {
	h, db, reg, site := effectHarness(t)
	rec := serveResourceMutationBody(t, h, db, reg, site, httpMethodPut, "/api/resource/Product/SODA", `{"stock_qty":999}`, "", admin)
	if got := stockOf(t, db, "SODA"); got != 10 {
		t.Fatalf("stock was typed directly to %v (status %d: %s)", got, rec.Code, rec.Body.String())
	}
}

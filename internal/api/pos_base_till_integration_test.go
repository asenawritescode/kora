//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

func tillRow(t *testing.T, db *sql.DB, name string) (status string, opening, expected float64, registerCode sql.NullString) {
	t.Helper()
	if err := db.QueryRow("SELECT status, opening_cash, expected_cash, register_code FROM `tabTill Session` WHERE name = ?", name).Scan(&status, &opening, &expected, &registerCode); err != nil {
		t.Fatalf("till session %q: %v", name, err)
	}
	return status, opening, expected, registerCode
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPOSBaseRegisterOpensATillWithOpeningCash(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)

	result := serveManifestAction(t, handler, site, db, reg, "open_till", map[string]any{
		"opening_cash": 500.0,
		"_bindings":    map[string]any{"register": "REG-001"},
		"_inputs":      map[string]any{"opening_cash": 500.0},
	})
	if result.Code != http.StatusOK {
		t.Fatalf("open till status = %d: %s", result.Code, result.Body.String())
	}
	name, _ := requireActionDocument(t, result.Body.Bytes())["name"].(string)
	status, opening, expected, code := tillRow(t, db, name)
	if status != "Open" || opening != 500 || expected != 500 || code.String != "REG-001" {
		t.Fatalf("till = status %q opening %v expected %v register %q, want Open/500/500/REG-001", status, opening, expected, code.String)
	}
	var openedBy sql.NullString
	if err := db.QueryRow("SELECT opened_by FROM `tabTill Session` WHERE name = ?", name).Scan(&openedBy); err != nil || openedBy.String == "" {
		t.Fatalf("opened_by = %q (%v), want the cashier recorded", openedBy.String, err)
	}
}

func TestPOSBaseRegisterSaleNeedsAnOpenTill(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)

	// No till session at all: the sale is refused and nothing is saved.
	refused := serveManifestAction(t, handler, site, db, reg, "complete", posBasePanelCheckout("POS-NO-TILL", 300, map[string]any{"till_session": ""}))
	if refused.Code == http.StatusOK {
		t.Fatalf("a sale with no till session was accepted: %s", refused.Body.String())
	}
	// A till session that does not exist is refused too.
	missing := serveManifestAction(t, handler, site, db, reg, "complete", posBasePanelCheckout("POS-GHOST", 300, map[string]any{"till_session": "TILL-NOPE"}))
	if missing.Code == http.StatusOK {
		t.Fatalf("a sale on an unknown till session was accepted: %s", missing.Body.String())
	}
	if n := countRows(t, db, "tabSale"); n != 0 {
		t.Fatalf("%d sales saved by refused checkouts, want 0", n)
	}

	opened := serveManifestAction(t, handler, site, db, reg, "open_till", map[string]any{
		"opening_cash": 500.0, "_bindings": map[string]any{"register": "REG-001"}, "_inputs": map[string]any{"opening_cash": 500.0},
	})
	till, _ := requireActionDocument(t, opened.Body.Bytes())["name"].(string)

	sold := serveManifestAction(t, handler, site, db, reg, "complete", posBasePanelCheckout("POS-TILL-1", 300, map[string]any{"till_session": till}))
	if sold.Code != http.StatusOK {
		t.Fatalf("sale on an open till = %d: %s", sold.Code, sold.Body.String())
	}
	saleName, _ := requireActionDocument(t, sold.Body.Bytes())["name"].(string)

	var saleTill, paymentTill sql.NullString
	if err := db.QueryRow("SELECT till_session FROM `tabSale` WHERE name = ?", saleName).Scan(&saleTill); err != nil || saleTill.String != till {
		t.Fatalf("sale till_session = %q (%v), want %q", saleTill.String, err, till)
	}
	if err := db.QueryRow("SELECT till_session FROM `tabPayment` WHERE sale = ?", saleName).Scan(&paymentTill); err != nil || paymentTill.String != till {
		t.Fatalf("payment till_session = %q (%v), want %q", paymentTill.String, err, till)
	}
	if _, _, expected, _ := tillRow(t, db, till); expected != 800 {
		t.Fatalf("expected cash after a 300 cash sale on a 500 float = %v, want 800", expected)
	}
}

func TestPOSBaseClosedTillTakesNoMoreSalesAndCannotBeChanged(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)
	opened := serveManifestAction(t, handler, site, db, reg, "open_till", map[string]any{
		"opening_cash": 500.0, "_bindings": map[string]any{"register": "REG-001"}, "_inputs": map[string]any{"opening_cash": 500.0},
	})
	till, _ := requireActionDocument(t, opened.Body.Bytes())["name"].(string)
	if sold := serveManifestAction(t, handler, site, db, reg, "complete", posBasePanelCheckout("POS-BEFORE-CLOSE", 300, map[string]any{"till_session": till})); sold.Code != http.StatusOK {
		t.Fatalf("sale before closing = %d: %s", sold.Code, sold.Body.String())
	}
	path := "/api/resource/Till%20Session/" + till
	put := func(fields map[string]any) int {
		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, path, string(body), "", []string{"Administrator"}).Code
	}

	if code := put(map[string]any{"status": "Closed"}); code == http.StatusOK {
		t.Fatal("a till was closed without a counted cash figure")
	}
	if status, _, _, _ := tillRow(t, db, till); status != "Open" {
		t.Fatalf("status after a refused close = %q, want Open", status)
	}
	if code := put(map[string]any{"status": "Closed", "closing_cash": 795, "closed_at": "2026-11-11 18:00:00"}); code != http.StatusOK {
		t.Fatalf("closing with a count = %d, want 200", code)
	}
	if status, _, _, _ := tillRow(t, db, till); status != "Closed" {
		t.Fatalf("status after closing = %q, want Closed", status)
	}

	if after := serveManifestAction(t, handler, site, db, reg, "complete", posBasePanelCheckout("POS-AFTER-CLOSE", 100, map[string]any{"till_session": till})); after.Code == http.StatusOK {
		t.Fatalf("a sale on a closed till was accepted: %s", after.Body.String())
	}
	if code := put(map[string]any{"opening_cash": 9999}); code == http.StatusOK {
		t.Fatal("a closed till's opening cash was changed")
	}
	if _, opening, expected, _ := tillRow(t, db, till); opening != 500 || expected != 800 {
		t.Fatalf("closed till = opening %v expected %v, want 500/800 unchanged", opening, expected)
	}
}

// Sales that exist before the upgrade, and entries made outside the register,
// have no till session. They must still save: the rule is on the register.
func TestPOSBaseSaleWithoutATillSessionStillSavesOutsideTheRegister(t *testing.T) {
	handler, db, reg, site := newPOSBaseHandler(t)
	seedPOSBaseShop(t, db)
	body := `{"reference":"BOOK-1","sale_date":"2026-11-11","items":[{"product":"PROD-001","quantity":1,"unit_price":100}]}`
	created := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost, "/api/resource/Sale", body, "", []string{"Administrator"})
	if created.Code != http.StatusOK && created.Code != http.StatusCreated {
		t.Fatalf("a sale entered outside the register = %d: %s", created.Code, created.Body.String())
	}
	var till sql.NullString
	if err := db.QueryRow("SELECT till_session FROM `tabSale` WHERE reference = 'BOOK-1'").Scan(&till); err != nil || till.Valid && till.String != "" {
		t.Fatalf("till_session = %v (%v), want empty", till, err)
	}
}

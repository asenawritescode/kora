//go:build integration

package api

import (
	"net/http"
	"net/url"
	"testing"
)

// The whole buying flow on real MySQL, with the packs loaded as shipped: a
// supplier, an order, goods received (which raise stock), the supplier's bill and
// our payment of it.
func TestERPPurchasingBuyToPayWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "pos-base", "erp-purchasing")
	if _, err := db.Exec("INSERT INTO `tabProduct` (name, reference, product_name, selling_price, stock_qty, status) VALUES ('PROD-001','P-1','Soda',100,0,'Active')"); err != nil {
		t.Fatal(err)
	}
	send := func(method resourceHTTPMethod, doctype, name, body string) (int, map[string]any) {
		path := "/api/resource/" + url.PathEscape(doctype)
		if name != "" {
			path += "/" + url.PathEscape(name)
		}
		recorder := serveResourceMutationBody(t, handler, db, reg, site, method, path, body, "", []string{"Administrator"})
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
			return recorder.Code, nil
		}
		return recorder.Code, requireRESTDocument(t, recorder.Body.Bytes(), doctype)
	}
	create := func(doctype, body string) string {
		t.Helper()
		code, doc := send(httpMethodPost, doctype, "", body)
		if doc == nil {
			t.Fatalf("create %s = %d", doctype, code)
		}
		name, _ := doc["name"].(string)
		return name
	}
	stock := func() float64 {
		var qty float64
		if err := db.QueryRow("SELECT stock_qty FROM `tabProduct` WHERE name = 'PROD-001'").Scan(&qty); err != nil {
			t.Fatal(err)
		}
		return qty
	}

	supplier := create("Supplier", `{"supplier_name":"Dar Traders"}`)
	order := create("Purchase Order", `{"reference":"PO-1","order_date":"2026-11-11","supplier":"`+supplier+`","items":[{"product":"PROD-001","quantity":10,"unit_cost":2}]}`)
	var orderTotal float64
	if err := db.QueryRow("SELECT total FROM `tabPurchase Order` WHERE name = ?", order).Scan(&orderTotal); err != nil || orderTotal != 20 {
		t.Fatalf("order total = %v (%v), want 20", orderTotal, err)
	}
	if got := stock(); got != 0 {
		t.Fatalf("stock after only ordering = %v, want 0 (an order does not move stock)", got)
	}

	receipt := create("Goods Receipt", `{"reference":"GR-1","received_on":"2026-11-12","supplier":"`+supplier+`","purchase_order":"`+order+`","items":[{"product":"PROD-001","quantity":10}]}`)
	if got := stock(); got != 10 {
		t.Fatalf("stock after receiving 10 = %v, want 10", got)
	}
	// A receipt is a fact that goods arrived: it cannot be edited afterwards.
	if code, edited := send(httpMethodPut, "Goods Receipt", receipt, `{"notes":"changed"}`); edited != nil {
		t.Fatalf("a saved goods receipt was edited (%d)", code)
	}
	if got := stock(); got != 10 {
		t.Fatalf("stock after a refused receipt edit = %v, want 10", got)
	}

	bill := create("Supplier Bill", `{"reference":"BILL-1","supplier":"`+supplier+`","bill_date":"2026-11-13","purchase_order":"`+order+`","goods_receipt":"`+receipt+`","amount":20}`)
	payment := create("Supplier Payment", `{"reference":"PAY-1","payment_date":"2026-11-14","supplier":"`+supplier+`","supplier_bill":"`+bill+`","amount":20}`)
	var baseAmount float64
	if err := db.QueryRow("SELECT base_amount FROM `tabSupplier Payment` WHERE name = ?", payment).Scan(&baseAmount); err != nil || baseAmount != 20 {
		t.Fatalf("payment base amount = %v (%v), want 20", baseAmount, err)
	}

	// A receipt for a product that does not exist is refused and moves nothing.
	if code, bad := send(httpMethodPost, "Goods Receipt", "", `{"reference":"GR-BAD","received_on":"2026-11-12","supplier":"`+supplier+`","items":[{"product":"NOPE","quantity":5}]}`); bad != nil {
		t.Fatalf("a receipt for an unknown product was accepted (%d)", code)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabGoods Receipt` WHERE reference = 'GR-BAD'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused receipt left %d rows (%v)", rows, err)
	}
}

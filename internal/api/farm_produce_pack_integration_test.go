//go:build integration

package api

import (
	"strconv"
	"strings"
	"testing"
)

// The Produce pack as shipped, on real MySQL: a delivery raises the product's
// stock and prices itself, refusals move no stock, and a saved delivery is frozen.
func TestProduceDeliveryRaisesStockAndIsFrozenOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "pos-base", "farm-produce")
	c := newPackClient(t, handler, db, reg, site)
	if _, err := db.Exec("INSERT INTO `tabProduct` (name, reference, product_name, selling_price, stock_qty, status) VALUES ('PROD-001','P-1','Maize',60,0,'Active')"); err != nil {
		t.Fatal(err)
	}
	number := func(recordType, name, column string) float64 {
		t.Helper()
		got, err := strconv.ParseFloat(c.value(recordType, name, column), 64)
		if err != nil {
			t.Fatalf("%s %s.%s is not a number: %v", recordType, name, column, err)
		}
		return got
	}
	stock := func() float64 { return number("Product", "PROD-001", "stock_qty") }

	farmer := c.create("Farmer", `{"member_no":"M-1","farmer_name":"Achieng Otieno","village":"Kisumu","group_name":"Lakeside"}`)
	if !c.refused(httpMethodPost, "Farmer", "", `{"member_no":"M-1","farmer_name":"Other Farmer"}`) {
		t.Fatal("a second farmer with the same member number was accepted")
	}

	delivery := c.create("Produce Delivery", `{"reference":"D-1","farmer":"`+farmer+`","product":"PROD-001","delivery_date":"2026-12-03","quantity_kg":120.5,"grade":"A","price_per_kg":45}`)
	if got := stock(); got != 120.5 {
		t.Fatalf("stock after delivering 120.5 kg = %v, want 120.5", got)
	}
	if got := number("Produce Delivery", delivery, "amount"); got != 5422.5 {
		t.Fatalf("amount = %v, want 5422.50", got)
	}
	if got := c.value("Produce Delivery", delivery, "status"); got != "Received" {
		t.Fatalf("status = %q, want Received", got)
	}
	if got := c.value("Produce Delivery", delivery, "delivery_date"); !strings.HasPrefix(got, "2026-12-03") {
		t.Fatalf("delivery date = %q", got)
	}
	c.create("Produce Delivery", `{"reference":"D-2","farmer":"`+farmer+`","product":"PROD-001","delivery_date":"2026-12-04","quantity_kg":10,"grade":"B","price_per_kg":30,"status":"Paid"}`)
	if got := stock(); got != 130.5 {
		t.Fatalf("stock after a second delivery = %v, want 130.5", got)
	}

	// A saved delivery is frozen: no edit, not even the status, and stock stays.
	if !c.refused(httpMethodPut, "Produce Delivery", delivery, `{"status":"Paid"}`) {
		t.Fatal("a saved delivery was edited")
	}
	if !c.refused(httpMethodPut, "Produce Delivery", delivery, `{"quantity_kg":1}`) {
		t.Fatal("the quantity of a saved delivery was changed")
	}
	if got := c.value("Produce Delivery", delivery, "status"); got != "Received" {
		t.Fatalf("status after a refused edit = %q, want Received", got)
	}

	// Refusals move no stock and leave no record.
	for name, body := range map[string]string{
		"zero quantity":     `{"reference":"D-BAD1","farmer":"` + farmer + `","product":"PROD-001","delivery_date":"2026-12-05","quantity_kg":0}`,
		"unknown product":   `{"reference":"D-BAD3","farmer":"` + farmer + `","product":"NOPE","delivery_date":"2026-12-05","quantity_kg":5}`,
		"duplicate ref":     `{"reference":"D-1","farmer":"` + farmer + `","product":"PROD-001","delivery_date":"2026-12-05","quantity_kg":5}`,
		"negative quantity": `{"reference":"D-BAD6","farmer":"` + farmer + `","product":"PROD-001","delivery_date":"2026-12-05","quantity_kg":-5}`,
		"missing quantity":  `{"reference":"D-BAD5","farmer":"` + farmer + `","product":"PROD-001","delivery_date":"2026-12-05"}`,
	} {
		if !c.refused(httpMethodPost, "Produce Delivery", "", body) {
			t.Errorf("%s: the delivery was accepted", name)
		}
	}
	if got := stock(); got != 130.5 {
		t.Fatalf("stock after refused deliveries = %v, want 130.5", got)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabProduce Delivery`").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("deliveries stored = %d (%v), want 2", rows, err)
	}
}

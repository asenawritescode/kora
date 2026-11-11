//go:build integration

package api

import (
	"strconv"
	"testing"
)

// The Menu pack as shipped, on real MySQL: an order's items add up, an empty order is
// refused, unique numbers hold, a cancellation needs a reason, and a served
// order's table is frozen.
func TestMenuKitchenOrderWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "pos-base", "menu-tables")
	c := newPackClient(t, handler, db, reg, site)

	stew := c.create("Menu Item", `{"item_name":"Beef stew","category":"Main","price":650}`)
	tea := c.create("Menu Item", `{"item_name":"Masala tea","category":"Drink","price":120}`)
	if got := c.value("Menu Item", stew, "available"); got != "1" {
		t.Fatalf("new menu item available = %q, want 1", got)
	}

	table := c.create("Dining Table", `{"table_no":"T1","seats":4}`)
	if got := c.value("Dining Table", table, "status"); got != "Free" {
		t.Fatalf("new table status = %q, want Free", got)
	}
	if !c.refused(httpMethodPost, "Dining Table", "", `{"table_no":"T1","seats":2}`) {
		t.Fatal("a second table with the same number was accepted")
	}
	other := c.create("Dining Table", `{"table_no":"T2","seats":2}`)

	line := func(item string, qty int) string {
		return `{"menu_item":"` + item + `","quantity":` + strconv.Itoa(qty) + `}`
	}
	if !c.refused(httpMethodPost, "Kitchen Order", "", `{"reference":"KO-0","dining_table":"`+table+`","lines":[]}`) {
		t.Fatal("an order with no lines was accepted")
	}
	order := c.create("Kitchen Order", `{"reference":"KO-1","dining_table":"`+table+`","lines":[`+line(stew, 2)+`,`+line(tea, 3)+`]}`)
	if got := c.value("Kitchen Order", order, "status"); got != "New" {
		t.Fatalf("new order status = %q, want New", got)
	}
	if got := c.value("Kitchen Order", order, "item_count"); got != "5" {
		t.Fatalf("items ordered = %q, want 5", got)
	}
	if !c.refused(httpMethodPost, "Kitchen Order", "", `{"reference":"KO-1","dining_table":"`+table+`","lines":[`+line(tea, 1)+`]}`) {
		t.Fatal("a second order with the same reference was accepted")
	}

	if c.refused(httpMethodPut, "Kitchen Order", order, `{"status":"Preparing"}`) {
		t.Fatal("moving an order to Preparing was refused")
	}
	if c.refused(httpMethodPut, "Kitchen Order", order, `{"status":"Served"}`) {
		t.Fatal("serving an order was refused")
	}
	if !c.refused(httpMethodPut, "Kitchen Order", order, `{"dining_table":"`+other+`"}`) {
		t.Fatal("the table of a served order was changed")
	}
	if got := c.value("Kitchen Order", order, "dining_table"); got != table {
		t.Fatalf("table after a refused change = %q, want %q", got, table)
	}

	second := c.create("Kitchen Order", `{"reference":"KO-2","dining_table":"`+other+`","lines":[`+line(tea, 1)+`]}`)
	if !c.refused(httpMethodPut, "Kitchen Order", second, `{"status":"Cancelled"}`) {
		t.Fatal("an order was cancelled without a reason")
	}
	if got := c.value("Kitchen Order", second, "status"); got != "New" {
		t.Fatalf("status after a refused cancellation = %q, want New", got)
	}
	if c.refused(httpMethodPut, "Kitchen Order", second, `{"status":"Cancelled","notes":"Guest left"}`) {
		t.Fatal("a cancellation with a reason was refused")
	}
}

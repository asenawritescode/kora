//go:build integration

package api

import (
	"strconv"
	"testing"
)

func manufacturingStock(t *testing.T, c *packClient, product string) float64 {
	t.Helper()
	got, err := strconv.ParseFloat(c.value("Product", product, "stock_qty"), 64)
	if err != nil {
		t.Fatalf("stock of %s: %v", product, err)
	}
	return got
}

// The Manufacturing pack as shipped, on real MySQL: a recipe, a work order,
// materials issued (stock down), a production run (finished stock up), and the
// refusals around them.
func TestManufacturingRecipeWorkOrderIssueAndRunWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "pos-base", "manufacturing")
	c := newPackClient(t, handler, db, reg, site)

	flour := c.create("Product", `{"reference":"M-FLOUR","product_name":"Flour","selling_price":0,"stock_qty":50,"unit":"kg"}`)
	bread := c.create("Product", `{"reference":"M-BREAD","product_name":"Bread","selling_price":100}`)
	if got := manufacturingStock(t, c, flour); got != 50 {
		t.Fatalf("flour stock = %v, want 50 (set at creation)", got)
	}

	bom := c.create("Bill of Materials", `{"reference":"BOM-BREAD","product":"`+bread+`","output_quantity":10,"status":"Active","components":[{"material":"`+flour+`","quantity":5}]}`)
	if !c.refused(httpMethodPost, "Bill of Materials", "", `{"reference":"BOM-BREAD","product":"`+bread+`"}`) {
		t.Fatal("a second recipe with the same reference was accepted")
	}
	if !c.refused(httpMethodPost, "Bill of Materials", "", `{"reference":"BOM-ZERO","product":"`+bread+`","output_quantity":0}`) {
		t.Fatal("a recipe that makes nothing was accepted")
	}
	// Observed: the Engine does not check the minimum on child table rows
	// (material quantity), so it is not asserted either way here.

	order := c.create("Work Order", `{"reference":"WO-1","bom":"`+bom+`","quantity":20,"due_date":"2026-12-03"}`)
	if got := c.value("Work Order", order, "status"); got != "Planned" {
		t.Fatalf("work order status = %q, want Planned", got)
	}
	if got := manufacturingStock(t, c, flour); got != 50 {
		t.Fatalf("creating a work order changed flour stock to %v, want 50", got)
	}
	if !c.refused(httpMethodPost, "Work Order", "", `{"reference":"WO-BAD","bom":"`+bom+`","quantity":0}`) {
		t.Fatal("a work order for zero was accepted")
	}

	// Materials are used up only when an issue is recorded.
	issue := c.create("Material Issue", `{"reference":"MI-1","work_order":"`+order+`","material":"`+flour+`","quantity":10,"issue_date":"2026-12-01"}`)
	if got := manufacturingStock(t, c, flour); got != 40 {
		t.Fatalf("flour stock after issue = %v, want 40", got)
	}
	// Observed Engine behaviour: stock never goes below zero; the issue is
	// refused and nothing is saved.
	if !c.refused(httpMethodPost, "Material Issue", "", `{"reference":"MI-TOO-MUCH","work_order":"`+order+`","material":"`+flour+`","quantity":41}`) {
		t.Fatal("issuing more than is in stock was accepted")
	}
	if got := manufacturingStock(t, c, flour); got != 40 {
		t.Fatalf("flour stock after a refused issue = %v, want 40", got)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabMaterial Issue` WHERE reference = 'MI-TOO-MUCH'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused issue left %d record(s), %v", n, err)
	}
	if !c.refused(httpMethodPut, "Material Issue", issue, `{"quantity":1}`) {
		t.Fatal("a material issue was edited after saving")
	}

	run := c.create("Production Run", `{"reference":"PR-1","work_order":"`+order+`","product":"`+bread+`","quantity":20,"run_date":"2026-12-02"}`)
	if got := manufacturingStock(t, c, bread); got != 20 {
		t.Fatalf("bread stock after run = %v, want 20", got)
	}
	if !c.refused(httpMethodPost, "Production Run", "", `{"reference":"PR-0","product":"`+bread+`","quantity":0,"run_date":"2026-12-02"}`) {
		t.Fatal("a production run of zero was accepted")
	}
	if !c.refused(httpMethodPost, "Production Run", "", `{"reference":"PR-NODATE","product":"`+bread+`","quantity":1}`) {
		t.Fatal("a production run without a date was accepted")
	}
	if !c.refused(httpMethodPut, "Production Run", run, `{"quantity":999}`) {
		t.Fatal("a production run was edited after saving")
	}
	if got := manufacturingStock(t, c, bread); got != 20 {
		t.Fatalf("bread stock after refused edit = %v, want 20", got)
	}

	// The work order status is set by a person; nothing checks for a run.
	if c.refused(httpMethodPut, "Work Order", order, `{"status":"Done"}`) {
		t.Fatal("marking a work order Done was refused")
	}

	// A retired recipe is frozen.
	if c.refused(httpMethodPut, "Bill of Materials", bom, `{"status":"Retired"}`) {
		t.Fatal("retiring a recipe was refused")
	}
	if !c.refused(httpMethodPut, "Bill of Materials", bom, `{"product":"`+flour+`"}`) {
		t.Fatal("the product of a retired recipe was changed")
	}
	if got := c.value("Bill of Materials", bom, "product"); got != bread {
		t.Fatalf("product after a refused change = %q, want %q", got, bread)
	}
	if !c.refused(httpMethodPut, "Bill of Materials", bom, `{"components":[{"material":"`+flour+`","quantity":99}]}`) {
		t.Fatal("the materials of a retired recipe were replaced")
	}
	var quantity float64
	if err := db.QueryRow("SELECT quantity FROM `tabBill of Materials__components` WHERE parent = ?", bom).Scan(&quantity); err != nil || quantity != 5 {
		t.Fatalf("material quantity after a refused change = %v, %v; want 5", quantity, err)
	}
}

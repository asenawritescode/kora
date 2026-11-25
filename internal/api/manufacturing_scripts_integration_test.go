//go:build integration

package api

import "testing"

const manufacturingPack = "../../config/_parked/manufacturing"

// The Manufacturing pack with its scripts, loaded as shipped on MySQL and PostgreSQL. A recipe must
// be Active before work is ordered from it, runs and material issues need an open work order, the
// quantity made follows the runs, and a work order can be Done only once the quantity is made.
func TestPackScriptsManufacturing(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, manufacturingPack}, func(f *packFixture) {
		runManufacturingScenario(f)
	})
}

// packProductStock reads a product's stock as a number.
func packProductStock(f *packFixture, product string) float64 {
	return f.amount("Product", product, "stock_qty")
}

func runManufacturingScenario(f *packFixture) {
	flour := f.create("Product", `{"reference":"M-FLOUR","product_name":"Flour","selling_price":0,"stock_qty":50,"unit":"kg"}`)
	bread := f.create("Product", `{"reference":"M-BREAD","product_name":"Bread","selling_price":100}`)
	cake := f.create("Product", `{"reference":"M-CAKE","product_name":"Cake","selling_price":200}`)
	draft := f.create("Bill of Materials", `{"reference":"BOM-DRAFT","product":"`+bread+`","output_quantity":10,"components":[{"material":"`+flour+`","quantity":5}]}`)
	recipe := f.create("Bill of Materials", `{"reference":"BOM-BREAD","product":"`+bread+`","output_quantity":10,"status":"Active","components":[{"material":"`+flour+`","quantity":5}]}`)

	t := f.t
	var order string
	t.Run("recipe cannot use its own product as a material", func(t *testing.T) {
		f.refused(httpMethodPost, "Bill of Materials", "", `{"reference":"BOM-SELF","product":"`+bread+`","output_quantity":1,"components":[{"material":"`+bread+`","quantity":1}]}`, "cannot use its own product as a material")
		f.refused(httpMethodPut, "Bill of Materials", recipe, `{"components":[{"material":"`+bread+`","quantity":1}]}`, "cannot use its own product as a material")
	})
	t.Run("each material of a recipe needs a quantity above zero", func(t *testing.T) {
		f.refused(httpMethodPost, "Bill of Materials", "", `{"reference":"BOM-ZERO","product":"`+bread+`","output_quantity":1,"components":[{"material":"`+flour+`","quantity":0}]}`, "has a material whose quantity is not above zero")
		f.refused(httpMethodPut, "Bill of Materials", recipe, `{"components":[{"material":"`+flour+`","quantity":0}]}`, "has a material whose quantity is not above zero")
	})
	t.Run("work order needs an Active recipe on create and on recipe change", func(t *testing.T) {
		f.refused(httpMethodPost, "Work Order", "", `{"reference":"WO-DRAFT","bom":"`+draft+`","quantity":5}`, "BOM-DRAFT is Draft; activate it first.")
		order = f.create("Work Order", `{"reference":"WO-1","bom":"`+recipe+`","quantity":20}`)
		f.expect("Work Order", order, "status", "Planned")
		expectMadeQuantity(f, order, 0)
		f.refused(httpMethodPut, "Work Order", order, `{"bom":"`+draft+`"}`, "BOM-DRAFT is Draft; activate it first.")
	})
	t.Run("work order must make more than zero", func(t *testing.T) {
		f.refused(httpMethodPost, "Work Order", "", `{"reference":"WO-ZERO","bom":"`+recipe+`","quantity":0}`, "The quantity must be more than zero.")
	})
	t.Run("work order cannot be Done before the full quantity is made", func(t *testing.T) {
		f.refused(httpMethodPut, "Work Order", order, `{"status":"Done"}`, "Produced 0 of 20.")
	})
	t.Run("production run adds to quantity made, moves the order and adds stock", func(t *testing.T) {
		before := packProductStock(f, bread)
		f.create("Production Run", `{"reference":"PR-1","work_order":"`+order+`","product":"`+bread+`","quantity":8,"run_date":"2026-12-02"}`)
		expectMadeQuantity(f, order, 8)
		f.expect("Work Order", order, "status", "In progress")
		if got := packProductStock(f, bread); got != before+8 {
			t.Fatalf("bread stock after run = %v, want %v", got, before+8)
		}
	})
	t.Run("production run must make the recipe's product", func(t *testing.T) {
		f.refused(httpMethodPost, "Production Run", "", `{"reference":"PR-WRONG","work_order":"`+order+`","product":"`+cake+`","quantity":2,"run_date":"2026-12-02"}`, "WO-1 makes M-BREAD.")
	})
	t.Run("production run must be more than zero", func(t *testing.T) {
		f.refused(httpMethodPost, "Production Run", "", `{"reference":"PR-ZERO","work_order":"`+order+`","product":"`+bread+`","quantity":0,"run_date":"2026-12-02"}`, "needs a nonzero number in quantity")
	})
	t.Run("work order is Done once the full quantity is made", func(t *testing.T) {
		f.create("Production Run", `{"reference":"PR-2","work_order":"`+order+`","product":"`+bread+`","quantity":12,"run_date":"2026-12-03"}`)
		expectMadeQuantity(f, order, 20)
		f.update("Work Order", order, `{"status":"Done"}`)
		f.expect("Work Order", order, "status", "Done")
	})
	t.Run("done work order takes no production or materials and cannot change status", func(t *testing.T) {
		f.refused(httpMethodPost, "Production Run", "", `{"reference":"PR-LATE","work_order":"`+order+`","product":"`+bread+`","quantity":1,"run_date":"2026-12-04"}`, "WO-1 is Done; only a planned or in-progress order takes production.")
		f.refused(httpMethodPost, "Material Issue", "", `{"reference":"MI-LATE","work_order":"`+order+`","material":"`+flour+`","quantity":1}`, "WO-1 is Done; only a planned or in-progress order takes materials.")
		f.refused(httpMethodPut, "Work Order", order, `{"status":"In progress"}`, "WO-1 is Done; it cannot change.")
	})
	t.Run("cancelled work order takes no production and cannot change status", func(t *testing.T) {
		cancelled := f.create("Work Order", `{"reference":"WO-2","bom":"`+recipe+`","quantity":5}`)
		f.update("Work Order", cancelled, `{"status":"Cancelled"}`)
		f.refused(httpMethodPut, "Work Order", cancelled, `{"status":"Planned"}`, "WO-2 is Cancelled; it cannot change.")
		f.refused(httpMethodPost, "Production Run", "", `{"reference":"PR-CANCEL","work_order":"`+cancelled+`","product":"`+bread+`","quantity":1,"run_date":"2026-12-04"}`, "WO-2 is Cancelled; only a planned or in-progress order takes production.")
	})
	t.Run("material issue moves a planned order to In progress and takes stock", func(t *testing.T) {
		planned := f.create("Work Order", `{"reference":"WO-3","bom":"`+recipe+`","quantity":4}`)
		before := packProductStock(f, flour)
		f.create("Material Issue", `{"reference":"MI-1","work_order":"`+planned+`","material":"`+flour+`","quantity":5,"issue_date":"2026-12-01"}`)
		f.expect("Work Order", planned, "status", "In progress")
		if got := packProductStock(f, flour); got != before-5 {
			t.Fatalf("flour stock after issue = %v, want %v", got, before-5)
		}
		f.refused(httpMethodPost, "Material Issue", "", `{"reference":"MI-ZERO","work_order":"`+planned+`","material":"`+flour+`","quantity":0}`, "needs a nonzero number in quantity")
	})
	t.Run("production operator runs and issues on an order it can only read", func(t *testing.T) {
		op := f.as("Production Operator", "Cashier")
		open := f.create("Work Order", `{"reference":"WO-4","bom":"`+recipe+`","quantity":6}`)
		if _, _, doc := op.send(httpMethodPut, "Work Order", open, `{"notes":"edited by operator"}`); doc != nil {
			t.Fatal("production operator edited a work order directly")
		}
		beforeBread := packProductStock(f, bread)
		beforeFlour := packProductStock(f, flour)
		op.create("Production Run", `{"reference":"PR-OP","work_order":"`+open+`","product":"`+bread+`","quantity":6,"run_date":"2026-12-05"}`)
		expectMadeQuantity(f, open, 6)
		if got := packProductStock(f, bread); got != beforeBread+6 {
			t.Fatalf("bread stock after operator run = %v, want %v", got, beforeBread+6)
		}
		op.create("Material Issue", `{"reference":"MI-OP","work_order":"`+open+`","material":"`+flour+`","quantity":1,"issue_date":"2026-12-05"}`)
		f.expect("Work Order", open, "status", "In progress")
		if got := packProductStock(f, flour); got != beforeFlour-1 {
			t.Fatalf("flour stock after operator issue = %v, want %v", got, beforeFlour-1)
		}
	})
}

// expectMadeQuantity fails the test unless a work order's quantity made equals want.
func expectMadeQuantity(f *packFixture, order string, want float64) {
	f.t.Helper()
	if got := f.amount("Work Order", order, "quantity_produced"); got != want {
		f.t.Fatalf("work order %s quantity made = %v, want %v", order, got, want)
	}
}

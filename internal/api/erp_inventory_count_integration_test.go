//go:build integration

package api

import "testing"

// A submitted stock count posts one netted entry through lib_books: value counted over is a
// gain (Dr Inventory / Cr Stock Adjustments) and value counted short is a loss (the opposite).
// Stock Manager has no permission on Product or the books, so the entry proves the script posts
// with the count's authority.
func TestPackScriptsStockCountPosting(t *testing.T) {
	eachPackDialect(t, []string{erpsellPack, erpInventoryPack, erpAccountingPack}, func(f *packFixture) {
		f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", "KES", "KES", "KES", true)

		f.run("a submitted count posts the net difference at cost, once", func(f *packFixture) {
			tea := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 10}))
			sugar := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Sugar", "selling_price": 50, "cost_price": 20, "stock_qty": 8}))
			// Tea counted 12 (gain 2 at 40 = +80); Sugar counted 5 (loss 3 at 20 = -60); net +20.
			count := f.as("Stock Manager").create("Stock Count", jsonBody(f.t, map[string]any{
				"status": "Draft",
				"items": []any{
					map[string]any{"product": tea, "counted_qty": 12},
					map[string]any{"product": sugar, "counted_qty": 5},
				},
			}))
			f.as("Stock Manager").update("Stock Count", count, `{"status":"Submitted"}`)

			entry := stockEntryOf(f, count)
			f.expect("Journal Entry", entry, "status", "Posted")
			f.expect("Journal Entry", entry, "memo", "Stock count")
			if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ?", "Stock Count", count); got != 1 {
				f.t.Fatalf("entries for %s = %d, want 1", count, got)
			}
			if got := accountingNet(f, inventoryAccount(f, "Inventory"), entry); got != 20 {
				f.t.Fatalf("Inventory net = %v, want 20 (gain 80 less loss 60)", got)
			}
			if got := accountingNet(f, inventoryAccount(f, "Stock Adjustments"), entry); got != -20 {
				f.t.Fatalf("Stock Adjustments net = %v, want -20", got)
			}
			expectStock(f, tea, 12)
			expectStock(f, sugar, 5)
		})

		f.run("a count with no differences posts nothing", func(f *packFixture) {
			salt := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Salt", "selling_price": 30, "cost_price": 10, "stock_qty": 4}))
			count := f.as("Stock Manager").create("Stock Count", jsonBody(f.t, map[string]any{
				"status": "Draft",
				"items":  []any{map[string]any{"product": salt, "counted_qty": 4}},
			}))
			f.as("Stock Manager").update("Stock Count", count, `{"status":"Submitted"}`)
			if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ?", "Stock Count", count); got != 0 {
				f.t.Fatalf("entries for a no-difference count = %d, want 0", got)
			}
			expectStock(f, salt, 4)
		})
	})
}

// Without the books (no erp-accounting, so lib_books is absent) a count still submits and sets
// stock; nothing posts and no ReferenceError is raised.
func TestPackScriptsStockCountWithoutBooks(t *testing.T) {
	eachPackDialect(t, []string{erpsellPack, erpInventoryPack}, func(f *packFixture) {
		f.run("a stock count submits and sets stock when the books are not installed", func(f *packFixture) {
			tea := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 10}))
			count := f.as("Stock Manager").create("Stock Count", jsonBody(f.t, map[string]any{
				"status": "Draft",
				"items":  []any{map[string]any{"product": tea, "counted_qty": 7}},
			}))
			f.as("Stock Manager").update("Stock Count", count, `{"status":"Submitted"}`)
			expectStock(f, tea, 7)
		})
	})
}

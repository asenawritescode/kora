//go:build integration

package api

import "testing"

// The purchasing pack posts to the books through lib_books when erp-accounting is installed:
// a goods receipt brings stock in against a holding account, a bill turns that into what the
// supplier is owed (or an expense), and a payment settles it. Purchasing Manager has no
// permission on the books, so every entry proves the script posts with the record's authority.
func TestPackScriptsPurchasingPosting(t *testing.T) {
	packs := []string{erpSellingPack, erpPurchasingPack, erpAccountingPack}
	eachPackDialect(t, packs, func(f *packFixture) {
		f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", "KES", "KES", "KES", true)
		buyer := f.as("Purchasing Manager")

		f.run("a goods receipt posts stock into the holding account", func(f *packFixture) {
			supplier := f.create("Supplier", `{"supplier_name":"Dar Traders"}`)
			tea := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 0}))
			receipt := buyer.create("Goods Receipt", jsonBody(f.t, map[string]any{
				"supplier": supplier, "received_on": "2026-11-20",
				"items": []any{map[string]any{"product": tea, "quantity": 10, "unit_cost": 40}},
			}))
			entry := stockEntryOf(f, receipt)
			f.expect("Journal Entry", entry, "memo", "Goods receipt")
			if got := accountingNet(f, inventoryAccount(f, "Inventory"), entry); got != 400 {
				f.t.Fatalf("Inventory net = %v, want 400 (10 at 40)", got)
			}
			if got := accountingNet(f, inventoryAccount(f, "Goods Received Not Billed"), entry); got != -400 {
				f.t.Fatalf("GRNI net = %v, want -400", got)
			}
		})

		f.run("a bill against a receipt clears the holding account into payables", func(f *packFixture) {
			supplier := f.create("Supplier", `{"supplier_name":"Nairobi Supply"}`)
			sugar := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Sugar", "selling_price": 60, "cost_price": 25, "stock_qty": 0}))
			receipt := buyer.create("Goods Receipt", jsonBody(f.t, map[string]any{
				"supplier": supplier, "received_on": "2026-11-20",
				"items": []any{map[string]any{"product": sugar, "quantity": 8, "unit_cost": 25}},
			}))
			bill := buyer.create("Supplier Bill", jsonBody(f.t, map[string]any{
				"reference": "BILL-001", "supplier": supplier, "bill_date": "2026-11-20",
				"goods_receipt": receipt, "amount": 200, "status": "Open",
			}))
			entry := stockEntryOf(f, bill)
			f.expect("Journal Entry", entry, "memo", "Supplier bill")
			if got := accountingNet(f, inventoryAccount(f, "Goods Received Not Billed"), entry); got != 200 {
				f.t.Fatalf("GRNI net on the bill = %v, want 200 (cleared)", got)
			}
			if got := accountingNet(f, inventoryAccount(f, "Payable"), entry); got != -200 {
				f.t.Fatalf("Payable net = %v, want -200", got)
			}
		})

		f.run("a bill with no receipt is an expense", func(f *packFixture) {
			supplier := f.create("Supplier", `{"supplier_name":"City Power"}`)
			bill := buyer.create("Supplier Bill", jsonBody(f.t, map[string]any{
				"reference": "BILL-UTIL", "supplier": supplier, "bill_date": "2026-11-20",
				"amount": 150, "status": "Open",
			}))
			entry := stockEntryOf(f, bill)
			if got := accountingNet(f, inventoryAccount(f, "Expenses"), entry); got != 150 {
				f.t.Fatalf("Expenses net = %v, want 150", got)
			}
			if got := accountingNet(f, inventoryAccount(f, "Payable"), entry); got != -150 {
				f.t.Fatalf("Payable net = %v, want -150", got)
			}
		})

		f.run("a payment settles the payable and voiding it reverses", func(f *packFixture) {
			supplier := f.create("Supplier", `{"supplier_name":"Lake Traders"}`)
			bill := buyer.create("Supplier Bill", jsonBody(f.t, map[string]any{
				"reference": "BILL-PAY", "supplier": supplier, "bill_date": "2026-11-20",
				"amount": 300, "status": "Open",
			}))
			payment := buyer.create("Supplier Payment", jsonBody(f.t, map[string]any{
				"reference": "PAY-001", "payment_date": "2026-11-21", "supplier": supplier,
				"supplier_bill": bill, "method": "Cash", "amount": 300, "status": "Paid",
			}))
			entry := stockEntryOf(f, payment)
			f.expect("Journal Entry", entry, "memo", "Supplier payment")
			if got := accountingNet(f, inventoryAccount(f, "Payable"), entry); got != 300 {
				f.t.Fatalf("Payable net on payment = %v, want 300 (debit, settling it)", got)
			}
			if got := accountingNet(f, inventoryAccount(f, "Cash"), entry); got != -300 {
				f.t.Fatalf("Cash net = %v, want -300", got)
			}
			// Voiding the payment reverses its entry.
			buyer.update("Supplier Payment", payment, `{"status":"Voided"}`)
			if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ? AND status = ?", "Supplier Payment", payment, "Reversed"); got != 1 {
				f.t.Fatalf("reversed entries for %s = %d, want 1", payment, got)
			}
		})
	})
}

// Without erp-accounting (lib_books absent) every purchasing document still saves and posts
// nothing; no ReferenceError is raised.
func TestPackScriptsPurchasingPostingWithoutBooks(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, erpPurchasingPack}, func(f *packFixture) {
		f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", "KES", "KES", "KES", true)
		f.run("a receipt, a bill and a payment save when the books are not installed", func(f *packFixture) {
			buyer := f.as("Purchasing Manager")
			supplier := f.create("Supplier", `{"supplier_name":"No Books Ltd"}`)
			rice := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Rice", "selling_price": 90, "cost_price": 50, "stock_qty": 0}))
			buyer.create("Goods Receipt", jsonBody(f.t, map[string]any{
				"supplier": supplier, "received_on": "2026-11-20",
				"items": []any{map[string]any{"product": rice, "quantity": 4, "unit_cost": 50}},
			}))
			bill := buyer.create("Supplier Bill", jsonBody(f.t, map[string]any{
				"reference": "NB-1", "supplier": supplier, "bill_date": "2026-11-20", "amount": 200, "status": "Open",
			}))
			buyer.create("Supplier Payment", jsonBody(f.t, map[string]any{
				"reference": "NB-PAY", "payment_date": "2026-11-21", "supplier": supplier,
				"supplier_bill": bill, "method": "Cash", "amount": 200, "status": "Paid",
			}))
			f.expect("Supplier Bill", bill, "status", "Paid")
		})
	})
}

//go:build integration

package api

import (
	"database/sql"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// The parked inventory pack with its selling pack (Product) and, when books are wanted, the
// accounting pack. Stock Manager keeps stock: it has no permission on Product or on the books,
// so every posting it causes proves the script acts with the source record's authority.
const erpInventoryPack = "../../config/_parked/erp-inventory"

func TestPackScriptsInventoryPosting(t *testing.T) {
	eachPackDialect(t, []string{erpsellPack, erpInventoryPack, erpAccountingPack}, func(f *packFixture) {
		f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", "KES", "KES", "KES", true)
		inventoryPostingScenario(f)
	})
}

func TestPackScriptsInventoryPostingExistingAccount(t *testing.T) {
	eachPackDialect(t, []string{erpsellPack, erpInventoryPack, erpAccountingPack}, func(f *packFixture) {
		f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", "KES", "KES", "KES", true)
		f.run("an active account with the Inventory role is used, no standard one is created", func(f *packFixture) {
			shelf := accountingCreate(f.as("Accountant"), "Account", map[string]any{"code": "1240", "account_name": "Stock on Hand", "account_type": "Asset", "posting_role": "Inventory"})
			tea := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 0}))
			adjustment := f.as("Stock Manager").create("Stock Adjustment", jsonBody(f.t, map[string]any{"product": tea, "reason": "Found", "delta": 2}))
			if got := accountingNet(f, shelf, stockEntryOf(f, adjustment)); got != 80 {
				f.t.Fatalf("Stock on Hand net = %v, want 80", got)
			}
			if got := f.count("tabAccount", "code = ?", "1200"); got != 0 {
				f.t.Fatalf("standard Inventory account 1200 created %d times, want 0", got)
			}
		})
	})
}

func TestPackScriptsInventoryPostingWithoutBooks(t *testing.T) {
	eachPackDialect(t, []string{erpsellPack, erpInventoryPack}, func(f *packFixture) {
		f.run("a stock adjustment saves and changes stock when the books are not installed", func(f *packFixture) {
			tea := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 1}))
			f.as("Stock Manager").create("Stock Adjustment", jsonBody(f.t, map[string]any{"product": tea, "reason": "Found", "delta": 3}))
			expectStock(f, tea, 4)
		})
	})
}

// inventoryPostingScenario runs the cases in order; later cases build on the stock of earlier ones.
func inventoryPostingScenario(f *packFixture) {
	var tea string
	manager := f.as("Stock Manager")
	adjust := func(f *packFixture, reason string, delta float64) string {
		f.t.Helper()
		return manager.create("Stock Adjustment", jsonBody(f.t, map[string]any{"product": tea, "reason": reason, "delta": delta}))
	}

	f.run("an increase posts stock at cost and creates the standard accounts", func(f *packFixture) {
		tea = f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 10}))
		found := adjust(f, "Found", 5)
		entry := stockEntryOf(f, found)
		f.expect("Journal Entry", entry, "status", "Posted")
		f.expect("Journal Entry", entry, "memo", "Stock adjustment")
		if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ? AND memo = ?", "Stock Adjustment", found, "Stock adjustment"); got != 1 {
			f.t.Fatalf("entries for %s = %d, want 1", found, got)
		}
		inventory := inventoryAccount(f, "Inventory")
		if got := accountingNet(f, inventory, entry); got != 200 {
			f.t.Fatalf("Inventory net = %v, want 200", got)
		}
		if got := accountingNet(f, inventoryAccount(f, "Stock Adjustments"), entry); got != -200 {
			f.t.Fatalf("Stock Adjustments net = %v, want -200", got)
		}
		if got := f.count("tabAccount", "code = ? AND posting_role = ?", "1200", "Inventory"); got != 1 {
			f.t.Fatalf("standard Inventory account 1200 count = %d, want 1", got)
		}
		expectStock(f, tea, 15)
	})

	f.run("a decrease by reason posts the opposite entry", func(f *packFixture) {
		damaged := adjust(f, "Damaged", 2)
		entry := stockEntryOf(f, damaged)
		if got := accountingNet(f, inventoryAccount(f, "Inventory"), entry); got != -80 {
			f.t.Fatalf("Inventory net = %v, want -80", got)
		}
		if got := accountingNet(f, inventoryAccount(f, "Stock Adjustments"), entry); got != 80 {
			f.t.Fatalf("Stock Adjustments net = %v, want 80", got)
		}
		expectStock(f, tea, 13)
	})

	f.run("a count correction takes the sign typed", func(f *packFixture) {
		correction := adjust(f, "Count correction", -3)
		if got := accountingNet(f, inventoryAccount(f, "Inventory"), stockEntryOf(f, correction)); got != -120 {
			f.t.Fatalf("Inventory net = %v, want -120", got)
		}
		expectStock(f, tea, 10)
	})

	f.run("a product without a cost posts nothing and still changes stock", func(f *packFixture) {
		free := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Sample", "selling_price": 0, "stock_qty": 0}))
		found := manager.create("Stock Adjustment", jsonBody(f.t, map[string]any{"product": free, "reason": "Found", "delta": 4}))
		if got := f.count("tabJournal Entry", "source_reference = ?", found); got != 0 {
			f.t.Fatalf("entries for a zero-value adjustment = %d, want 0", got)
		}
		expectStock(f, free, 4)
	})

	f.run("the adjustment is append-only and cannot be re-saved", func(f *packFixture) {
		found := adjust(f, "Found", 1)
		manager.refused(httpMethodPut, "Stock Adjustment", found, `{"delta":5}`, "append-only records cannot be changed")
		if got := f.count("tabJournal Entry", "source_reference = ?", found); got != 1 {
			f.t.Fatalf("entries after refused re-save = %d, want 1", got)
		}
	})

	f.run("the front-line role cannot edit the product or the books directly", func(f *packFixture) {
		entry := stockEntryOf(f, adjust(f, "Found", 1))
		manager.refused(httpMethodPut, "Product", tea, `{"stock_qty":99}`, "not permitted")
		manager.refused(httpMethodPut, "Journal Entry", entry, `{"status":"Reversed"}`, "not permitted")
	})

	f.run("a closed period refuses the adjustment and changes nothing", func(f *packFixture) {
		now := time.Now().UTC()
		start := now.Format("2006-01") + "-01"
		end := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		f.as("Accountant").create("Accounting Period", jsonBody(f.t, map[string]any{"period_name": now.Format("2006-01"), "start_date": start, "end_date": end, "status": "Closed"}))
		before := f.amount("Product", tea, "stock_qty")
		manager.refused(httpMethodPost, "Stock Adjustment", "", jsonBody(f.t, map[string]any{"product": tea, "reason": "Found", "delta": 1}), "is Closed.")
		if got := f.amount("Product", tea, "stock_qty"); got != before {
			f.t.Fatalf("stock_qty = %v after refused adjustment, want %v", got, before)
		}
	})
}

// inventoryAccount returns the active account that holds role, or fails the case.
func inventoryAccount(f *packFixture, role string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT name FROM " + dl.QuoteIdent("tabAccount") + " WHERE posting_role = ? AND status = ?"
	var name sql.NullString
	if err := f.db.QueryRow(kdb.Rebind(dl, query), role, "Active").Scan(&name); err != nil || !name.Valid {
		f.t.Fatalf("no active account for %s: %v", role, err)
	}
	return name.String
}

// stockEntryOf returns the journal entry posted for a stock adjustment, failing the case when none exists.
func stockEntryOf(f *packFixture, adjustment string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT name FROM " + dl.QuoteIdent("tabJournal Entry") + " WHERE source_reference = ?"
	var name sql.NullString
	if err := f.db.QueryRow(kdb.Rebind(dl, query), adjustment).Scan(&name); err != nil || !name.Valid {
		f.t.Fatalf("no journal entry for %s: %v", adjustment, err)
	}
	return name.String
}

// expectStock fails the case unless the product's stock_qty is want.
func expectStock(f *packFixture, product string, want float64) {
	f.t.Helper()
	if got := f.amount("Product", product, "stock_qty"); got != want {
		f.t.Fatalf("stock_qty of %s = %v, want %v", product, got, want)
	}
}

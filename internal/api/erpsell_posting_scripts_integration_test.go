//go:build integration

package api

import (
	"math"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// Posting to the books (#815). With the accounting pack installed, the selling pack's sales,
// payments, expenses and cash movements post journal entries in the same save. Each case reads
// the entries back by source record and memo, and checks the net movement of every posting role
// since the case began (cases share one database, so absolute balances would depend on order).

// sellPostingRoles are the posting roles the selling pack moves money through.
var sellPostingRoles = []string{"Cash", "Mobile Money", "Bank", "Cash Clearing", "Receivable", "Inventory", "Tax Payable", "Sales", "Cost of Sales", "Expenses"}

// sellPostingSaleBody builds a Sale with one product line, its tax, and optional payment lines.
func sellPostingSaleBody(status, customer, product string, qty, unit, tax float64, payments []any) string {
	body := map[string]any{
		"sale_date": "2026-11-20", "status": status,
		"items": []any{map[string]any{"product": product, "quantity": qty, "unit_price": unit, "tax_amount": tax}},
	}
	if customer != "" {
		body["customer"] = customer
	}
	if payments != nil {
		body["payments"] = payments
	}
	return jsonBodyForCredit(body)
}

// sellPostingBalance is the net debit (debits minus credits) of the accounts matching where,
// over the entries that are part of the books: Posted and Reversed (a reversal nets to zero).
func sellPostingBalance(f *packFixture, where string, args ...any) float64 {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT COALESCE(SUM(l.debit), 0) - COALESCE(SUM(l.credit), 0) FROM " + dl.QuoteIdent("tabJournal Entry__lines") + " l " +
		"JOIN " + dl.QuoteIdent("tabJournal Entry") + " e ON e.name = l.parent " +
		"WHERE e.status IN ('Posted', 'Reversed') AND l.account IN (SELECT name FROM " + dl.QuoteIdent("tabAccount") + " WHERE " + where + ")"
	var out float64
	if err := f.db.QueryRow(kdb.Rebind(dl, query), args...).Scan(&out); err != nil {
		f.t.Fatalf("read balance of %s: %v", where, err)
	}
	return out
}

// sellPostingSnapshot is the net debit of every selling posting role right now.
func sellPostingSnapshot(f *packFixture) map[string]float64 {
	f.t.Helper()
	out := make(map[string]float64, len(sellPostingRoles))
	for _, role := range sellPostingRoles {
		out[role] = sellPostingBalance(f, "posting_role = ?", role)
	}
	return out
}

// sellPostingMoved expects every posting role to have moved since before by exactly the amount
// listed in want (roles not listed must not move at all).
func sellPostingMoved(f *packFixture, before map[string]float64, want map[string]float64) {
	f.t.Helper()
	now := sellPostingSnapshot(f)
	for _, role := range sellPostingRoles {
		if got := now[role] - before[role]; math.Abs(got-want[role]) > 0.005 {
			f.t.Fatalf("%s moved by %.2f, want %.2f", role, got, want[role])
		}
	}
}

// sellPostingEntry returns the one entry a source event posted (Posted or Reversed) and fails
// when there is not exactly one.
func sellPostingEntry(f *packFixture, source, sourceName, memo string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT name FROM " + dl.QuoteIdent("tabJournal Entry") +
		" WHERE source_type = ? AND source_reference = ? AND memo = ? AND status IN ('Posted', 'Reversed')"
	rows, err := f.db.Query(kdb.Rebind(dl, query), source, sourceName, memo)
	if err != nil {
		f.t.Fatalf("find %s %s entry: %v", source, memo, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			f.t.Fatalf("scan entry: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read entries: %v", err)
	}
	if len(names) != 1 {
		f.t.Fatalf("%s %s %q has %d entries, want 1", source, sourceName, memo, len(names))
	}
	return names[0]
}

// sellPostingNoEntry expects a source record to have no entry at all.
func sellPostingNoEntry(f *packFixture, source, sourceName string) {
	f.t.Helper()
	if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ?", source, sourceName); got != 0 {
		f.t.Fatalf("%s %s has %d entries, want none", source, sourceName, got)
	}
}

// sellPostingAccount returns the name of the active account used for role.
func sellPostingAccount(f *packFixture, role string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	var name string
	if err := f.db.QueryRow(kdb.Rebind(dl, "SELECT name FROM "+dl.QuoteIdent("tabAccount")+" WHERE posting_role = ?"), role).Scan(&name); err != nil {
		f.t.Fatalf("find account for %s: %v", role, err)
	}
	return name
}

// sellPostingBalanced expects an entry's debits to equal its credits, to the cent.
func sellPostingBalanced(f *packFixture, entry string) {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT COALESCE(SUM(debit), 0) - COALESCE(SUM(credit), 0) FROM " + dl.QuoteIdent("tabJournal Entry__lines") + " WHERE parent = ?"
	var diff float64
	if err := f.db.QueryRow(kdb.Rebind(dl, query), entry).Scan(&diff); err != nil {
		f.t.Fatalf("read entry %s lines: %v", entry, err)
	}
	if math.Abs(diff) > 0.005 {
		f.t.Fatalf("entry %s is out of balance by %.2f", entry, diff)
	}
}

// sellPostingCurrencies seeds the currencies the posting cases pay in.
func sellPostingCurrencies(f *packFixture) {
	f.t.Helper()
	f.create("Currency", jsonBody(f.t, map[string]any{"code": "USD", "currency_name": "US Dollar", "rate_to_base": 130}))
	f.create("Currency", jsonBody(f.t, map[string]any{"code": "EUR", "currency_name": "Euro", "rate_to_base": 100}))
}

func TestPackScriptsSellingPosting(t *testing.T) {
	eachPackDialect(t, []string{creditPackDir, erpAccountingPack}, func(f *packFixture) {
		sellPostingCurrencies(f)

		f.run("a register sale created as Paid posts its money, revenue, tax and cost once", func(f *packFixture) {
			customer, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			sale := f.create("Sale", sellPostingSaleBody("Paid", customer, product, 3, 100, 30, []any{
				map[string]any{"method": "Cash", "amount": 330},
			}))
			entry := sellPostingEntry(f, "Sale", sale, "Sale")
			f.expect("Journal Entry", entry, "status", "Posted")
			sellPostingBalanced(f, entry)
			sellPostingMoved(f, before, map[string]float64{"Cash": 330, "Sales": -300, "Tax Payable": -30, "Cost of Sales": 120, "Inventory": -120})

			f.update("Sale", sale, `{"notes":"checked"}`)
			if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ? AND memo = ?", "Sale", sale, "Sale"); got != 1 {
				f.t.Fatalf("re-saving a Paid sale left %d entries, want 1", got)
			}
			sellPostingMoved(f, before, map[string]float64{"Cash": 330, "Sales": -300, "Tax Payable": -30, "Cost of Sales": 120, "Inventory": -120})
		})

		f.run("a held sale paid at the till posts on the paid update only", func(f *packFixture) {
			_, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			held := f.create("Sale", sellPostingSaleBody("Held", "", product, 2, 100, 0, nil))
			sellPostingNoEntry(f, "Sale", held)
			f.update("Sale", held, `{"status":"Paid","payments":[{"method":"Cash","amount":200}]}`)
			sellPostingEntry(f, "Sale", held, "Sale")
			sellPostingMoved(f, before, map[string]float64{"Cash": 200, "Sales": -200, "Cost of Sales": 80, "Inventory": -80})
		})

		f.run("a draft turned Paid by a person posts once, and a Paid sale cannot be paid twice", func(f *packFixture) {
			_, product := creditSeed(f)
			draft := f.create("Sale", sellPostingSaleBody("Draft", "", product, 1, 100, 0, nil))
			sellPostingNoEntry(f, "Sale", draft)
			f.refused(httpMethodPut, "Sale", draft, `{"status":"Paid"}`, "add up to 0, not the sale total 100")
			f.update("Sale", draft, `{"status":"Paid","payments":[{"method":"Cash","amount":100}]}`)
			sellPostingEntry(f, "Sale", draft, "Sale")
			f.update("Sale", draft, `{"status":"Paid"}`)
			if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ? AND memo = ?", "Sale", draft, "Sale"); got != 1 {
				f.t.Fatalf("a second Paid save left %d entries, want 1", got)
			}
		})

		f.run("payment lines that do not add up to the total refuse the sale and post nothing", func(f *packFixture) {
			customer, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			f.refused(httpMethodPost, "Sale", "", sellPostingSaleBody("Paid", customer, product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 250},
			}), "add up to 250, not the sale total 300")
			sellPostingMoved(f, before, map[string]float64{})
			if got := f.amount("Product", product, "stock_qty"); got != 10 {
				f.t.Fatalf("stock after a refused sale = %v, want 10", got)
			}
		})

		f.run("a payment in another currency converts at its rate and a rounding cent goes to the largest line", func(f *packFixture) {
			_, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			sale := f.create("Sale", sellPostingSaleBody("Paid", "", product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 40},
				map[string]any{"method": "Card", "amount": 2, "currency": "USD"},
			}))
			sellPostingEntry(f, "Sale", sale, "Sale")
			sellPostingMoved(f, before, map[string]float64{"Cash": 40, "Bank": 260, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})

			_, product = creditSeed(f)
			before = sellPostingSnapshot(f)
			f.create("Sale", sellPostingSaleBody("Paid", "", product, 3, 100, 0, []any{
				map[string]any{"method": "Card", "amount": 1.333, "currency": "EUR"},
				map[string]any{"method": "Cash", "amount": 166.69},
			}))
			sellPostingMoved(f, before, map[string]float64{"Cash": 166.70, "Bank": 133.30, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})
		})

		f.run("voiding a register sale reverses its entry", func(f *packFixture) {
			customer, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			sale := f.create("Sale", sellPostingSaleBody("Paid", customer, product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 300},
			}))
			f.update("Sale", sale, `{"status":"Void"}`)
			// The original entry is Reversed and its reversal Posted: together they net to nothing.
			sellPostingMoved(f, before, map[string]float64{})
			f.expect("Journal Entry", sellPostingEntry(f, "Sale", sale, "Sale"), "status", "Reversed")
		})

		f.run("a credit sale posts its receivable, a payment settles it and voiding the payment reverses it", func(f *packFixture) {
			customer, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			sale := f.create("Sale", sellPostingSaleBody("Awaiting Payment", customer, product, 3, 100, 0, nil))
			sellPostingEntry(f, "Sale", sale, "Sale on credit")
			sellPostingMoved(f, before, map[string]float64{"Receivable": 300, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})

			cash := f.create("Payment", creditPaymentBody(sale, "Received", 100, map[string]any{"method": "Cash"}))
			entry := sellPostingEntry(f, "Payment", cash, "Payment")
			sellPostingBalanced(f, entry)
			sellPostingMoved(f, before, map[string]float64{"Receivable": 200, "Cash": 100, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})

			f.create("Payment", creditPaymentBody(sale, "Received", 50, map[string]any{"method": "Card"}))
			sellPostingMoved(f, before, map[string]float64{"Receivable": 150, "Cash": 100, "Bank": 50, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})

			f.update("Payment", cash, `{"status":"Voided"}`)
			f.expect("Journal Entry", entry, "status", "Reversed")
			sellPostingMoved(f, before, map[string]float64{"Receivable": 250, "Bank": 50, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})
		})

		f.run("cancelling a credit sale without payments reverses its entry", func(f *packFixture) {
			customer, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			sale := f.create("Sale", sellPostingSaleBody("Awaiting Payment", customer, product, 3, 100, 0, nil))
			f.update("Sale", sale, `{"status":"Cancelled"}`)
			sellPostingMoved(f, before, map[string]float64{})
			f.expect("Journal Entry", sellPostingEntry(f, "Sale", sale, "Sale on credit"), "status", "Reversed")
		})

		f.run("a payment on a register sale posts nothing", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", sellPostingSaleBody("Paid", customer, product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 300},
			}))
			payment := f.create("Payment", creditPaymentBody(sale, "Received", 50, nil))
			sellPostingNoEntry(f, "Payment", payment)
		})

		f.run("a return posts the opposite of its sale, refunded from its rows or in cash", func(f *packFixture) {
			_, product := creditSeed(f)
			sale := f.create("Sale", sellPostingSaleBody("Paid", "", product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 300},
			}))
			before := sellPostingSnapshot(f)
			ret := f.create("Sale", sellPostingSaleBody("Return", "", product, 1, 100, 0, []any{
				map[string]any{"method": "Card", "amount": 100},
			}))
			sellPostingEntry(f, "Sale", ret, "Return")
			sellPostingMoved(f, before, map[string]float64{"Bank": -100, "Sales": 100, "Cost of Sales": -40, "Inventory": 40})

			before = sellPostingSnapshot(f)
			cashReturn := f.create("Sale", sellPostingSaleBody("Return", "", product, 1, 100, 0, nil))
			sellPostingEntry(f, "Sale", cashReturn, "Return")
			sellPostingMoved(f, before, map[string]float64{"Cash": -100, "Sales": 100, "Cost of Sales": -40, "Inventory": 40})
			sellPostingEntry(f, "Sale", sale, "Sale")
		})

		f.run("a credit sale and a payment post through a Cashier, who cannot write the books by hand", func(f *packFixture) {
			customer, product := creditSeed(f)
			cashier := f.as("Cashier")
			before := sellPostingSnapshot(f)
			sale := cashier.create("Sale", sellPostingSaleBody("Awaiting Payment", customer, product, 3, 100, 0, nil))
			cashier.create("Payment", creditPaymentBody(sale, "Received", 100, map[string]any{"method": "Cash"}))
			sellPostingMoved(f, before, map[string]float64{"Receivable": 200, "Cash": 100, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})
			if got := f.count("tabAccount", "posting_role = ?", "Receivable"); got != 1 {
				f.t.Fatalf("accounts for Receivable = %d, want 1", got)
			}

			code, _, doc := cashier.send(httpMethodPost, "Journal Entry", "", jsonBody(f.t, map[string]any{
				"posting_date": "2026-11-20", "status": "Posted", "memo": "by hand", "lines": []any{
					map[string]any{"account": sellPostingAccount(f, "Cash"), "debit": 10, "credit": 0},
					map[string]any{"account": sellPostingAccount(f, "Sales"), "debit": 0, "credit": 10},
				},
			}))
			if doc != nil {
				f.t.Fatalf("Cashier wrote a journal entry by hand (status %d)", code)
			}
		})

		f.run("an expense posts once when recorded and not again when edited", func(f *packFixture) {
			before := sellPostingSnapshot(f)
			expense := f.create("Expense", jsonBody(f.t, map[string]any{
				"expense_date": "2026-11-20", "payee": "Landlord", "category": "Rent", "reason": "Shop rent",
				"payment_method": "Card", "amount": 500,
			}))
			sellPostingEntry(f, "Expense", expense, "Expense")
			sellPostingMoved(f, before, map[string]float64{"Expenses": 500, "Bank": -500})
			f.update("Expense", expense, `{"amount":600}`)
			if got := f.count("tabJournal Entry", "source_type = ? AND source_reference = ?", "Expense", expense); got != 1 {
				f.t.Fatalf("expense edited into %d entries, want 1", got)
			}
		})

		f.run("cash movements post cash in and out, and a safe drop posts nothing", func(f *packFixture) {
			till := f.create("Till Session", jsonBody(f.t, map[string]any{"status": "Open", "register_code": "REG-001"}))
			before := sellPostingSnapshot(f)
			in := f.create("Cash Movement", jsonBody(f.t, map[string]any{"till_session": till, "movement_type": "Cash in", "amount": 200, "reason": "Float"}))
			out := f.create("Cash Movement", jsonBody(f.t, map[string]any{"till_session": till, "movement_type": "Cash out", "amount": 50, "reason": "Bank drop"}))
			safe := f.create("Cash Movement", jsonBody(f.t, map[string]any{"till_session": till, "movement_type": "Safe drop", "amount": 100, "reason": "Safe"}))
			sellPostingEntry(f, "Cash Movement", in, "Cash movement")
			sellPostingEntry(f, "Cash Movement", out, "Cash movement")
			sellPostingNoEntry(f, "Cash Movement", safe)
			sellPostingMoved(f, before, map[string]float64{"Cash": 150, "Cash Clearing": -150})
		})

		f.run("one active account per role, created with the standard code on first use", func(f *packFixture) {
			standard := map[string]string{"Cash": "1000", "Bank": "1020", "Cash Clearing": "1090", "Receivable": "1100", "Inventory": "1200", "Tax Payable": "2200", "Sales": "4000", "Cost of Sales": "5000", "Expenses": "6000"}
			for role, code := range standard {
				if got := f.count("tabAccount", "posting_role = ?", role); got != 1 {
					f.t.Fatalf("accounts for %s = %d, want 1", role, got)
				}
				if got := f.count("tabAccount", "posting_role = ? AND code = ?", role, code); got != 1 {
					f.t.Fatalf("%s is not on standard code %s", role, code)
				}
			}
		})

		f.run("an existing account with the role is used instead of creating the standard one", func(f *packFixture) {
			wallet := f.create("Account", jsonBody(f.t, map[string]any{
				"code": "1500", "account_name": "Mobile wallet", "account_type": "Asset", "posting_role": "Mobile Money",
			}))
			_, product := creditSeed(f)
			before := sellPostingSnapshot(f)
			f.create("Sale", sellPostingSaleBody("Paid", "", product, 3, 100, 0, []any{
				map[string]any{"method": "Mobile Money", "amount": 300},
			}))
			if got := sellPostingBalance(f, "name = ?", wallet); math.Abs(got-300) > 0.005 {
				f.t.Fatalf("wallet moved by %.2f, want 300", got)
			}
			sellPostingMoved(f, before, map[string]float64{"Mobile Money": 300, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})
			if got := f.count("tabAccount", "code = ?", "1010"); got != 0 {
				f.t.Fatalf("standard Mobile Money account was created although one exists")
			}
		})

		// The closed period comes last: it covers the sale date every case above posts on.
		f.run("a closed period refuses a sale and leaves no entry and no stock change", func(f *packFixture) {
			customer, product := creditSeed(f)
			f.create("Accounting Period", jsonBody(f.t, map[string]any{
				"period_name": "2026-11", "start_date": "2026-11-01", "end_date": "2026-11-30", "status": "Closed",
			}))
			f.refused(httpMethodPost, "Sale", "", sellPostingSaleBody("Paid", customer, product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 300},
			}), "is Closed")
			if got := f.amount("Product", product, "stock_qty"); got != 10 {
				f.t.Fatalf("stock after a refused sale = %v, want 10", got)
			}
		})

	})
}

// TestPackScriptsSellingPostingWithoutBooks proves the posting scripts stay out of the way when
// the accounting pack is not installed: every event still saves, and nothing needs the books.
func TestPackScriptsSellingPostingWithoutBooks(t *testing.T) {
	eachPackDialect(t, []string{creditPackDir}, func(f *packFixture) {
		f.run("every posting event saves when the books are not installed", func(f *packFixture) {
			customer, product := creditSeed(f)
			f.create("Sale", sellPostingSaleBody("Paid", "", product, 3, 100, 0, nil))
			credit := f.create("Sale", sellPostingSaleBody("Awaiting Payment", customer, product, 1, 100, 0, nil))
			f.create("Payment", creditPaymentBody(credit, "Received", 50, map[string]any{"method": "Card"}))
			f.create("Expense", jsonBody(f.t, map[string]any{
				"expense_date": "2026-11-20", "payee": "Landlord", "category": "Rent", "reason": "Shop rent", "amount": 500,
			}))
			till := f.create("Till Session", jsonBody(f.t, map[string]any{"status": "Open", "register_code": "REG-001"}))
			f.create("Cash Movement", jsonBody(f.t, map[string]any{"till_session": till, "movement_type": "Cash in", "amount": 200, "reason": "Float"}))
			ret := f.create("Sale", sellPostingSaleBody("Return", "", product, 1, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 100},
			}))
			f.expect("Sale", ret, "status", "Return")
			if got := f.amount("Product", product, "stock_qty"); got != 7 {
				f.t.Fatalf("stock = %v, want 7", got)
			}
		})
	})
}

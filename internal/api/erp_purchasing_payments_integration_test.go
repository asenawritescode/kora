//go:build integration

package api

import (
	"strconv"
	"testing"
)

// The parked purchasing pack (and the selling pack it needs) loaded as shipped, with its scripts
// run the way the Engine runs them. A supplier payment allocates to supplier bills; a bill's paid
// amount and status follow its payments.
const (
	erpPurchasingPack = "../../config/_parked/erp-purchasing"
	erpSellingPack    = "../../config/_parked/erpsell"
)

// erpPaymentsFixture is the pack fixture with both packs and their scripts installed.
type erpPaymentsFixture struct{ *packFixture }

func TestPackScriptsPurchasingPayments(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, erpPurchasingPack}, func(f *packFixture) {
		for _, currency := range []string{"KES", "USD"} {
			f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", currency, currency, currency, true)
		}
		(&erpPaymentsFixture{f}).runScenario()
	})
}

// rejected reports whether saving the record is refused.
func (f *erpPaymentsFixture) rejected(method resourceHTTPMethod, recordType, name, body string) bool {
	f.t.Helper()
	_, _, doc := f.send(method, recordType, name, body)
	return doc == nil
}

// allocationRows counts the allocation rows a payment holds.
func (f *erpPaymentsFixture) allocationRows(payment string) int {
	f.t.Helper()
	return f.count("tabSupplier Payment__allocations", "parent = ?", payment)
}

// expectBill checks the three values a person sees on a bill.
func (f *erpPaymentsFixture) expectBill(bill, status string, paid, outstanding float64) {
	f.t.Helper()
	if got := f.value("Supplier Bill", bill, "status"); got != status {
		f.t.Fatalf("bill %s status = %q, want %q", bill, got, status)
	}
	if got := f.amount("Supplier Bill", bill, "amount_paid"); got != paid {
		f.t.Fatalf("bill %s amount paid = %v, want %v", bill, got, paid)
	}
	if got := f.amount("Supplier Bill", bill, "outstanding"); got != outstanding {
		f.t.Fatalf("bill %s outstanding = %v, want %v", bill, got, outstanding)
	}
}

func (f *erpPaymentsFixture) runScenario() {
	t := f.t
	supplier := f.create("Supplier", `{"supplier_name":"Dar Traders"}`)
	otherSupplier := f.create("Supplier", `{"supplier_name":"Mombasa Paper"}`)
	bill := func(reference, who, amount, currency string) string {
		body := `{"reference":"` + reference + `","supplier":"` + who + `","bill_date":"2026-11-13","amount":` + amount + `,"status":"Open"`
		if currency != "" {
			body += `,"currency":"` + currency + `"`
		}
		return f.create("Supplier Bill", body+`}`)
	}
	payment := func(reference, amount string, allocations, shortcut string) string {
		body := `{"reference":"` + reference + `","payment_date":"2026-11-14","supplier":"` + supplier + `","amount":` + amount
		if allocations != "" {
			body += `,"allocations":[` + allocations + `]`
		}
		if shortcut != "" {
			body += `,"supplier_bill":"` + shortcut + `"`
		}
		return f.create("Supplier Payment", body+`}`)
	}
	alloc := func(bill string, amount string) string {
		return `{"supplier_bill":"` + bill + `","amount":` + amount + `}`
	}

	var b1, p1, p2 string
	t.Run("new bill is open with nothing paid", func(t *testing.T) {
		b1 = bill("BILL-1", supplier, "1000", "")
		f.expectBill(b1, "Open", 0, 1000)
	})
	t.Run("partial payment leaves the bill partly paid", func(t *testing.T) {
		p1 = payment("PAY-1", "300", alloc(b1, "300"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", p1, `{"status":"Paid"}`) {
			t.Fatal("paying an open bill was refused")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
		if got := f.amount("Supplier Payment", p1, "unallocated_amount"); got != 0 {
			t.Fatalf("unallocated amount = %v, want 0", got)
		}
	})
	t.Run("payment for the rest makes the bill paid", func(t *testing.T) {
		p2 = payment("PAY-2", "700", alloc(b1, "700"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", p2, `{"status":"Paid"}`) {
			t.Fatal("paying the rest was refused")
		}
		f.expectBill(b1, "Paid", 1000, 0)
	})
	t.Run("voiding a payment reverses exactly its allocation", func(t *testing.T) {
		if f.rejected(httpMethodPut, "Supplier Payment", p2, `{"status":"Voided"}`) {
			t.Fatal("voiding a paid payment was refused")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
		if got := f.amount("Supplier Payment", p2, "unallocated_amount"); got != 0 {
			t.Fatalf("voided payment unallocated amount = %v, want 0", got)
		}
	})
	t.Run("money beyond the listed bills stays as supplier credit", func(t *testing.T) {
		b2 := bill("BILL-2", supplier, "200", "")
		p3 := payment("PAY-3", "500", alloc(b2, "200"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", p3, `{"status":"Paid"}`) {
			t.Fatal("payment with credit was refused")
		}
		f.expectBill(b2, "Paid", 200, 0)
		if got := f.amount("Supplier Payment", p3, "unallocated_amount"); got != 300 {
			t.Fatalf("supplier credit = %v, want 300", got)
		}
	})
	t.Run("supplier bill shortcut pays up to outstanding and records a row", func(t *testing.T) {
		b3 := bill("BILL-3", supplier, "1000", "")
		p4 := payment("PAY-4", "1500", "", b3)
		if f.rejected(httpMethodPut, "Supplier Payment", p4, `{"status":"Paid"}`) {
			t.Fatal("shortcut payment was refused")
		}
		f.expectBill(b3, "Paid", 1000, 0)
		if rows := f.allocationRows(p4); rows != 1 {
			t.Fatalf("shortcut recorded %d allocation rows, want 1", rows)
		}
		if got := f.amount("Supplier Payment", p4, "unallocated_amount"); got != 500 {
			t.Fatalf("shortcut supplier credit = %v, want 500", got)
		}
	})
	t.Run("allocating to another supplier's bill is refused", func(t *testing.T) {
		b4 := bill("BILL-4", otherSupplier, "100", "")
		p5 := payment("PAY-5", "50", alloc(b4, "50"), "")
		if !f.rejected(httpMethodPut, "Supplier Payment", p5, `{"status":"Paid"}`) {
			t.Fatal("payment to the wrong supplier's bill was accepted")
		}
		f.expectBill(b4, "Open", 0, 100)
	})
	t.Run("allocating in another currency is refused", func(t *testing.T) {
		b5 := bill("BILL-5", supplier, "100", "USD")
		p6 := payment("PAY-6", "100", alloc(b5, "100"), "")
		if !f.rejected(httpMethodPut, "Supplier Payment", p6, `{"status":"Paid"}`) {
			t.Fatal("payment in another currency was accepted")
		}
		f.expectBill(b5, "Open", 0, 100)
	})
	t.Run("over-allocation is refused", func(t *testing.T) {
		b6 := bill("BILL-6", supplier, "200", "")
		over := []struct{ amount, allocations string }{
			{"300", alloc(b6, "250")},                          // more than the bill's outstanding
			{"300", alloc(b6, "150") + "," + alloc(b6, "100")}, // two rows on one bill beyond outstanding
			{"100", alloc(b6, "150")},                          // more than the payment
		}
		for i, tc := range over {
			p := payment("PAY-OVER-"+strconv.Itoa(i+1), tc.amount, tc.allocations, "")
			if !f.rejected(httpMethodPut, "Supplier Payment", p, `{"status":"Paid"}`) {
				t.Fatalf("over-allocation %s accepted", tc.allocations)
			}
		}
		f.expectBill(b6, "Open", 0, 200)
	})
	t.Run("a person cannot mark an unpaid bill paid", func(t *testing.T) {
		b7 := bill("BILL-7", supplier, "100", "")
		f.refused(httpMethodPut, "Supplier Bill", b7, `{"status":"Paid"}`, "A bill becomes Partly Paid or Paid by recording a Supplier Payment.")
		f.expect("Supplier Bill", b7, "status", "Open")
		f.expectBill(b7, "Open", 0, 100)
	})
	t.Run("a direct edit of amount paid is ignored", func(t *testing.T) {
		b8 := bill("BILL-8", supplier, "100", "")
		_ = f.rejected(httpMethodPut, "Supplier Bill", b8, `{"amount_paid":50}`)
		f.expectBill(b8, "Open", 0, 100)
	})
	t.Run("a bill with payments cannot be cancelled or reopened by hand", func(t *testing.T) {
		if !f.rejected(httpMethodPut, "Supplier Bill", b1, `{"status":"Cancelled"}`) {
			t.Fatal("bill with payments was cancelled")
		}
		if !f.rejected(httpMethodPut, "Supplier Bill", b1, `{"status":"Open"}`) {
			t.Fatal("bill with payments was reopened by hand")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
	})
	t.Run("a payment amount is fixed once paid", func(t *testing.T) {
		if !f.rejected(httpMethodPut, "Supplier Payment", p1, `{"amount":999}`) {
			t.Fatal("a paid payment's amount was changed")
		}
	})
	t.Run("voiding back to zero reopens the bill as open", func(t *testing.T) {
		b9 := bill("BILL-9", supplier, "300", "")
		p9 := payment("PAY-9", "300", alloc(b9, "300"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", p9, `{"status":"Paid"}`) {
			t.Fatal("payment was refused")
		}
		f.expectBill(b9, "Paid", 300, 0)
		if f.rejected(httpMethodPut, "Supplier Payment", p9, `{"status":"Voided"}`) {
			t.Fatal("voiding was refused")
		}
		f.expectBill(b9, "Open", 0, 300)
	})
	t.Run("an unpaid bill can be cancelled", func(t *testing.T) {
		b10 := bill("BILL-10", supplier, "100", "")
		if f.rejected(httpMethodPut, "Supplier Bill", b10, `{"status":"Cancelled"}`) {
			t.Fatal("cancelling an unpaid bill was refused")
		}
		if got := f.value("Supplier Bill", b10, "status"); got != "Cancelled" {
			t.Fatalf("status = %q, want Cancelled", got)
		}
	})
	t.Run("payments in parts add up exactly", func(t *testing.T) {
		b11 := bill("BILL-11", supplier, "1000", "")
		first := payment("PAY-11A", "300", alloc(b11, "300"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", first, `{"status":"Paid"}`) {
			t.Fatal("first payment was refused")
		}
		second := payment("PAY-11B", "200", alloc(b11, "200"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", second, `{"status":"Paid"}`) {
			t.Fatal("second payment was refused")
		}
		f.expectBill(b11, "Partly Paid", 500, 500)
	})
	t.Run("a paid payment cannot be deleted; a draft one can", func(t *testing.T) {
		b12 := bill("BILL-12", supplier, "100", "")
		paid := payment("PAY-12A", "100", alloc(b12, "100"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", paid, `{"status":"Paid"}`) {
			t.Fatal("payment was refused")
		}
		if !f.rejected(httpMethodDelete, "Supplier Payment", paid, "") {
			t.Fatal("a paid payment was deleted")
		}
		if got := f.value("Supplier Payment", paid, "status"); got != "Paid" {
			t.Fatalf("status after refused delete = %q, want Paid", got)
		}
		f.expectBill(b12, "Paid", 100, 0)
		draft := payment("PAY-12B", "50", alloc(b12, "50"), "")
		if f.rejected(httpMethodDelete, "Supplier Payment", draft, "") {
			t.Fatal("a draft payment could not be deleted")
		}
	})
	t.Run("a voided payment can be deleted", func(t *testing.T) {
		b13 := bill("BILL-13", supplier, "100", "")
		p := payment("PAY-13", "100", alloc(b13, "100"), "")
		if f.rejected(httpMethodPut, "Supplier Payment", p, `{"status":"Paid"}`) {
			t.Fatal("payment was refused")
		}
		if f.rejected(httpMethodPut, "Supplier Payment", p, `{"status":"Voided"}`) {
			t.Fatal("voiding was refused")
		}
		if f.rejected(httpMethodDelete, "Supplier Payment", p, "") {
			t.Fatal("a voided payment could not be deleted")
		}
	})
	t.Run("a bill with payments cannot be deleted; an unpaid one can", func(t *testing.T) {
		if !f.rejected(httpMethodDelete, "Supplier Bill", b1, "") {
			t.Fatal("bill with payments was deleted")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
		b14 := bill("BILL-14", supplier, "100", "")
		if f.rejected(httpMethodDelete, "Supplier Bill", b14, "") {
			t.Fatal("unpaid bill could not be deleted")
		}
	})
	t.Run("a payment created as paid applies at once", func(t *testing.T) {
		b15 := bill("BILL-15", supplier, "100", "")
		body := `{"reference":"PAY-15","payment_date":"2026-11-14","supplier":"` + supplier + `","amount":100,"status":"Paid","allocations":[` + alloc(b15, "100") + `]}`
		created := f.create("Supplier Payment", body)
		f.expectBill(b15, "Paid", 100, 0)
		if got := f.value("Supplier Payment", created, "status"); got != "Paid" {
			t.Fatalf("status = %q, want Paid", got)
		}
	})
	t.Run("a payment created as paid with a bad row changes no bill", func(t *testing.T) {
		good := bill("BILL-16", supplier, "100", "")
		badBill := bill("BILL-17", otherSupplier, "100", "")
		body := `{"reference":"PAY-16","payment_date":"2026-11-14","supplier":"` + supplier + `","amount":60,"status":"Paid","allocations":[` +
			alloc(good, "50") + `,` + alloc(badBill, "10") + `]}`
		if !f.rejected(httpMethodPost, "Supplier Payment", "", body) {
			t.Fatal("payment with a bill of another supplier was accepted")
		}
		f.expectBill(good, "Open", 0, 100)
		if rows := f.count("tabSupplier Payment", "reference = ?", "PAY-16"); rows != 0 {
			t.Fatalf("refused payment left %d rows", rows)
		}
	})
	t.Run("the purchasing manager pays a bill with the pack's own permissions", func(t *testing.T) {
		manager := f.as("Purchasing Manager")
		b19 := manager.create("Supplier Bill", `{"reference":"BILL-19","supplier":"`+supplier+`","bill_date":"2026-11-13","amount":100,"status":"Open"}`)
		p19 := manager.create("Supplier Payment", `{"reference":"PAY-19","payment_date":"2026-11-14","supplier":"`+supplier+`","amount":100,"allocations":[`+alloc(b19, "100")+`]}`)
		manager.update("Supplier Payment", p19, `{"status":"Paid"}`)
		f.expectBill(b19, "Paid", 100, 0)
	})
	t.Run("a payment cannot be created as voided", func(t *testing.T) {
		b18 := bill("BILL-18", supplier, "100", "")
		body := `{"reference":"PAY-18","payment_date":"2026-11-14","supplier":"` + supplier + `","amount":100,"status":"Voided","allocations":[` + alloc(b18, "100") + `]}`
		if !f.rejected(httpMethodPost, "Supplier Payment", "", body) {
			t.Fatal("payment created as voided was accepted")
		}
		f.expectBill(b18, "Open", 0, 100)
	})
}

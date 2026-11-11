//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Subscriptions pack as shipped, on real MySQL: a subscription adds up its
// extra items, an invoice works out tax, total, balance and payment state, a
// payment lands on its invoice in the same step and cannot be more than the
// balance, and usage stays within an entitlement's allowance.
func TestSubscriptionsBillingWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "subscriptions")
	c := newPackClient(t, handler, db, reg, site)

	plan := c.create("Plan", `{"plan_code":"TERM","plan_name":"Term fees","billing_interval":"Quarterly","unit_price":1000,"currency":"KES"}`)
	subscriber := c.create("Subscriber", `{"subscriber_code":"S-1","subscriber_name":"Wanjiru Kamau","email":"wanjiru@example.com"}`)
	if !c.refused(httpMethodPost, "Subscriber", "", `{"subscriber_code":"S-1","subscriber_name":"Other"}`) {
		t.Fatal("a second subscriber with the same code was accepted")
	}

	base := `"reference":"SUB-1","subscriber":"` + subscriber + `","plan":"` + plan + `","unit_price":1000,"quantity":2,"start_date":"2026-12-01","next_billing_date":"2026-12-01"`
	sub := c.create("Subscription", `{`+base+`,"lines":[{"description":"Transport","quantity":1,"unit_price":300},{"description":"Meals","quantity":2,"unit_price":150.5}]}`)
	if lines, amount := c.value("Subscription", sub, "lines_total"), c.value("Subscription", sub, "recurring_amount"); !strings.HasPrefix(lines, "601") || !strings.HasPrefix(amount, "2601") {
		t.Fatalf("subscription adds up to extra items %q and amount per period %q, want 601 and 2601", lines, amount)
	}
	if got := c.value("Subscription", sub, "status"); got != "Quoted" {
		t.Fatalf("new subscription status = %q, want Quoted", got)
	}
	if !c.refused(httpMethodPost, "Subscription", "", `{"reference":"SUB-BAD","subscriber":"`+subscriber+`","plan":"`+plan+`","unit_price":1000,"start_date":"2026-12-05","next_billing_date":"2026-12-05","end_date":"2026-12-01"}`) {
		t.Fatal("a subscription that ends before it starts was accepted")
	}
	if !c.refused(httpMethodPut, "Subscription", sub, `{"status":"Paused"}`) {
		t.Fatal("a subscription was paused without a reason")
	}
	if c.refused(httpMethodPut, "Subscription", sub, `{"status":"Paused","exception_reason":"Holiday"}`) {
		t.Fatal("pausing with a reason was refused")
	}
	if c.refused(httpMethodPut, "Subscription", sub, `{"status":"Active"}`) {
		t.Fatal("reactivating was refused")
	}

	invoice := c.create("Recurring Invoice", `{"invoice_number":"INV-1","subscription":"`+sub+`","issue_date":"2026-12-01","due_date":"2026-12-15","subtotal":1000,"tax_rate":16}`)
	if tax, total, status := c.value("Recurring Invoice", invoice, "tax_amount"), c.value("Recurring Invoice", invoice, "total"), c.value("Recurring Invoice", invoice, "payment_status"); !strings.HasPrefix(tax, "160") || !strings.HasPrefix(total, "1160") || status != "Unpaid" {
		t.Fatalf("invoice tax %q total %q payment state %q, want 160, 1160, Unpaid", tax, total, status)
	}
	if got := c.value("Recurring Invoice", invoice, "subscriber"); got != subscriber {
		t.Fatalf("invoice subscriber = %q, want %q", got, subscriber)
	}
	payment := func(ref, amount string) string {
		return `{"payment_reference":"` + ref + `","invoice":"` + invoice + `","amount":` + amount + `,"paid_at":"2026-12-02 10:00:00","method":"Cash"}`
	}
	if !c.refused(httpMethodPost, "Subscription Payment", "", payment("P-DRAFT", "100")) {
		t.Fatal("a payment against a draft invoice was accepted")
	}
	if c.refused(httpMethodPut, "Recurring Invoice", invoice, `{"status":"Issued"}`) {
		t.Fatal("issuing the invoice was refused")
	}
	if got := c.value("Recurring Invoice", invoice, "balance"); !strings.HasPrefix(got, "1160") {
		t.Fatalf("balance of an issued invoice = %q, want 1160", got)
	}
	if !c.refused(httpMethodPut, "Recurring Invoice", invoice, `{"subtotal":900}`) {
		t.Fatal("an issued invoice was repriced")
	}

	first := c.create("Subscription Payment", payment("P-1", "500"))
	if paid, balance, state := c.value("Recurring Invoice", invoice, "amount_paid"), c.value("Recurring Invoice", invoice, "balance"), c.value("Recurring Invoice", invoice, "payment_status"); !strings.HasPrefix(paid, "500") || !strings.HasPrefix(balance, "660") || state != "Partially Paid" {
		t.Fatalf("after 500: paid %q balance %q state %q, want 500, 660, Partially Paid", paid, balance, state)
	}
	if !c.refused(httpMethodPost, "Subscription Payment", "", payment("P-OVER", "700")) {
		t.Fatal("a payment above the balance was accepted")
	}
	if got := c.value("Recurring Invoice", invoice, "amount_paid"); !strings.HasPrefix(got, "500") {
		t.Fatalf("amount paid after a refused payment = %q, want 500", got)
	}
	if !c.refused(httpMethodPut, "Subscription Payment", first, `{"amount":50}`) {
		t.Fatal("a saved payment was edited")
	}
	c.create("Subscription Payment", payment("P-2", "660"))
	if balance, state := c.value("Recurring Invoice", invoice, "balance"), c.value("Recurring Invoice", invoice, "payment_status"); !strings.HasPrefix(balance, "0") || state != "Paid" {
		t.Fatalf("after full payment: balance %q state %q, want 0 and Paid", balance, state)
	}

	paidInvoice := c.create("Recurring Invoice", `{"invoice_number":"INV-2","subscription":"`+sub+`","issue_date":"2027-03-01","due_date":"2027-03-15","subtotal":500,"status":"Issued"}`)
	c.create("Subscription Payment", `{"payment_reference":"P-3","invoice":"`+paidInvoice+`","amount":100,"paid_at":"2027-03-02 10:00:00","method":"Mobile Money"}`)
	if !c.refused(httpMethodPut, "Recurring Invoice", paidInvoice, `{"status":"Void"}`) {
		t.Fatal("an invoice with a payment on it was voided")
	}
	voided := c.create("Recurring Invoice", `{"invoice_number":"INV-3","subscription":"`+sub+`","issue_date":"2027-04-01","due_date":"2027-04-15","subtotal":500,"status":"Issued"}`)
	if c.refused(httpMethodPut, "Recurring Invoice", voided, `{"status":"Void"}`) {
		t.Fatal("voiding an unpaid invoice was refused")
	}
	if !c.refused(httpMethodPost, "Subscription Payment", "", `{"payment_reference":"P-VOID","invoice":"`+voided+`","amount":50,"paid_at":"2027-04-03 10:00:00","method":"Cash"}`) {
		t.Fatal("a payment against a void invoice was accepted")
	}

	entitlement := c.create("Entitlement", `{"reference":"ENT-1","subscription":"`+sub+`","entitlement_name":"SMS reminders","allowance":10,"unit":"SMS","valid_from":"2026-12-01"}`)
	usage := func(ref, quantity string) string {
		return `{"reference":"` + ref + `","entitlement":"` + entitlement + `","metric":"sms","quantity":` + quantity + `,"unit":"SMS","period_start":"2026-12-01 00:00:00","period_end":"2026-12-31 00:00:00","recorded_at":"2026-12-10 09:00:00"}`
	}
	c.create("Usage Record", usage("U-1", "4"))
	if got := c.value("Entitlement", entitlement, "used_quantity"); !strings.HasPrefix(got, "4") {
		t.Fatalf("used quantity = %q, want 4", got)
	}
	if !c.refused(httpMethodPost, "Usage Record", "", usage("U-2", "7")) {
		t.Fatal("usage above the allowance was accepted")
	}
	if got := c.value("Entitlement", entitlement, "used_quantity"); !strings.HasPrefix(got, "4") {
		t.Fatalf("used quantity after refused usage = %q, want 4", got)
	}
}

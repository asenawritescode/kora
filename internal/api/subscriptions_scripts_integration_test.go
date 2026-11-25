//go:build integration

package api

import "testing"

// The Subscriptions pack's before_save scripts, run the way the Engine runs them. A new
// subscription takes its plan's terms and needs an active plan; an invoice takes its
// subtotal from a billing schedule, which it then invoices, and voiding it returns the
// schedule. The front-line Subscriptions Clerk can read plans only: its flow proves the
// scripts work for it, and that it still cannot edit a plan.
const subscriptionsPack = "../../config/subscriptions"

func TestPackScriptsSubscriptions(t *testing.T) {
	eachPackDialect(t, []string{subscriptionsPack}, func(f *packFixture) {
		// run gives each rule its own subtest, reporting failures under that subtest's name.
		run := func(name string, rule func(f *packFixture)) {
			t.Run(name, func(t *testing.T) {
				scoped := *f
				scoped.t = t
				rule(&scoped)
			})
		}

		subscriber := f.create("Subscriber", `{"subscriber_code":"S-1","subscriber_name":"Wanjiru Kamau","email":"wanjiru@example.com"}`)
		term := f.create("Plan", `{"plan_name":"Term fees","billing_interval":"Quarterly","interval_count":3,"unit_price":250.5,"currency":"USD","status":"Active"}`)
		annual := f.create("Plan", `{"plan_name":"Annual fees","billing_interval":"Annually","interval_count":1,"unit_price":500,"currency":"USD","status":"Active"}`)
		draft := f.create("Plan", `{"plan_name":"Pilot draft","unit_price":100,"status":"Draft"}`)
		free := f.create("Plan", `{"plan_name":"Pilot free","unit_price":0,"status":"Active"}`)
		retiredAtStart := f.create("Plan", `{"plan_name":"Old term","unit_price":90,"status":"Retired"}`)

		subscription := func(plan, extra string) string {
			return `{"subscriber":"` + subscriber + `","plan":"` + plan + `","quantity":2,"start_date":"2026-12-01","next_billing_date":"2026-12-01"` + extra + `}`
		}
		schedule := func(sub, reference, amount, status string) string {
			return f.create("Billing Schedule", `{"reference":"`+reference+`","subscription":"`+sub+`","billing_date":"2026-12-01","period_start":"2026-12-01","period_end":"2026-12-31","amount":`+amount+`,"status":"`+status+`"}`)
		}

		var termSub, annualSub, otherSub string

		run("new subscription needs an active plan", func(f *packFixture) {
			f.refused(httpMethodPost, "Subscription", "", subscription(draft, ""), "Plan Pilot draft is Draft; choose an active plan.")
			f.refused(httpMethodPost, "Subscription", "", subscription(retiredAtStart, ""), "Plan Old term is Retired; choose an active plan.")
		})

		run("new subscription takes the plan's terms", func(f *packFixture) {
			termSub = f.create("Subscription", subscription(term, ""))
			f.expect("Subscription", termSub, "currency", "USD")
			f.expect("Subscription", termSub, "billing_interval", "Quarterly")
			f.expect("Subscription", termSub, "interval_count", "3")
			if got := f.amount("Subscription", termSub, "unit_price"); got != 250.5 {
				f.t.Fatalf("unit price = %v, want the plan's 250.5", got)
			}
			if got := f.amount("Subscription", termSub, "recurring_amount"); got != 501 {
				f.t.Fatalf("amount per period = %v, want 2 x 250.5 = 501", got)
			}
		})

		run("zero price takes the plan's price, typed price stays", func(f *packFixture) {
			fromPlan := f.create("Subscription", subscription(term, `,"unit_price":0`))
			if got := f.amount("Subscription", fromPlan, "unit_price"); got != 250.5 {
				f.t.Fatalf("zero price became %v, want the plan's 250.5", got)
			}
			typed := f.create("Subscription", subscription(term, `,"unit_price":300`))
			if got := f.amount("Subscription", typed, "unit_price"); got != 300 {
				f.t.Fatalf("typed price became %v, want 300", got)
			}
			if got := f.amount("Subscription", typed, "recurring_amount"); got != 600 {
				f.t.Fatalf("amount per period with typed price = %v, want 600", got)
			}
		})

		run("a plan without a price refuses a blank or zero price", func(f *packFixture) {
			f.refused(httpMethodPost, "Subscription", "", subscription(free, ""), "Set a unit price or choose a plan with one.")
			f.refused(httpMethodPost, "Subscription", "", subscription(free, `,"unit_price":0`), "Set a unit price or choose a plan with one.")
		})

		run("copied terms and price stay editable", func(f *packFixture) {
			f.update("Subscription", termSub, `{"billing_interval":"Annually","interval_count":2,"currency":"EUR"}`)
			f.expect("Subscription", termSub, "billing_interval", "Annually")
			f.expect("Subscription", termSub, "interval_count", "2")
			f.expect("Subscription", termSub, "currency", "EUR")
			f.update("Subscription", termSub, `{"unit_price":400}`)
			if got := f.amount("Subscription", termSub, "recurring_amount"); got != 800 {
				f.t.Fatalf("amount per period after a new price = %v, want 800", got)
			}
			f.refused(httpMethodPut, "Subscription", termSub, `{"unit_price":null}`, "Set a unit price.")
		})

		run("plan change needs an active plan, retired plan keeps existing subscriptions", func(f *packFixture) {
			f.refused(httpMethodPut, "Subscription", termSub, `{"plan":"`+draft+`"}`, "Plan Pilot draft is Draft; choose an active plan.")
			f.update("Plan", term, `{"status":"Retired"}`)
			f.update("Subscription", termSub, `{"end_date":"2027-12-31"}`)
			f.refused(httpMethodPost, "Subscription", "", subscription(term, ""), "Plan Term fees is Retired; choose an active plan.")
			f.update("Subscription", termSub, `{"plan":"`+annual+`"}`)
			f.expect("Subscription", termSub, "plan", annual)
		})

		var (
			inv1, inv2, bill1, bill2, bill3, bill4, bill5 string
		)
		run("invoice takes the schedule's amount and the plan's currency", func(f *packFixture) {
			annualSub = f.create("Subscription", subscription(annual, `,"quantity":1`))
			otherSub = f.create("Subscription", subscription(annual, `,"quantity":1`))
			bill1 = schedule(annualSub, "B-1", "150", "Scheduled")
			inv1 = f.create("Recurring Invoice", `{"invoice_number":"INV-1","subscription":"`+annualSub+`","billing_schedule":"`+bill1+`","issue_date":"2026-12-01","due_date":"2026-12-15"}`)
			f.expect("Recurring Invoice", inv1, "currency", "USD")
			expectAmount(f, "Recurring Invoice", inv1, "subtotal", 150)
			expectAmount(f, "Recurring Invoice", inv1, "total", 150)
			f.expect("Billing Schedule", bill1, "status", "Invoiced")
		})

		run("invoice takes the subscription's amount when there is no schedule", func(f *packFixture) {
			inv2 = f.create("Recurring Invoice", `{"invoice_number":"INV-2","subscription":"`+otherSub+`","issue_date":"2026-12-01","due_date":"2026-12-15"}`)
			if got := f.amount("Recurring Invoice", inv2, "subtotal"); got != 500 {
				f.t.Fatalf("subtotal = %v, want the subscription's 500", got)
			}
			if got := f.amount("Recurring Invoice", inv2, "balance"); got != 500 {
				f.t.Fatalf("balance = %v, want 500", got)
			}
		})

		run("typed subtotal stays and tax is worked out", func(f *packFixture) {
			bill2 = schedule(otherSub, "B-2", "150", "Scheduled")
			inv3 := f.create("Recurring Invoice", `{"invoice_number":"INV-3","subscription":"`+otherSub+`","billing_schedule":"`+bill2+`","issue_date":"2026-12-01","due_date":"2026-12-15","subtotal":90,"tax_rate":16}`)
			if got := f.amount("Recurring Invoice", inv3, "subtotal"); got != 90 {
				f.t.Fatalf("subtotal = %v, want the typed 90", got)
			}
			if got := f.amount("Recurring Invoice", inv3, "tax_amount"); got != 14.4 {
				f.t.Fatalf("tax = %v, want 14.4", got)
			}
			if got := f.amount("Recurring Invoice", inv3, "total"); got != 104.4 {
				f.t.Fatalf("total = %v, want 104.4", got)
			}
			if got := f.amount("Recurring Invoice", inv3, "balance"); got != 104.4 {
				f.t.Fatalf("balance = %v, want 104.4", got)
			}
			f.expect("Billing Schedule", bill2, "status", "Invoiced")
		})

		run("a schedule is invoiced once", func(f *packFixture) {
			f.refused(httpMethodPost, "Recurring Invoice", "", `{"invoice_number":"INV-4","subscription":"`+annualSub+`","billing_schedule":"`+bill1+`","issue_date":"2026-12-01","due_date":"2026-12-15"}`, "Billing B-1 is already Invoiced.")
		})

		run("only a scheduled billing can be invoiced", func(f *packFixture) {
			bill3 = schedule(annualSub, "B-3", "80", "Skipped")
			f.refused(httpMethodPost, "Recurring Invoice", "", `{"invoice_number":"INV-5","subscription":"`+annualSub+`","billing_schedule":"`+bill3+`","issue_date":"2026-12-01","due_date":"2026-12-15"}`, "Billing B-3 is Skipped; only a scheduled billing can be invoiced.")
		})

		run("a schedule must belong to the invoice's subscription", func(f *packFixture) {
			bill4 = schedule(otherSub, "B-4", "60", "Scheduled")
			f.refused(httpMethodPost, "Recurring Invoice", "", `{"invoice_number":"INV-6","subscription":"`+annualSub+`","billing_schedule":"`+bill4+`","issue_date":"2026-12-01","due_date":"2026-12-15"}`, "Billing B-4 belongs to another subscription.")
		})

		run("a blank subtotal with nothing to copy is refused and the schedule is left alone", func(f *packFixture) {
			bill5 = schedule(annualSub, "B-5", "0", "Scheduled")
			f.refused(httpMethodPost, "Recurring Invoice", "", `{"invoice_number":"INV-7","subscription":"`+annualSub+`","billing_schedule":"`+bill5+`","issue_date":"2026-12-01","due_date":"2026-12-15"}`, "Set a subtotal, or choose a billing schedule or subscription with an amount.")
			f.expect("Billing Schedule", bill5, "status", "Scheduled")
		})

		run("voiding an invoice returns its schedule to Scheduled", func(f *packFixture) {
			f.update("Recurring Invoice", inv1, `{"status":"Void"}`)
			f.expect("Billing Schedule", bill1, "status", "Scheduled")
			inv8 := f.create("Recurring Invoice", `{"invoice_number":"INV-8","subscription":"`+annualSub+`","billing_schedule":"`+bill1+`","issue_date":"2027-01-01","due_date":"2027-01-15"}`)
			f.expect("Billing Schedule", bill1, "status", "Invoiced")
			expectAmount(f, "Recurring Invoice", inv8, "subtotal", 150)
		})

		run("a blank subtotal on edit is refused", func(f *packFixture) {
			f.refused(httpMethodPut, "Recurring Invoice", inv2, `{"subtotal":null}`, "subtotal")
		})

		run("front-line Subscriptions Clerk creates a subscription and an invoice from a schedule", func(f *packFixture) {
			clerk := f.as("Subscriptions Clerk")
			clerkSub := clerk.create("Subscription", subscription(annual, `,"quantity":1`))
			clerk.expect("Subscription", clerkSub, "currency", "USD")
			if got := clerk.amount("Subscription", clerkSub, "recurring_amount"); got != 500 {
				clerk.t.Fatalf("clerk subscription amount per period = %v, want 500", got)
			}
			clerkBill := schedule(clerkSub, "B-9", "150", "Scheduled")
			clerkInvoice := clerk.create("Recurring Invoice", `{"invoice_number":"INV-9","subscription":"`+clerkSub+`","billing_schedule":"`+clerkBill+`","issue_date":"2027-02-01","due_date":"2027-02-15"}`)
			expectAmount(clerk, "Recurring Invoice", clerkInvoice, "subtotal", 150)
			clerk.expect("Billing Schedule", clerkBill, "status", "Invoiced")
			clerk.refused(httpMethodPost, "Subscription", "", subscription(draft, ""), "Plan Pilot draft is Draft; choose an active plan.")
			if _, _, doc := clerk.send(httpMethodPut, "Plan", annual, `{"unit_price":1}`); doc != nil {
				clerk.t.Fatal("clerk edited a plan, which it may only read")
			}
		})
	})
}

// expectAmount fails the test unless a numeric column holds want (dialects store decimals
// with different scales, so the text is not compared).
func expectAmount(f *packFixture, recordType, name, column string, want float64) {
	f.t.Helper()
	if got := f.amount(recordType, name, column); got != want {
		f.t.Fatalf("%s %s.%s = %v, want %v", recordType, name, column, got, want)
	}
}

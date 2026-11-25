//go:build integration

package api

import (
	"fmt"
	"testing"
)

const membersSavingsPack = "../../config/members-savings"

// savedNot reports whether a save is refused, without checking the message.
func (f *packFixture) savedNot(method resourceHTTPMethod, recordType, name, body string) bool {
	f.t.Helper()
	_, _, doc := f.send(method, recordType, name, body)
	return doc == nil
}

// disbursedLoan records a loan for member, approves it and disburses it.
func disbursedLoan(f *packFixture, member, reference string, amount int) string {
	f.t.Helper()
	loan := f.create("Loan", fmt.Sprintf(`{"reference":%q,"member":%q,"amount":%d,"term_months":12,"applied_on":"2026-12-05"}`, reference, member, amount))
	f.update("Loan", loan, `{"status":"Approved"}`)
	f.update("Loan", loan, `{"status":"Disbursed"}`)
	return loan
}

// TestPackScriptsSavings runs the members-savings rules through their scripts on both dialects.
func TestPackScriptsSavings(t *testing.T) {
	eachPackDialect(t, []string{membersSavingsPack}, func(f *packFixture) {
		teller := f.as("Teller")

		f.t.Run("deposits and withdrawals add up exactly", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-S1","member_name":"Achieng Otieno"}`)
			f.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-01","entry_type":"Deposit","amount":100.10}`)
			if got := f.amount("Member", m, "savings_balance"); got != 100.1 {
				t.Fatalf("balance after a deposit of 100.10 = %v, want 100.1", got)
			}
			f.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-02","entry_type":"Deposit","amount":0.20}`)
			f.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-03","entry_type":"Withdrawal","amount":0.30}`)
			if got := f.amount("Member", m, "savings_balance"); got != 100 {
				t.Fatalf("balance = %v, want 100", got)
			}
		})

		f.t.Run("a withdrawal above savings is refused and the balance kept", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-S2","member_name":"Wanjiru Kamau"}`)
			f.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-01","entry_type":"Deposit","amount":100}`)
			f.refused(httpMethodPost, "Savings Entry", "", `{"member":"`+m+`","entry_date":"2026-12-02","entry_type":"Withdrawal","amount":100.01}`,
				"has 100 in savings; cannot withdraw 100.01")
			if got := f.amount("Member", m, "savings_balance"); got != 100 {
				t.Fatalf("balance after a refused withdrawal = %v, want 100", got)
			}
		})

		f.t.Run("an exited member takes no savings entries", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-S3","member_name":"Juma Hassan"}`)
			f.update("Member", m, `{"status":"Exited"}`)
			f.refused(httpMethodPost, "Savings Entry", "", `{"member":"`+m+`","entry_date":"2026-12-01","entry_type":"Deposit","amount":5}`,
				"has exited; no new savings entries.")
		})

		f.t.Run("a savings entry for a missing member is refused", func(t *testing.T) {
			if !f.savedNot(httpMethodPost, "Savings Entry", "", `{"member":"NO-SUCH-MEMBER","entry_date":"2026-12-01","entry_type":"Deposit","amount":5}`) {
				t.Fatal("a savings entry for a missing member was saved")
			}
		})

		f.t.Run("a savings entry is append-only", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-S4","member_name":"Neema Lyimo"}`)
			entry := f.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-01","entry_type":"Deposit","amount":40}`)
			if !f.savedNot(httpMethodPut, "Savings Entry", entry, `{"amount":900}`) {
				t.Fatal("a savings entry was edited after saving")
			}
			if !f.savedNot(httpMethodDelete, "Savings Entry", entry, "") {
				t.Fatal("a savings entry was deleted after saving")
			}
			if got := f.amount("Member", m, "savings_balance"); got != 40 {
				t.Fatalf("balance = %v, want 40", got)
			}
		})

		f.t.Run("a new loan starts as Applied, Approved or Rejected", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-L1","member_name":"Peter Ouma"}`)
			for i, status := range []string{"Applied", "Approved", "Rejected"} {
				f.create("Loan", fmt.Sprintf(`{"reference":"L-START-%d","member":%q,"amount":100,"term_months":3,"applied_on":"2026-12-05","status":%q}`, i, m, status))
			}
			for _, status := range []string{"Disbursed", "Repaid"} {
				f.refused(httpMethodPost, "Loan", "", fmt.Sprintf(`{"reference":"L-BAD-%s","member":%q,"amount":100,"term_months":3,"applied_on":"2026-12-05","status":%q}`, status, m, status),
					"starts as Applied, Approved or Rejected, not "+status)
			}
		})

		f.t.Run("an exited member takes no new loan", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-L2","member_name":"Halima Yusuf"}`)
			f.update("Member", m, `{"status":"Exited"}`)
			f.refused(httpMethodPost, "Loan", "", `{"reference":"L-EXITED","member":"`+m+`","amount":100,"term_months":3,"applied_on":"2026-12-05"}`,
				"has exited; no new loans.")
		})

		f.t.Run("a loan moves only along the allowed moves", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-L3","member_name":"Sam Mutua"}`)
			loan := f.create("Loan", `{"reference":"L-MOVE","member":"`+m+`","amount":1000,"term_months":12,"applied_on":"2026-12-05"}`)
			f.refused(httpMethodPut, "Loan", loan, `{"status":"Disbursed"}`,
				"cannot change from Applied to Disbursed. Allowed moves from Applied: Approved, Rejected.")
			f.update("Loan", loan, `{"status":"Approved"}`)
			f.update("Loan", loan, `{"status":"Disbursed"}`)
			f.refused(httpMethodPut, "Loan", loan, `{"status":"Approved"}`, "Allowed moves from Disbursed: none.")
			rejected := f.create("Loan", `{"reference":"L-REJ-MOVE","member":"`+m+`","amount":100,"term_months":3,"applied_on":"2026-12-05","status":"Rejected"}`)
			f.refused(httpMethodPut, "Loan", rejected, `{"status":"Applied"}`, "Allowed moves from Rejected: none.")
		})

		f.t.Run("Repaid cannot be set by hand", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-L4","member_name":"Rose Akinyi"}`)
			loan := disbursedLoan(f, m, "L-HAND", 1000)
			f.refused(httpMethodPut, "Loan", loan, `{"status":"Repaid"}`, "A loan becomes Repaid when repayments cover it.")
		})

		f.t.Run("a disbursed loan's terms cannot change", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-L5","member_name":"Grace Njeri"}`)
			loan := disbursedLoan(f, m, "L-TERMS", 1000)
			f.refused(httpMethodPut, "Loan", loan, `{"amount":1200}`, "cannot change")
			f.refused(httpMethodPut, "Loan", loan, `{"term_months":24}`, "cannot change")
			f.refused(httpMethodPut, "Loan", loan, `{"interest_rate":5.0}`, "cannot change")
		})

		f.t.Run("a repayment needs a Disbursed loan, a positive amount and no more than is owed", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-R1","member_name":"Tom Kiprop"}`)
			approved := f.create("Loan", `{"reference":"L-NOT-YET","member":"`+m+`","amount":500,"term_months":6,"applied_on":"2026-12-05","status":"Approved"}`)
			f.refused(httpMethodPost, "Loan Repayment", "", `{"loan":"`+approved+`","repayment_date":"2026-12-10","amount":100}`,
				"is Approved; repayments start once it is Disbursed.")
			loan := disbursedLoan(f, m, "L-REPAY", 1000)
			f.refused(httpMethodPost, "Loan Repayment", "", `{"loan":"`+loan+`","repayment_date":"2026-12-10","amount":0}`, "more than zero")
			f.refused(httpMethodPost, "Loan Repayment", "", `{"loan":"`+loan+`","repayment_date":"2026-12-10","amount":1000.01}`,
				"is more than the 1000 still owed on "+loan+".")
		})

		f.t.Run("partial then final repayment makes the loan Repaid", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-R2","member_name":"Zawadi Mwangi"}`)
			loan := disbursedLoan(f, m, "L-FULL", 1000)
			f.create("Loan Repayment", `{"loan":"`+loan+`","repayment_date":"2026-12-10","amount":400}`)
			if got := f.amount("Loan", loan, "outstanding"); got != 600 {
				t.Fatalf("outstanding after 400 = %v, want 600", got)
			}
			if got := f.value("Loan", loan, "status"); got != "Disbursed" {
				t.Fatalf("status after a partial repayment = %q, want Disbursed", got)
			}
			f.create("Loan Repayment", `{"loan":"`+loan+`","repayment_date":"2026-12-11","amount":600}`)
			f.expect("Loan", loan, "status", "Repaid")
			if got := f.amount("Loan", loan, "outstanding"); got != 0 {
				t.Fatalf("outstanding after the final repayment = %v, want 0", got)
			}
			f.refused(httpMethodPost, "Loan Repayment", "", `{"loan":"`+loan+`","repayment_date":"2026-12-12","amount":1}`,
				"is Repaid; repayments start once it is Disbursed.")
		})

		f.t.Run("a repaid loan is frozen", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-R3","member_name":"Brian Odhiambo"}`)
			loan := disbursedLoan(f, m, "L-FROZEN", 300)
			f.create("Loan Repayment", `{"loan":"`+loan+`","repayment_date":"2026-12-10","amount":300}`)
			f.refused(httpMethodPut, "Loan", loan, `{"amount":1}`, "cannot be changed when status is Repaid")
			f.refused(httpMethodPut, "Loan", loan, `{"status":"Disbursed"}`, "Allowed moves from Repaid: none.")
		})

		f.t.Run("a Teller records savings and repayments; the Member and Loan follow", func(t *testing.T) {
			m := f.create("Member", `{"member_no":"M-T1","member_name":"Esther Wambui"}`)
			loan := disbursedLoan(f, m, "L-TELLER", 1000)
			teller.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-01","entry_type":"Deposit","amount":250}`)
			teller.create("Savings Entry", `{"member":"`+m+`","entry_date":"2026-12-02","entry_type":"Withdrawal","amount":50}`)
			if got := f.amount("Member", m, "savings_balance"); got != 200 {
				t.Fatalf("member balance after the Teller's entries = %v, want 200", got)
			}
			teller.create("Loan Repayment", `{"loan":"`+loan+`","repayment_date":"2026-12-10","amount":300}`)
			if got := f.amount("Loan", loan, "amount_repaid"); got != 300 {
				t.Fatalf("amount repaid after the Teller's repayment = %v, want 300", got)
			}
			teller.create("Loan Repayment", `{"loan":"`+loan+`","repayment_date":"2026-12-11","amount":700}`)
			f.expect("Loan", loan, "status", "Repaid")
			if !teller.savedNot(httpMethodPut, "Loan", loan, `{"amount":1}`) {
				t.Fatal("the Teller edited a loan")
			}
			if !teller.savedNot(httpMethodPut, "Member", m, `{"savings_balance":9999}`) {
				t.Fatal("the Teller edited a member's savings balance")
			}
			if got := f.amount("Member", m, "savings_balance"); got != 200 {
				t.Fatalf("balance after refused edits = %v, want 200", got)
			}
		})
	})
}

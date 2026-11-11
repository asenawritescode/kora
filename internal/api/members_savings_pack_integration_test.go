//go:build integration

package api

import (
	"strconv"
	"testing"
)

// The Members pack as shipped, on real MySQL: a member, savings entries, a loan
// and repayments, the minimums on amounts, append-only records and a frozen loan.
func TestMembersSavingsAndLoanWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "members-savings")
	c := newPackClient(t, handler, db, reg, site)

	member := c.create("Member", `{"member_no":"M-1","member_name":"Achieng Otieno","phone":"0700000001","joined_on":"2026-01-10"}`)
	if got := c.value("Member", member, "status"); got != "Active" {
		t.Fatalf("member status = %q, want Active", got)
	}
	if !c.refused(httpMethodPost, "Member", "", `{"member_no":"M-1","member_name":"Someone Else"}`) {
		t.Fatal("a second member with the same number was accepted")
	}

	deposit := c.create("Savings Entry", `{"member":"`+member+`","entry_date":"2026-12-01","entry_type":"Deposit","amount":500}`)
	c.create("Savings Entry", `{"member":"`+member+`","entry_date":"2026-12-02","entry_type":"Withdrawal","amount":100}`)
	if !c.refused(httpMethodPost, "Savings Entry", "", `{"member":"`+member+`","entry_date":"2026-12-03","amount":0}`) {
		t.Fatal("a savings entry of zero was accepted")
	}
	if !c.refused(httpMethodPost, "Savings Entry", "", `{"member":"`+member+`","entry_date":"2026-12-03","amount":0.0}`) {
		t.Fatal("a savings entry of 0.0 was accepted")
	}
	if !c.refused(httpMethodPost, "Savings Entry", "", `{"member":"`+member+`","amount":10}`) {
		t.Fatal("a savings entry without a date was accepted")
	}
	if !c.refused(httpMethodPut, "Savings Entry", deposit, `{"amount":900}`) {
		t.Fatal("a savings entry was edited after saving")
	}
	if got, err := strconv.ParseFloat(c.value("Savings Entry", deposit, "amount"), 64); err != nil || got != 500 {
		t.Fatalf("amount after a refused edit = %v, %v; want 500", got, err)
	}

	loan := c.create("Loan", `{"reference":"L-1","member":"`+member+`","amount":10000,"interest_rate":12.5,"term_months":12,"applied_on":"2026-12-05"}`)
	if got := c.value("Loan", loan, "status"); got != "Applied" {
		t.Fatalf("loan status = %q, want Applied", got)
	}
	for name, body := range map[string]string{
		"a duplicate reference":    `{"reference":"L-1","member":"` + member + `","amount":1,"term_months":1,"applied_on":"2026-12-05"}`,
		"an amount of zero":        `{"reference":"L-2","member":"` + member + `","amount":0.0,"term_months":6,"applied_on":"2026-12-05"}`,
		"a negative interest rate": `{"reference":"L-3","member":"` + member + `","amount":100,"interest_rate":-1.0,"term_months":6,"applied_on":"2026-12-05"}`,
		"a term of zero months":    `{"reference":"L-4","member":"` + member + `","amount":100,"term_months":0,"applied_on":"2026-12-05"}`,
		"a loan without a date":    `{"reference":"L-5","member":"` + member + `","amount":100,"term_months":6}`,
		"a loan without a member":  `{"reference":"L-6","amount":100,"term_months":6,"applied_on":"2026-12-05"}`,
	} {
		if !c.refused(httpMethodPost, "Loan", "", body) {
			t.Fatalf("%s was accepted", name)
		}
	}
	// A zero interest rate is allowed.
	c.create("Loan", `{"reference":"L-ZERO","member":"`+member+`","amount":100,"interest_rate":0.0,"term_months":1,"applied_on":"2026-12-05"}`)

	// The terms may change while the loan is being decided.
	if c.refused(httpMethodPut, "Loan", loan, `{"amount":12000,"status":"Approved"}`) {
		t.Fatal("changing an approved loan's amount was refused")
	}
	if c.refused(httpMethodPut, "Loan", loan, `{"status":"Disbursed"}`) {
		t.Fatal("disbursing a loan was refused")
	}

	repayment := c.create("Loan Repayment", `{"loan":"`+loan+`","repayment_date":"2026-12-20","amount":1000}`)
	if !c.refused(httpMethodPost, "Loan Repayment", "", `{"loan":"`+loan+`","repayment_date":"2026-12-21","amount":0.0}`) {
		t.Fatal("a repayment of zero was accepted")
	}
	if !c.refused(httpMethodPost, "Loan Repayment", "", `{"loan":"`+loan+`","amount":50}`) {
		t.Fatal("a repayment without a date was accepted")
	}
	if !c.refused(httpMethodPut, "Loan Repayment", repayment, `{"amount":1}`) {
		t.Fatal("a repayment was edited after saving")
	}

	// Repaid and Rejected loans are frozen.
	if c.refused(httpMethodPut, "Loan", loan, `{"status":"Repaid"}`) {
		t.Fatal("marking a loan Repaid was refused")
	}
	if !c.refused(httpMethodPut, "Loan", loan, `{"amount":1}`) {
		t.Fatal("the amount of a repaid loan was changed")
	}
	if !c.refused(httpMethodPut, "Loan", loan, `{"term_months":99}`) {
		t.Fatal("the term of a repaid loan was changed")
	}
	if got := c.value("Loan", loan, "term_months"); got != "12" {
		t.Fatalf("term after a refused change = %q, want 12", got)
	}

	rejected := c.create("Loan", `{"reference":"L-REJ","member":"`+member+`","amount":500,"term_months":3,"applied_on":"2026-12-06","status":"Rejected"}`)
	if !c.refused(httpMethodPut, "Loan", rejected, `{"interest_rate":5.0}`) {
		t.Fatal("the interest rate of a rejected loan was changed")
	}

	// Members can be set Dormant or Exited.
	if c.refused(httpMethodPut, "Member", member, `{"status":"Dormant"}`) {
		t.Fatal("marking a member Dormant was refused")
	}
}

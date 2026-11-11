//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Grants pack as shipped, on real MySQL: a grant keeps its minimum amount
// and date order, a report needs its submission date, and a closed grant is frozen.
func TestGrantsDonorGrantAndReportWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "grants-donors")
	c := newPackClient(t, handler, db, reg, site)

	donor := c.create("Donor", `{"donor_name":"Hope Foundation","donor_type":"Foundation"}`)
	other := c.create("Donor", `{"donor_name":"County Fund","donor_type":"Government"}`)
	if !c.refused(httpMethodPost, "Donor", "", `{"donor_name":"Hope Foundation"}`) {
		t.Fatal("a second donor with the same name was accepted")
	}

	grant := c.create("Grant", `{"reference":"G-1","grant_title":"Clean water","donor":"`+donor+`","amount":50000,"start_date":"2026-01-01","end_date":"2026-12-31"}`)
	if got := c.value("Grant", grant, "status"); got != "Proposed" {
		t.Fatalf("new grant status = %q, want Proposed", got)
	}
	for _, amount := range []string{"0", "-5", "0.001"} {
		if !c.refused(httpMethodPost, "Grant", "", `{"reference":"G-BAD","grant_title":"Bad","donor":"`+donor+`","amount":`+amount+`}`) {
			t.Fatalf("a grant of amount %s was accepted", amount)
		}
	}
	if c.refused(httpMethodPost, "Grant", "", `{"reference":"G-MIN","grant_title":"Smallest","donor":"`+donor+`","amount":0.01}`) {
		t.Fatal("a grant of exactly 0.01 was refused")
	}
	if !c.refused(httpMethodPost, "Grant", "", `{"reference":"G-2","grant_title":"Backwards","donor":"`+donor+`","amount":100,"start_date":"2026-06-01","end_date":"2026-01-01"}`) {
		t.Fatal("a grant ending before it starts was accepted")
	}
	if !c.refused(httpMethodPost, "Grant", "", `{"reference":"G-1","grant_title":"Duplicate","donor":"`+donor+`","amount":100}`) {
		t.Fatal("a second grant with the same reference was accepted")
	}

	report := c.create("Grant Report", `{"grant_ref":"`+grant+`","due_date":"2026-06-30"}`)
	if got := c.value("Grant Report", report, "status"); got != "Due" {
		t.Fatalf("new report status = %q, want Due", got)
	}
	if !c.refused(httpMethodPut, "Grant Report", report, `{"status":"Submitted"}`) {
		t.Fatal("a report was marked Submitted without a submission date")
	}
	if !c.refused(httpMethodPut, "Grant Report", report, `{"status":"Accepted"}`) {
		t.Fatal("a report was marked Accepted without a submission date")
	}
	if c.refused(httpMethodPut, "Grant Report", report, `{"status":"Submitted","submitted_on":"2026-06-20"}`) {
		t.Fatal("submitting a report with a date was refused")
	}
	if got := c.value("Grant Report", report, "submitted_on"); !strings.HasPrefix(got, "2026-06-20") {
		t.Fatalf("submitted on = %q, want 2026-06-20", got)
	}

	if c.refused(httpMethodPut, "Grant", grant, `{"status":"Active"}`) {
		t.Fatal("activating a grant was refused")
	}
	if c.refused(httpMethodPut, "Grant", grant, `{"amount":60000}`) {
		t.Fatal("changing the amount of an active grant was refused")
	}
	if c.refused(httpMethodPut, "Grant", grant, `{"status":"Closed"}`) {
		t.Fatal("closing a grant was refused")
	}
	if !c.refused(httpMethodPut, "Grant", grant, `{"amount":1000}`) {
		t.Fatal("the amount of a closed grant was changed")
	}
	if !c.refused(httpMethodPut, "Grant", grant, `{"donor":"`+other+`"}`) {
		t.Fatal("the donor of a closed grant was changed")
	}
	if got := c.value("Grant", grant, "amount"); !strings.HasPrefix(got, "60000") {
		t.Fatalf("amount after refused changes = %q, want 60000", got)
	}

	declined := c.create("Grant", `{"reference":"G-3","grant_title":"Rejected bid","donor":"`+donor+`","amount":900}`)
	if c.refused(httpMethodPut, "Grant", declined, `{"status":"Declined"}`) {
		t.Fatal("declining a grant was refused")
	}
	if !c.refused(httpMethodPut, "Grant", declined, `{"amount":950}`) {
		t.Fatal("the amount of a declined grant was changed")
	}
}

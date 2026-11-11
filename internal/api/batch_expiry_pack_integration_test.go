//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Dispensary pack as shipped, on real MySQL, with its medicines taken from
// the Selling pack: batches need an expiry date, a prescription keeps its
// medicine lines, and a dispensed prescription is frozen.
func TestDispensaryPrescriptionAndBatchWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "pos-base", "batch-expiry")
	if _, err := db.Exec("INSERT INTO `tabProduct` (name, reference, product_name, selling_price, stock_qty, status) VALUES ('PROD-001','P-1','Amoxicillin 500mg',300,0,'Active')"); err != nil {
		t.Fatal(err)
	}
	c := newPackClient(t, handler, db, reg, site)

	batch := c.create("Medicine Batch", `{"batch_no":"B-100","product":"PROD-001","expiry_date":"2027-03-31","quantity_received":200,"supplier":"MediSupply","received_on":"2026-11-01"}`)
	if got := c.value("Medicine Batch", batch, "expiry_date"); !strings.HasPrefix(got, "2027-03-31") {
		t.Fatalf("batch expiry = %q, want 2027-03-31", got)
	}
	if !c.refused(httpMethodPost, "Medicine Batch", "", `{"batch_no":"B-100","product":"PROD-001","expiry_date":"2027-06-30"}`) {
		t.Fatal("a second batch with the same number was accepted")
	}
	if !c.refused(httpMethodPost, "Medicine Batch", "", `{"batch_no":"B-101","product":"PROD-001"}`) {
		t.Fatal("a batch without an expiry date was accepted")
	}
	if !c.refused(httpMethodPost, "Medicine Batch", "", `{"batch_no":"B-102","product":"PROD-001","expiry_date":"2027-06-30","quantity_received":-5}`) {
		t.Fatal("a batch with a negative quantity was accepted")
	}

	patient := c.create("Patient", `{"patient_name":"Achieng Otieno","phone":"0712000111","date_of_birth":"1990-05-20","allergies":"Penicillin"}`)
	other := c.create("Patient", `{"patient_name":"Brian Mwangi","phone":"0722000222"}`)

	rx := c.create("Prescription", `{"reference":"RX-1","patient":"`+patient+`","prescriber":"Dr Wafula","prescribed_on":"2026-11-10","lines":[{"product":"PROD-001","dosage":"1 tablet three times a day","quantity":15}]}`)
	if got := c.value("Prescription", rx, "status"); got != "Received" {
		t.Fatalf("new prescription status = %q, want Received", got)
	}
	var lines int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabPrescription__lines` WHERE parent = ?", rx).Scan(&lines); err != nil || lines != 1 {
		t.Fatalf("prescription lines = %d, %v, want 1", lines, err)
	}
	if !c.refused(httpMethodPost, "Prescription", "", `{"reference":"RX-1","patient":"`+patient+`"}`) {
		t.Fatal("a second prescription with the same reference was accepted")
	}

	if c.refused(httpMethodPut, "Prescription", rx, `{"patient":"`+other+`"}`) {
		t.Fatal("changing the patient of a prescription still waiting was refused")
	}
	if c.refused(httpMethodPut, "Prescription", rx, `{"status":"Dispensed"}`) {
		t.Fatal("dispensing a prescription was refused")
	}
	if !c.refused(httpMethodPut, "Prescription", rx, `{"patient":"`+patient+`"}`) {
		t.Fatal("the patient of a dispensed prescription was changed")
	}
	if !c.refused(httpMethodPut, "Prescription", rx, `{"prescribed_on":"2026-11-01"}`) {
		t.Fatal("the date of a dispensed prescription was changed")
	}
	if got := c.value("Prescription", rx, "prescribed_on"); !strings.HasPrefix(got, "2026-11-10") {
		t.Fatalf("date after a refused change = %q, want 2026-11-10", got)
	}
	if got := c.value("Prescription", rx, "patient"); got != other {
		t.Fatalf("patient after refused changes = %q, want %q", got, other)
	}
}

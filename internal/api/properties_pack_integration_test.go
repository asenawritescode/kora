//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Properties pack as shipped, on real MySQL: units and leases are saved,
// negative rents and backwards leases are refused, an ended lease is frozen.
func TestPropertiesLettingWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "properties")
	c := newPackClient(t, handler, db, reg, site)

	property := c.create("Property", `{"property_name":"Acacia Court","address":"Ngong Road","property_type":"Apartment"}`)
	if !c.refused(httpMethodPost, "Property", "", `{"property_name":"Acacia Court"}`) {
		t.Fatal("a second property with the same name was accepted")
	}

	unit := c.create("Unit", `{"unit_no":"A1","property":"`+property+`","monthly_rent":25000}`)
	if got := c.value("Unit", unit, "status"); got != "Vacant" {
		t.Fatalf("new unit status = %q, want Vacant", got)
	}
	if !c.refused(httpMethodPost, "Unit", "", `{"unit_no":"A2","property":"`+property+`","monthly_rent":-1}`) {
		t.Fatal("a unit with a negative rent was accepted")
	}
	if !c.refused(httpMethodPut, "Unit", unit, `{"monthly_rent":-5.5}`) {
		t.Fatal("a unit rent was changed to a negative value")
	}
	if !c.refused(httpMethodPost, "Unit", "", `{"unit_no":"A3","property":"`+property+`","monthly_rent":-0.5}`) {
		t.Fatal("a fractional negative rent was accepted")
	}

	tenant := c.create("Tenant", `{"tenant_name":"Achieng Otieno","phone":"0700000001","id_number":"12345678"}`)

	if !c.refused(httpMethodPost, "Lease", "", `{"reference":"L-BAD","unit":"`+unit+`","tenant":"`+tenant+`","start_date":"2026-12-01","end_date":"2026-11-01"}`) {
		t.Fatal("a lease that ends before it starts was accepted")
	}
	if !c.refused(httpMethodPost, "Lease", "", `{"reference":"L-NEG","unit":"`+unit+`","tenant":"`+tenant+`","start_date":"2026-12-01","monthly_rent":-1}`) {
		t.Fatal("a lease with a negative rent was accepted")
	}
	if !c.refused(httpMethodPost, "Lease", "", `{"reference":"L-DEP","unit":"`+unit+`","tenant":"`+tenant+`","start_date":"2026-12-01","deposit":-1}`) {
		t.Fatal("a lease with a negative deposit was accepted")
	}

	lease := c.create("Lease", `{"reference":"L-1","unit":"`+unit+`","tenant":"`+tenant+`","start_date":"2026-12-01","end_date":"2027-11-30","monthly_rent":25000,"deposit":50000}`)
	if got := c.value("Lease", lease, "status"); got != "Active" {
		t.Fatalf("new lease status = %q, want Active", got)
	}
	if !c.refused(httpMethodPost, "Lease", "", `{"reference":"L-1","unit":"`+unit+`","tenant":"`+tenant+`","start_date":"2027-12-01"}`) {
		t.Fatal("a second lease with the same reference was accepted")
	}

	if c.refused(httpMethodPut, "Lease", lease, `{"monthly_rent":26000}`) {
		t.Fatal("an active lease rent change was refused")
	}
	if c.refused(httpMethodPut, "Lease", lease, `{"status":"Ended","end_date":"2027-06-30"}`) {
		t.Fatal("ending an active lease was refused")
	}
	if !c.refused(httpMethodPut, "Lease", lease, `{"start_date":"2026-11-15"}`) {
		t.Fatal("the start date of an ended lease was changed")
	}
	otherTenant := c.create("Tenant", `{"tenant_name":"Mwangi Kariuki"}`)
	if !c.refused(httpMethodPut, "Lease", lease, `{"tenant":"`+otherTenant+`"}`) {
		t.Fatal("the tenant of an ended lease was changed")
	}
	if got := c.value("Lease", lease, "start_date"); !strings.HasPrefix(got, "2026-12-01") {
		t.Fatalf("start date after a refused change = %q, want 2026-12-01", got)
	}
	if got := c.value("Lease", lease, "tenant"); got != tenant {
		t.Fatalf("tenant after a refused change = %q, want %q", got, tenant)
	}
}

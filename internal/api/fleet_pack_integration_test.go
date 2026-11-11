//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Fleet pack as shipped, on real MySQL: a trip runs to Delivered (refused
// without an end date), is frozen afterwards, cannot run backwards, and fuel
// and service records are saved.
func TestFleetVehicleDriverTripWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "fleet")
	c := newPackClient(t, handler, db, reg, site)

	vehicle := c.create("Vehicle", `{"registration":"KDA 123A","make":"Isuzu","model":"NPR","year":2021,"odometer_km":45000}`)
	if got := c.value("Vehicle", vehicle, "status"); got != "Available" {
		t.Fatalf("new vehicle status = %q, want Available", got)
	}
	if !c.refused(httpMethodPost, "Vehicle", "", `{"registration":"KDA 123A"}`) {
		t.Fatal("a second vehicle with the same registration was accepted")
	}
	driver := c.create("Driver", `{"driver_name":"Otieno Odhiambo","phone":"0700000000","licence_no":"DL-77","licence_expiry":"2028-01-31"}`)
	other := c.create("Driver", `{"driver_name":"Achieng Wekesa"}`)

	trip := c.create("Trip", `{"reference":"T-1","vehicle":"`+vehicle+`","driver":"`+driver+`","origin":"Nairobi","destination":"Mombasa","start_date":"2026-12-01","distance_km":480}`)
	if got := c.value("Trip", trip, "status"); got != "Planned" {
		t.Fatalf("new trip status = %q, want Planned", got)
	}
	if !c.refused(httpMethodPost, "Trip", "", `{"reference":"T-BAD","vehicle":"`+vehicle+`","driver":"`+driver+`","origin":"A","destination":"B","start_date":"2026-12-05","end_date":"2026-12-01"}`) {
		t.Fatal("a trip that ends before it starts was accepted")
	}
	if !c.refused(httpMethodPost, "Trip", "", `{"reference":"T-NEG","vehicle":"`+vehicle+`","driver":"`+driver+`","origin":"A","destination":"B","start_date":"2026-12-01","distance_km":-5}`) {
		t.Fatal("a negative distance was accepted")
	}

	if c.refused(httpMethodPut, "Trip", trip, `{"status":"On the road"}`) {
		t.Fatal("starting a planned trip was refused")
	}
	if !c.refused(httpMethodPut, "Trip", trip, `{"status":"Delivered"}`) {
		t.Fatal("a trip was delivered without an end date")
	}
	if c.refused(httpMethodPut, "Trip", trip, `{"status":"Delivered","end_date":"2026-12-02"}`) {
		t.Fatal("delivering a trip with an end date was refused")
	}
	if got := c.value("Trip", trip, "status"); got != "Delivered" {
		t.Fatalf("trip status = %q, want Delivered", got)
	}

	for _, change := range []string{`{"driver":"` + other + `"}`, `{"destination":"Kisumu"}`, `{"origin":"Nakuru"}`} {
		if !c.refused(httpMethodPut, "Trip", trip, change) {
			t.Fatalf("a delivered trip was changed with %s", change)
		}
	}
	if got := c.value("Trip", trip, "destination"); got != "Mombasa" {
		t.Fatalf("destination after refused change = %q, want Mombasa", got)
	}
	if got := c.value("Trip", trip, "end_date"); !strings.HasPrefix(got, "2026-12-02") {
		t.Fatalf("end date = %q, want 2026-12-02", got)
	}

	fuel := c.create("Fuel Log", `{"vehicle":"`+vehicle+`","fill_date":"2026-12-02","litres":60,"cost":9000,"odometer_km":45480}`)
	if got := c.value("Fuel Log", fuel, "label"); got == "" {
		t.Fatal("fuel log has no readable label")
	}
	service := c.create("Service Record", `{"vehicle":"`+vehicle+`","service_date":"2026-12-10","description":"Oil change","cost":4500,"next_service_date":"2027-03-10"}`)
	if got := c.value("Service Record", service, "next_service_date"); !strings.HasPrefix(got, "2027-03-10") {
		t.Fatalf("next service date = %q, want 2027-03-10", got)
	}
}

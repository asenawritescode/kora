//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Shipments pack as shipped, on real MySQL: a shipment is booked, tracked
// through events and delivered; a delivery needs its date, then the parties are frozen.
func TestShipmentsBookTrackAndDeliverWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "shipments")
	c := newPackClient(t, handler, db, reg, site)

	shipment := c.create("Shipment", `{"reference":"SH-1","customer_name":"Acme Traders","origin":"Mombasa","destination":"Nairobi","booked_on":"2026-12-01","weight_kg":120.5,"charge":4500,"vehicle":"KDA 123A","driver":"Otieno"}`)
	if got := c.value("Shipment", shipment, "status"); got != "Booked" {
		t.Fatalf("new shipment status = %q, want Booked", got)
	}
	if !c.refused(httpMethodPost, "Shipment", "", `{"reference":"SH-1","customer_name":"Other","origin":"A","destination":"B","booked_on":"2026-12-01"}`) {
		t.Fatal("a second shipment with the same reference was accepted")
	}
	if !c.refused(httpMethodPost, "Shipment", "", `{"reference":"SH-NEG","customer_name":"Other","origin":"A","destination":"B","booked_on":"2026-12-01","weight_kg":-1}`) {
		t.Fatal("a negative weight was accepted")
	}
	if !c.refused(httpMethodPost, "Shipment", "", `{"reference":"SH-NEGC","customer_name":"Other","origin":"A","destination":"B","booked_on":"2026-12-01","charge":-5}`) {
		t.Fatal("a negative charge was accepted")
	}

	c.create("Shipment", `{"reference":"SH-ZERO","customer_name":"Other","origin":"A","destination":"B","booked_on":"2026-12-01","weight_kg":0,"charge":0}`)

	event := c.create("Shipment Event", `{"shipment":"`+shipment+`","event_date":"2026-12-02","status":"Collected","note":"Picked up at the depot"}`)
	if got := c.value("Shipment Event", event, "label"); got == "" {
		t.Fatal("shipment event has no readable label")
	}
	if c.refused(httpMethodPut, "Shipment", shipment, `{"status":"In transit"}`) {
		t.Fatal("moving a shipment to In transit was refused")
	}

	if !c.refused(httpMethodPut, "Shipment", shipment, `{"status":"Delivered"}`) {
		t.Fatal("a shipment was marked Delivered without a delivery date")
	}
	if got := c.value("Shipment", shipment, "status"); got != "In transit" {
		t.Fatalf("status after a refused delivery = %q, want In transit", got)
	}
	if c.refused(httpMethodPut, "Shipment", shipment, `{"status":"Delivered","delivered_on":"2026-12-04"}`) {
		t.Fatal("delivering a shipment with its date was refused")
	}

	for _, change := range []string{`{"customer_name":"Someone Else"}`, `{"origin":"Kisumu"}`, `{"destination":"Eldoret"}`} {
		if !c.refused(httpMethodPut, "Shipment", shipment, change) {
			t.Fatalf("a delivered shipment accepted %s", change)
		}
	}
	if got := c.value("Shipment", shipment, "destination"); got != "Nairobi" {
		t.Fatalf("destination after refused changes = %q, want Nairobi", got)
	}
	if got := c.value("Shipment", shipment, "delivered_on"); !strings.HasPrefix(got, "2026-12-04") {
		t.Fatalf("delivered_on = %q, want 2026-12-04", got)
	}
}

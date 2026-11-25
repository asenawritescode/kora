//go:build integration

package api

import (
	"strings"
	"testing"
)

// The shipments pack as shipped, with its scripts run the way the Engine runs them: a
// shipment's status moves forward through its tracking events, and a person's status
// change follows the same rule. Shipping Clerk (the front-line role) may edit both record
// types, so its flow proves the scripts, not the permissions.
const shipmentsPack = "../../config/shipments"

func TestPackScriptsShipments(t *testing.T) {
	eachPackDialect(t, []string{shipmentsPack}, func(f *packFixture) {
		t.Run("a booked shipment takes its first event and moves to that status", func(t *testing.T) {
			ship := bookShipment(f, "SH-S1")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "Collected"))
			f.expect("Shipment", ship, "status", "Collected")
		})
		t.Run("events move the shipment through in transit to delivered", func(t *testing.T) {
			ship := bookShipment(f, "SH-S2")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "Collected"))
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-03", "In transit"))
			f.expect("Shipment", ship, "status", "In transit")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-03", "In transit"))
			f.create("Shipment Event", `{"shipment":"`+ship+`","event_date":"2026-12-03","note":"Waiting at the border"}`)
			f.expect("Shipment", ship, "status", "In transit")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-04", "Delivered"))
			f.expect("Shipment", ship, "status", "Delivered")
			expectDeliveredOn(f, ship, "2026-12-04")
		})
		t.Run("a delivered event keeps a delivered-on date that is already set", func(t *testing.T) {
			ship := bookShipment(f, "SH-S3")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "In transit"))
			f.update("Shipment", ship, `{"delivered_on":"2026-12-03"}`)
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-04", "Delivered"))
			f.expect("Shipment", ship, "status", "Delivered")
			if got := f.value("Shipment", ship, "delivered_on"); !strings.HasPrefix(got, "2026-12-03") {
				t.Fatalf("delivered_on = %q, want it kept at 2026-12-03", got)
			}
		})
		t.Run("a backwards event is refused and the shipment does not change", func(t *testing.T) {
			ship := bookShipment(f, "SH-S4")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "In transit"))
			f.refused(httpMethodPost, "Shipment Event", "", shipmentEventBody(ship, "2026-12-03", "Collected"),
				"SH-S4 is In transit; it cannot go back to Collected.")
			f.expect("Shipment", ship, "status", "In transit")
		})
		t.Run("a failed shipment is final", func(t *testing.T) {
			ship := bookShipment(f, "SH-S5")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "Failed"))
			f.expect("Shipment", ship, "status", "Failed")
			f.refused(httpMethodPost, "Shipment Event", "", shipmentEventBody(ship, "2026-12-03", "In transit"),
				"SH-S5 is already Failed.")
			f.refused(httpMethodPost, "Shipment Event", "", `{"shipment":"`+ship+`","event_date":"2026-12-03","note":"Late note"}`,
				"SH-S5 is already Failed.")
			f.expect("Shipment", ship, "status", "Failed")
		})
		t.Run("a delivered shipment takes no more events", func(t *testing.T) {
			ship := bookShipment(f, "SH-S6")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-04", "Delivered"))
			f.expect("Shipment", ship, "status", "Delivered")
			f.refused(httpMethodPost, "Shipment Event", "", shipmentEventBody(ship, "2026-12-05", "Failed"),
				"SH-S6 is already Delivered.")
			f.refused(httpMethodPost, "Shipment Event", "", `{"shipment":"`+ship+`","event_date":"2026-12-05","note":"Note"}`,
				"SH-S6 is already Delivered.")
		})
		t.Run("a note without a status changes nothing", func(t *testing.T) {
			ship := bookShipment(f, "SH-S7")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "Collected"))
			f.create("Shipment Event", `{"shipment":"`+ship+`","event_date":"2026-12-03","note":"Dog at the gate"}`)
			f.expect("Shipment", ship, "status", "Collected")
		})
		t.Run("an event with no shipment is refused", func(t *testing.T) {
			f.refused(httpMethodPost, "Shipment Event", "", `{"shipment":"SH-NOPE","event_date":"2026-12-02","status":"Collected"}`,
				"Cannot check shipment SH-NOPE.")
		})
		t.Run("a saved tracking event cannot be edited", func(t *testing.T) {
			ship := bookShipment(f, "SH-S8")
			event := f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "Collected"))
			f.refused(httpMethodPut, "Shipment Event", event, `{"note":"Changed after the fact"}`,
				"cannot be changed after it is saved")
			f.refused(httpMethodPut, "Shipment Event", event, `{"status":"Failed"}`,
				"cannot be changed after it is saved")
			f.expect("Shipment", ship, "status", "Collected")
		})
		t.Run("a person cannot move a shipment backwards or leave a final status", func(t *testing.T) {
			ship := bookShipment(f, "SH-S9")
			f.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "In transit"))
			f.refused(httpMethodPut, "Shipment", ship, `{"status":"Collected"}`,
				"SH-S9 is In transit; it cannot go back to Collected.")
			f.expect("Shipment", ship, "status", "In transit")
			f.update("Shipment", ship, `{"status":"Delivered","delivered_on":"2026-12-04"}`)
			f.refused(httpMethodPut, "Shipment", ship, `{"status":"Failed"}`,
				"SH-S9 is Delivered; it cannot change to Failed.")
			f.expect("Shipment", ship, "status", "Delivered")
		})
		t.Run("a person may skip forward", func(t *testing.T) {
			ship := bookShipment(f, "SH-S10")
			f.update("Shipment", ship, `{"status":"In transit"}`)
			f.expect("Shipment", ship, "status", "In transit")
		})
		t.Run("shipping clerk books, tracks and delivers, and cannot edit the log or go back", func(t *testing.T) {
			clerk := f.as("Shipping Clerk")
			ship := clerk.create("Shipment", `{"reference":"SH-C1","customer_name":"Acme Traders","origin":"Mombasa","destination":"Nairobi","booked_on":"2026-12-01"}`)
			event := clerk.create("Shipment Event", shipmentEventBody(ship, "2026-12-02", "Collected"))
			f.expect("Shipment", ship, "status", "Collected")
			clerk.create("Shipment Event", shipmentEventBody(ship, "2026-12-03", "In transit"))
			f.expect("Shipment", ship, "status", "In transit")
			clerk.refused(httpMethodPut, "Shipment", ship, `{"status":"Collected"}`,
				"SH-C1 is In transit; it cannot go back to Collected.")
			clerk.refused(httpMethodPut, "Shipment Event", event, `{"note":"Edited"}`,
				"cannot be changed after it is saved")
			clerk.create("Shipment Event", shipmentEventBody(ship, "2026-12-04", "Delivered"))
			f.expect("Shipment", ship, "status", "Delivered")
			expectDeliveredOn(f, ship, "2026-12-04")
			clerk.refused(httpMethodPut, "Shipment", ship, `{"status":"Failed"}`,
				"SH-C1 is Delivered; it cannot change to Failed.")
		})
	})
}

// bookShipment creates a shipment in Booked with the given reference.
func bookShipment(f *packFixture, reference string) string {
	f.t.Helper()
	return f.create("Shipment", `{"reference":"`+reference+`","customer_name":"Acme Traders","origin":"Mombasa","destination":"Nairobi","booked_on":"2026-12-01"}`)
}

// shipmentEventBody is a tracking event body with a status.
func shipmentEventBody(shipment, date, status string) string {
	return `{"shipment":"` + shipment + `","event_date":"` + date + `","status":"` + status + `"}`
}

// expectDeliveredOn checks the delivered-on date, which a dialect may return with a time part.
func expectDeliveredOn(f *packFixture, ship, want string) {
	f.t.Helper()
	if got := f.value("Shipment", ship, "delivered_on"); !strings.HasPrefix(got, want) {
		f.t.Fatalf("delivered_on = %q, want %s", got, want)
	}
}

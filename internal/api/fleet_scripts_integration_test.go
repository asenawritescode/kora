//go:build integration

package api

import (
	"net/http"
	"testing"
)

// The Fleet pack as shipped, with its Trip script run the way the Engine runs it. A trip
// holds its vehicle while it is On the road. The Dispatcher may only read vehicles and
// drivers, yet starting and finishing a trip still changes the vehicle's status.
func TestPackScriptsFleet(t *testing.T) {
	eachPackDialect(t, []string{"../../config/fleet"}, func(f *packFixture) {
		runFleetTripRules(f)
	})
}

func runFleetTripRules(f *packFixture) {

	dispatcher := f.as("Dispatcher")

	vehicleA := f.create("Vehicle", `{"registration":"KDA 100A","make":"Isuzu","model":"NPR"}`)
	vehicleB := f.create("Vehicle", `{"registration":"KDB 200B"}`)
	retired := f.create("Vehicle", `{"registration":"KDR 300R","status":"Retired"}`)
	achieng := f.create("Driver", `{"driver_name":"Achieng Wekesa","licence_expiry":"2028-01-31"}`)
	otieno := f.create("Driver", `{"driver_name":"Otieno Odhiambo"}`)
	kamau := f.create("Driver", `{"driver_name":"Kamau Njoroge","licence_expiry":"2026-11-30"}`)
	wanjiru := f.create("Driver", `{"driver_name":"Wanjiru Mwangi","status":"Left"}`)
	mutua := f.create("Driver", `{"driver_name":"Mutua Kilonzo","status":"On leave"}`)
	edge := f.create("Driver", `{"driver_name":"Zawadi Akinyi","licence_expiry":"2026-12-01"}`)

	tripBody := func(reference, vehicle, driver, start string) string {
		return `{"reference":"` + reference + `","vehicle":"` + vehicle + `","driver":"` + driver + `","origin":"Nairobi","destination":"Mombasa","start_date":"` + start + `"}`
	}
	trip := func(reference, vehicle, driver, start string) string {
		return dispatcher.create("Trip", tripBody(reference, vehicle, driver, start))
	}
	var t1, t2 string

	f.t.Run("two planned trips may share a vehicle", func(t *testing.T) {
		t1 = trip("T-1", vehicleA, achieng, "2026-12-01")
		t2 = trip("T-2", vehicleA, otieno, "2026-12-10")
		f.expect("Trip", t1, "status", "Planned")
		f.expect("Trip", t2, "status", "Planned")
		f.expect("Vehicle", vehicleA, "status", "Available")
	})

	f.t.Run("a new trip starts Planned or On the road only", func(t *testing.T) {
		dispatcher.refused(httpMethodPost, "Trip", "", `{"reference":"T-X","vehicle":"`+vehicleB+`","driver":"`+otieno+`","origin":"Nairobi","destination":"Mombasa","start_date":"2026-12-02","status":"Cancelled"}`, "A new trip is Planned or On the road.")
	})

	f.t.Run("a retired vehicle or a driver who has left cannot take a trip", func(t *testing.T) {
		dispatcher.refused(httpMethodPost, "Trip", "", tripBody("T-R", retired, otieno, "2026-12-02"), "is retired")
		dispatcher.refused(httpMethodPut, "Trip", t1, `{"vehicle":"`+retired+`"}`, "is retired")
		dispatcher.refused(httpMethodPost, "Trip", "", tripBody("T-L", vehicleB, wanjiru, "2026-12-02"), "has left")
		dispatcher.refused(httpMethodPut, "Trip", t1, `{"driver":"`+wanjiru+`"}`, "has left")
	})

	f.t.Run("a driver who is not Active cannot start a trip", func(t *testing.T) {
		t3 := trip("T-3", vehicleB, mutua, "2026-12-03")
		dispatcher.refused(httpMethodPut, "Trip", t3, `{"status":"On the road"}`, "is On leave")
	})

	f.t.Run("starting a trip moves its vehicle to On trip", func(t *testing.T) {
		dispatcher.update("Trip", t1, `{"status":"On the road"}`)
		f.expect("Vehicle", vehicleA, "status", "On trip")
	})

	f.t.Run("a vehicle on a trip cannot start a second trip", func(t *testing.T) {
		dispatcher.refused(httpMethodPut, "Trip", t2, `{"status":"On the road"}`, "KDA 100A is On trip.")
	})

	f.t.Run("a driver on a trip on the road cannot start another", func(t *testing.T) {
		t4 := trip("T-4", vehicleB, achieng, "2026-12-04")
		dispatcher.refused(httpMethodPut, "Trip", t4, `{"status":"On the road"}`, "Achieng Wekesa is on trip T-1.")
	})

	f.t.Run("an expired licence blocks starting, not planning", func(t *testing.T) {
		t5 := trip("T-5", vehicleB, kamau, "2026-12-01")
		f.expect("Trip", t5, "status", "Planned")
		dispatcher.refused(httpMethodPut, "Trip", t5, `{"status":"On the road"}`, "Kamau Njoroge's licence expired on 2026-11-30.")
	})

	f.t.Run("a planned trip cannot be delivered, and a trip on the road cannot go back to Planned", func(t *testing.T) {
		dispatcher.refused(httpMethodPut, "Trip", t2, `{"status":"Delivered","end_date":"2026-12-12"}`, "Start the trip first.")
		dispatcher.refused(httpMethodPut, "Trip", t1, `{"status":"Planned"}`, "T-1 is On the road; it cannot become Planned.")
	})

	f.t.Run("delivering a trip releases its vehicle", func(t *testing.T) {
		dispatcher.update("Trip", t1, `{"status":"Delivered","end_date":"2026-12-03"}`)
		f.expect("Vehicle", vehicleA, "status", "Available")
	})

	f.t.Run("a delivered trip cannot change status", func(t *testing.T) {
		dispatcher.refused(httpMethodPut, "Trip", t1, `{"status":"On the road"}`, "T-1 is Delivered; it cannot become On the road.")
		dispatcher.refused(httpMethodPut, "Trip", t1, `{"status":"Cancelled"}`, "T-1 is Delivered; it cannot become Cancelled.")
	})

	f.t.Run("a planned trip can be cancelled without touching its vehicle, and stays cancelled", func(t *testing.T) {
		dispatcher.update("Trip", t2, `{"status":"Cancelled"}`)
		f.expect("Vehicle", vehicleA, "status", "Available")
		dispatcher.refused(httpMethodPut, "Trip", t2, `{"status":"Planned"}`, "T-2 is Cancelled; it cannot become Planned.")
	})

	f.t.Run("a trip created On the road starts on the spot, and cancelling it releases the vehicle", func(t *testing.T) {
		t6 := dispatcher.create("Trip", `{"reference":"T-6","vehicle":"`+vehicleB+`","driver":"`+edge+`","origin":"Nairobi","destination":"Mombasa","start_date":"2026-12-01","status":"On the road"}`)
		f.expect("Vehicle", vehicleB, "status", "On trip")
		dispatcher.update("Trip", t6, `{"status":"Cancelled"}`)
		f.expect("Vehicle", vehicleB, "status", "Available")
	})

	f.t.Run("a dispatcher cannot write a vehicle directly", func(t *testing.T) {
		code, _, doc := dispatcher.send(httpMethodPut, "Vehicle", vehicleA, `{"status":"Available"}`)
		if doc != nil {
			t.Fatalf("dispatcher saved vehicle %s directly", vehicleA)
		}
		if code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Fatalf("dispatcher vehicle write = %d, want 403 or 401", code)
		}
	})

	f.t.Run("a trip on the road keeps its vehicle and driver until it is finished", func(t *testing.T) {
		t7 := trip("T-7", vehicleA, otieno, "2026-12-20")
		dispatcher.update("Trip", t7, `{"status":"On the road"}`)
		f.expect("Vehicle", vehicleA, "status", "On trip")
		dispatcher.refused(httpMethodPut, "Trip", t7, `{"vehicle":"`+vehicleB+`"}`, "Finish or cancel T-7 first.")
		dispatcher.refused(httpMethodPut, "Trip", t7, `{"driver":"`+achieng+`"}`, "Finish or cancel T-7 first.")
		f.expect("Vehicle", vehicleA, "status", "On trip")
		f.expect("Vehicle", vehicleB, "status", "Available")
		dispatcher.update("Trip", t7, `{"status":"Cancelled"}`)
		f.expect("Vehicle", vehicleA, "status", "Available")
	})

	f.t.Run("a trip on the road cannot be deleted, a planned trip can", func(t *testing.T) {
		t8 := trip("T-8", vehicleB, otieno, "2026-12-21")
		dispatcher.update("Trip", t8, `{"status":"On the road"}`)
		f.refused(httpMethodDelete, "Trip", t8, "", "Finish or cancel T-8 first.")
		f.expect("Trip", t8, "status", "On the road")
		f.expect("Vehicle", vehicleB, "status", "On trip")
		t9 := trip("T-9", vehicleA, achieng, "2026-12-22")
		if _, _, doc := f.send(httpMethodDelete, "Trip", t9, ""); doc == nil {
			f.t.Fatalf("planned trip %s was not deleted", t9)
		}
		if rows := f.count("tabTrip", "name = ?", t9); rows != 0 {
			f.t.Fatalf("planned trip %s still has %d rows", t9, rows)
		}
		dispatcher.update("Trip", t8, `{"status":"Cancelled"}`)
		f.expect("Vehicle", vehicleB, "status", "Available")
	})
}

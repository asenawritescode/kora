//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Rooms pack as shipped, on real MySQL: nights and total are calculated from
// the dates and the rate, a booking cannot end before it starts, a rate cannot be
// negative, and a booking that is checked out or cancelled is frozen.
func TestRoomsBookingWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "rooms-bookings")
	c := newPackClient(t, handler, db, reg, site)

	room := c.create("Room", `{"room_no":"101","room_type":"Double","rate_per_night":4500}`)
	if got := c.value("Room", room, "status"); got != "Available" {
		t.Fatalf("new room status = %q, want Available", got)
	}
	if !c.refused(httpMethodPost, "Room", "", `{"room_no":"101"}`) {
		t.Fatal("a second room with the same number was accepted")
	}
	if !c.refused(httpMethodPost, "Room", "", `{"room_no":"102","rate_per_night":-1}`) {
		t.Fatal("a negative room rate was accepted")
	}
	guest := c.create("Guest", `{"guest_name":"Achieng Otieno","phone":"0700000000","id_number":"12345678"}`)

	booking := c.create("Booking", `{"reference":"BK-1","guest":"`+guest+`","room":"`+room+`","check_in":"2026-12-01","check_out":"2026-12-04","rate":4500}`)
	if nights, total, status := c.value("Booking", booking, "nights"), c.value("Booking", booking, "total"), c.value("Booking", booking, "status"); nights != "3" || status != "Booked" || !strings.HasPrefix(total, "13500") {
		t.Fatalf("booking = %s nights, total %s, status %q, want 3 nights, 13500 and Booked", nights, total, status)
	}
	if !c.refused(httpMethodPost, "Booking", "", `{"reference":"BK-1","guest":"`+guest+`","room":"`+room+`","check_in":"2026-12-10","check_out":"2026-12-11","rate":1}`) {
		t.Fatal("a second booking with the same reference was accepted")
	}
	for _, dates := range []string{`"check_in":"2026-12-05","check_out":"2026-12-01"`, `"check_in":"2026-12-05","check_out":"2026-12-05"`} {
		if !c.refused(httpMethodPost, "Booking", "", `{"reference":"BK-BAD","guest":"`+guest+`","room":"`+room+`",`+dates+`,"rate":4500}`) {
			t.Fatalf("a booking with %s was accepted", dates)
		}
	}
	if !c.refused(httpMethodPost, "Booking", "", `{"reference":"BK-NEG","guest":"`+guest+`","room":"`+room+`","check_in":"2026-12-01","check_out":"2026-12-02","rate":-5}`) {
		t.Fatal("a negative booking rate was accepted")
	}

	if c.refused(httpMethodPut, "Booking", booking, `{"status":"Checked in"}`) {
		t.Fatal("checking a guest in was refused")
	}
	if c.refused(httpMethodPut, "Booking", booking, `{"check_out":"2026-12-05"}`) {
		t.Fatal("extending a stay that is checked in was refused")
	}
	if got := c.value("Booking", booking, "nights"); got != "4" {
		t.Fatalf("nights after extending = %s, want 4", got)
	}
	if c.refused(httpMethodPut, "Booking", booking, `{"status":"Checked out"}`) {
		t.Fatal("checking a guest out was refused")
	}
	if !c.refused(httpMethodPut, "Booking", booking, `{"check_out":"2026-12-09"}`) {
		t.Fatal("the dates of a checked-out booking were changed")
	}
	if got := c.value("Booking", booking, "check_out"); !strings.HasPrefix(got, "2026-12-05") {
		t.Fatalf("check out after a refused change = %q, want 2026-12-05", got)
	}

	cancelled := c.create("Booking", `{"reference":"BK-2","guest":"`+guest+`","room":"`+room+`","check_in":"2026-12-20","check_out":"2026-12-22","rate":4500}`)
	if c.refused(httpMethodPut, "Booking", cancelled, `{"status":"Cancelled"}`) {
		t.Fatal("cancelling a booking was refused")
	}
	if !c.refused(httpMethodPut, "Booking", cancelled, `{"room":"`+room+`","check_in":"2026-12-21"}`) {
		t.Fatal("a cancelled booking was changed")
	}
}

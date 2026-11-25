//go:build integration

package api

import (
	"fmt"
	"net/http"
	"testing"
)

const roomsBookingsPack = "../../config/rooms-bookings"

// The Rooms pack's Booking script as shipped, on MySQL and PostgreSQL: an active stay cannot
// overlap another active stay of its room, check-in and check-out move the room's status, and a
// booking's status moves only along the allowed path. Front Desk reads Room only, so its
// check-in and check-out prove the script updates a room that role may not write.
func TestPackScriptsRooms(t *testing.T) {
	eachPackDialect(t, []string{roomsBookingsPack}, func(f *packFixture) {
		guest := f.create("Guest", `{"guest_name":"Achieng Otieno"}`)

		f.t.Run("overlap refused on create", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "201", "4500", "")
			g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			g.refused(httpMethodPost, "Booking", "", stay(guest, room, "2026-12-03", "2026-12-05", ""),
				"Room 201 is booked 2026-12-01 to 2026-12-04")
		})

		f.t.Run("edit into an overlap refused, edit to another room allowed", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "202", "4500", "")
			g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			later := g.create("Booking", stay(guest, room, "2026-12-10", "2026-12-12", ""))
			g.refused(httpMethodPut, "Booking", later, `{"check_in":"2026-12-03","check_out":"2026-12-06"}`,
				"Room 202 is booked 2026-12-01 to 2026-12-04")
			other := roomsRoom(g, "203", "4500", "")
			g.update("Booking", later, fmt.Sprintf(`{"room":%q,"check_in":"2026-12-03","check_out":"2026-12-06"}`, other))
		})

		f.t.Run("back-to-back stays allowed", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "204", "4500", "")
			g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			g.create("Booking", stay(guest, room, "2026-12-04", "2026-12-06", ""))
		})

		f.t.Run("cancelled and checked-out stays are ignored", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "205", "4500", "")
			cancelled := g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			g.update("Booking", cancelled, `{"status":"Cancelled"}`)
			g.create("Booking", stay(guest, room, "2026-12-02", "2026-12-03", ""))
			done := g.create("Booking", stay(guest, room, "2026-12-07", "2026-12-08", ""))
			g.update("Booking", done, `{"status":"Checked in"}`)
			g.update("Booking", done, `{"status":"Checked out"}`)
			g.create("Booking", stay(guest, room, "2026-12-07", "2026-12-08", ""))
		})

		f.t.Run("check-in needs an Available room", func(t *testing.T) {
			g := f.withT(t)
			cleaning := roomsRoom(g, "206", "4500", "Cleaning")
			occupied := roomsRoom(g, "207", "4500", "Occupied")
			outOfService := roomsRoom(g, "208", "4500", "Out of service")
			g.refused(httpMethodPost, "Booking", "", stay(guest, cleaning, "2026-12-01", "2026-12-02", `,"status":"Checked in"`),
				"Room 206 is Cleaning.")
			inOccupied := g.create("Booking", stay(guest, occupied, "2026-12-01", "2026-12-02", ""))
			g.refused(httpMethodPut, "Booking", inOccupied, `{"status":"Checked in"}`, "Room 207 is Occupied.")
			inService := g.create("Booking", stay(guest, outOfService, "2026-12-01", "2026-12-02", ""))
			g.refused(httpMethodPut, "Booking", inService, `{"status":"Checked in"}`, "Room 208 is Out of service.")
		})

		f.t.Run("check-in sets the room Occupied, check-out sets it Cleaning", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "209", "4500", "")
			b := g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			g.expect("Room", room, "status", "Available")
			g.update("Booking", b, `{"status":"Checked in"}`)
			g.expect("Room", room, "status", "Occupied")
			g.update("Booking", b, `{"status":"Checked out"}`)
			g.expect("Room", room, "status", "Cleaning")

			walkIn := roomsRoom(g, "210", "4500", "")
			g.create("Booking", stay(guest, walkIn, "2026-12-01", "2026-12-02", `,"status":"Checked in"`))
			g.expect("Room", walkIn, "status", "Occupied")
		})

		f.t.Run("cancel only from Booked", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "211", "4500", "")
			booked := g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			g.update("Booking", booked, `{"status":"Cancelled"}`)
			g.expect("Booking", booked, "status", "Cancelled")
			g.expect("Room", room, "status", "Available")

			inHouse := g.create("Booking", stay(guest, room, "2026-12-10", "2026-12-12", `,"status":"Checked in"`))
			g.refused(httpMethodPut, "Booking", inHouse, `{"status":"Cancelled"}`, "Check the guest out instead.")
		})

		f.t.Run("other status moves refused", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "212", "4500", "")
			booked := g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			g.refused(httpMethodPut, "Booking", booked, `{"status":"Checked out"}`,
				"A booking cannot change from Booked to Checked out")

			cancelled := g.create("Booking", stay(guest, room, "2026-12-20", "2026-12-22", ""))
			g.update("Booking", cancelled, `{"status":"Cancelled"}`)
			g.refused(httpMethodPut, "Booking", cancelled, `{"status":"Booked"}`,
				"A booking cannot change from Cancelled to Booked")

			inHouse := g.create("Booking", stay(guest, room, "2026-12-05", "2026-12-07", `,"status":"Checked in"`))
			g.refused(httpMethodPut, "Booking", inHouse, `{"status":"Booked"}`,
				"A booking cannot change from Checked in to Booked")
			g.update("Booking", inHouse, `{"status":"Checked out"}`)
			g.refused(httpMethodPut, "Booking", inHouse, `{"status":"Checked in"}`,
				"A booking cannot change from Checked out to Checked in")

			g.refused(httpMethodPost, "Booking", "", stay(guest, room, "2026-12-30", "2027-01-02", `,"status":"Cancelled"`),
				"A new booking must be Booked or Checked in, not Cancelled.")
		})

		f.t.Run("blank rate takes the room rate", func(t *testing.T) {
			g := f.withT(t)
			room := roomsRoom(g, "213", "4500", "")
			copied := g.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", ""))
			if got := g.amount("Booking", copied, "rate"); got != 4500 {
				t.Fatalf("rate of blank-rate booking = %v, want 4500", got)
			}
			if got := g.amount("Booking", copied, "total"); got != 13500 {
				t.Fatalf("total of blank-rate booking = %v, want 13500", got)
			}
			explicit := g.create("Booking", stay(guest, room, "2026-12-10", "2026-12-11", `,"rate":3000`))
			if got := g.amount("Booking", explicit, "rate"); got != 3000 {
				t.Fatalf("explicit rate = %v, want 3000 kept", got)
			}
		})

		f.t.Run("Front Desk updates a room it only reads", func(t *testing.T) {
			g := f.withT(t)
			desk := g.as("Front Desk")
			room := roomsRoom(g, "301", "4500", "")

			walkIn := desk.create("Booking", stay(guest, room, "2026-12-01", "2026-12-04", `,"status":"Checked in"`))
			g.expect("Room", room, "status", "Occupied")
			code, _, doc := desk.send(httpMethodPut, "Room", room, `{"status":"Available"}`)
			if doc != nil || (code != http.StatusForbidden && code != http.StatusUnauthorized) {
				t.Fatalf("Front Desk direct edit of Room = %d, want refused as forbidden", code)
			}
			g.expect("Room", room, "status", "Occupied")

			desk.update("Booking", walkIn, `{"status":"Checked out"}`)
			g.expect("Room", room, "status", "Cleaning")

			next := desk.create("Booking", stay(guest, room, "2026-12-10", "2026-12-12", ""))
			desk.refused(httpMethodPut, "Booking", next, `{"status":"Checked in"}`, "Room 301 is Cleaning.")
			g.update("Room", room, `{"status":"Available"}`)
			desk.update("Booking", next, `{"status":"Checked in"}`)
			g.expect("Room", room, "status", "Occupied")
		})
	})
}

// withT returns the fixture reporting to t, for a subtest.
func (f *packFixture) withT(t *testing.T) *packFixture {
	other := *f
	other.t = t
	return &other
}

// roomsRoom creates a room with a rate; status, when set, is written by a manager afterwards.
func roomsRoom(f *packFixture, no, rate, status string) string {
	f.t.Helper()
	room := f.create("Room", fmt.Sprintf(`{"room_no":%q,"rate_per_night":%s}`, no, rate))
	if status != "" {
		f.update("Room", room, fmt.Sprintf(`{"status":%q}`, status))
	}
	return room
}

// stay is a booking body for a guest, a room and a stay; extra adds fields (with a leading comma).
func stay(guest, room, checkIn, checkOut, extra string) string {
	return fmt.Sprintf(`{"guest":%q,"room":%q,"check_in":%q,"check_out":%q%s}`, guest, room, checkIn, checkOut, extra)
}

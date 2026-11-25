# rooms-bookings

Rooms and stays: Room, Guest and Booking, in the **Rooms** module.
Roles: **Hotel Manager** (everything) and **Front Desk** (reads rooms, creates and edits
guests and bookings, cannot delete). One view, **Rooms Desk** at `/rooms` (current
bookings, and all rooms), and a `rooms_overview` report (booking value and count by status,
rooms by status).

- Room numbers and booking references are unique; rooms, guests and bookings are found by
  number, name or reference.
- A booking must check out after it checks in.
- Nights are counted from the two dates (check out minus check in) and the total is nights
  times the rate; both are calculated and read only.
- Rates cannot be negative.
- Once a booking is Checked out or Cancelled, its guest, room and dates are frozen.
- A booking is active while Booked or Checked in. An active booking cannot overlap another
  active booking of the same room (back-to-back stays are fine); a cancelled or checked-out
  booking is ignored.
- Booking status moves: Booked to Checked in (the room must be Available; it becomes
  Occupied) or Cancelled (only from Booked); Checked in to Checked out (the room becomes
  Cleaning). A new booking starts as Booked or Checked in. Any other move is refused.
- A new booking with a blank rate takes the room's rate per night.
- A checked-in booking keeps its room until checkout, and a checked-in booking cannot be
  deleted.

Limits: rooms, guests and bookings only. Check-in and check-out set a room's status, but
managers set the other changes by hand (Cleaning to Available, Out of service), there is no
housekeeping schedule, and no channel manager or online booking.

Checks: `go test ./internal/site -run Rooms` and `go test -tags integration ./internal/api
-run 'TestPackScriptsRooms|TestRoomsBookingWorkflowOnMySQL'` (needs the MySQL and Postgres
`KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN` variables).

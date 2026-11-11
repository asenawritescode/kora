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

Limits: rooms, guests and bookings only. Double-booking a room is not prevented (there is
no overlap check), a booking does not change the room's status (set by hand), there is no
housekeeping schedule, and no channel manager or online booking.

Checks: `go test ./internal/site -run Rooms`, `go test ./internal/analytics -run Rooms`
and `go test -tags integration ./internal/api -run Rooms` (needs MySQL, set `KORA_TEST_DSN`).

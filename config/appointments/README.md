# appointments

Bookings for a salon or wellness business: Service and Appointment.
Roles: **Salon Manager** (everything, including deleting) and **Stylist** (reads services,
creates and updates appointments, cannot delete). One view, **Appointment Book** at
`/appointments` (upcoming booked appointments and the services table), and an appointments
overview report (appointments by status and by service).

- Service names are unique. A service takes at least 5 minutes and its price cannot be negative.
- Appointment references are unique. Status is Booked, Done, No show or Cancelled, and a new
  appointment starts as Booked; other status values are refused.

Limits: services and appointments only. Double-booking a stylist is not prevented (no overlap
check across records), there are no reminders and there is no online booking page.

Checks: `go test ./internal/site -run Appointments`, `go test ./internal/analytics -run Appointments`
and `go test -tags integration ./internal/api -run Appointments` (needs MySQL, set `KORA_TEST_DSN`).

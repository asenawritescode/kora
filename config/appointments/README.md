# appointments

Bookings for a salon or wellness business: Service and Appointment.
Roles: **Salon Manager** (everything, including deleting) and **Stylist** (reads services,
creates and updates appointments, cannot delete). One view, **Appointment Book** at
`/appointments` (upcoming booked appointments and the services table), and an appointments
overview report (appointments by status and by service).

- Service names are unique. A service takes at least 5 minutes and its price cannot be negative.
- Appointment references are unique. Status is Booked, Done, No show or Cancelled, and a new
  appointment starts as Booked; other status values are refused.
- Script rules (`scripts/Appointment/before_save.js`): a stylist cannot have two Booked
  appointments that overlap (back-to-back is allowed; Cancelled, Done and No show do not
  block). A new appointment, or one whose service changes, must use an offered service.
  Done, No show and Cancelled cannot change to another status.

Limits: services and appointments only. There are no reminders and there is no online booking page.

Checks: `go test ./internal/site -run Appointments` and `go test -tags integration ./internal/api -run 'Appointment|TestPackScriptsAppointments'`
(needs MySQL, set `KORA_TEST_DSN`; the scripts test also takes `KORA_PACK_SCRIPTS_DISPOSABLE_{MYSQL,POSTGRES}_DSN`).

# fleet

Vehicles, drivers and deliveries: Vehicle, Driver, Trip, Fuel Log and Service Record.
Roles: **Fleet Manager** (everything) and **Dispatcher** (reads vehicles and drivers;
creates and changes trips, fuel logs and service records; cannot delete). One view,
**Fleet Overview** at `/fleet` (all vehicles and the open trips), and a `fleet_overview`
report: trips by status, fuel cost by vehicle, distance by vehicle.

- Vehicle registrations are unique; a trip reference is unique. Left blank, it is the trip's number.
- A trip's end date cannot be before its start date, and a Delivered trip must have an end date.
- Once a trip is Delivered or Cancelled, its vehicle, driver, origin and destination are frozen.
- Distance, litres and odometer readings cannot be negative.
- Fuel logs and service records get a readable label (vehicle and date).
- The pack does not link to the People pack: a driver is its own record.
- A trip's status moves Planned → On the road → Delivered, or Planned or On the road → Cancelled.
  A new trip starts Planned or On the road. Delivered and Cancelled trips cannot change status.
- Starting a trip sets its vehicle to On trip, and delivering or cancelling it sets the vehicle
  back to Available. A vehicle or driver cannot start a trip while another trip is On the road
  with it. A retired vehicle, a driver who has left, a driver who is not Active, and a licence
  that expired before the start date all block starting (a retired vehicle or a driver who has
  left also blocks planning). Several planned trips may share a vehicle.

Limits: vehicles, drivers, trips and fuel only. No live GPS tracking, no route planning.
In service is set by a person. Fuel economy and service reminders are not calculated.

Checks: `go test -tags integration ./internal/api -run TestFleet` (needs MySQL, set `KORA_TEST_DSN`),
`go test -tags integration ./internal/api -run TestPackScriptsFleet` (MySQL and PostgreSQL DSNs
`KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`) and
`go test ./internal/site -run Fleet`.

# fleet

Vehicles, drivers and deliveries: Vehicle, Driver, Trip, Fuel Log and Service Record.
Roles: **Fleet Manager** (everything) and **Dispatcher** (reads vehicles and drivers;
creates and changes trips, fuel logs and service records; cannot delete). One view,
**Fleet Overview** at `/fleet` (all vehicles and the open trips), and a `fleet_overview`
report: trips by status, fuel cost by vehicle, distance by vehicle.

- Vehicle registrations are unique; a trip reference is unique.
- A trip's end date cannot be before its start date, and a Delivered trip must have an end date.
- Once a trip is Delivered or Cancelled, its vehicle, driver, origin and destination are frozen.
- Distance, litres and odometer readings cannot be negative.
- Fuel logs and service records get a readable label (vehicle and date).
- The pack does not link to the People pack: a driver is its own record.

Limits: vehicles, drivers, trips and fuel only. No live GPS tracking, no route planning.
Vehicle status (On trip, In service) is set by a person, not by trips. Nothing stops a
vehicle or driver being on two trips at once, and a licence past its expiry date does not
block a trip. Fuel economy and service reminders are not calculated.

Checks: `go test ./internal/site -run TestFleet`, `go test ./internal/analytics -run TestFleet`
and `go test -tags integration ./internal/api -run TestFleet` (needs MySQL, set `KORA_TEST_DSN`).

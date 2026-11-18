# job-cards

Garage and workshop records: Customer Vehicle, Job Card and its child table Job Card Part.
Module name **Workshop**. Roles: **Workshop Manager** (everything, including delete) and
**Mechanic** (reads vehicles and job cards, creates and updates job cards, cannot delete).
One view, **Workshop** at `/workshop` (open job cards and vehicles), and one report,
`job_cards_by_status`. Parts link to `Product`, so this pack needs the Selling pack (`pos-base`).

What is enforced (each point is tested on real MySQL):

- Registration and job card reference are unique. A job card needs a vehicle and an opened date.
- The promised date cannot be before the opened date. Labour charge cannot be negative.
- Saving a **new** job card takes every part's quantity off that product's `stock_qty`, in the same
  all-or-nothing step as the save (a `quantity_effect`, direction decrease, on the `parts` table).
- If any part would take stock below zero, the whole job card is refused: no job card is saved and
  no stock changes.
- A Collected job card cannot change its vehicle or opened date.

What was observed about stock (Engine `internal/kernel/quantity_effect.go`):

- Effects apply on **create only**. Editing a saved job card (changing, adding or removing parts)
  saves the new parts but does not move stock. Deleting a job card does not return the parts.
  Fix stock with a Stock Adjustment from the Stock module.
- The Engine does not enforce constraints on child-table rows, so the `min` on part quantity is
  documentation only. A negative part quantity is accepted and **adds** stock back.

Limits: customer vehicles and job cards that take parts from stock. No quotes or invoices from the
job card, no service reminders. Freezing a Collected card does not freeze its parts or charge, and
status moves (Booked to Collected) are set by a person, not checked.

Checks: `go test ./internal/site -run Workshop`, `go test ./internal/analytics -run Workshop` and
`go test -tags integration ./internal/api -run Workshop` (needs MySQL, set `KORA_TEST_DSN`).

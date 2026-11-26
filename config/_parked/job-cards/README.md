# job-cards

Garage and workshop records: Customer Vehicle, Job Card and its child table Job Card Part.
Module name **Workshop**. Roles: **Workshop Manager** (everything, including delete) and
**Mechanic** (reads vehicles and job cards, creates and updates job cards, cannot delete).
One view, **Workshop** at `/workshop` (open job cards and vehicles), and one dashboard,
`job_cards_insights` (`job_cards_by_status`). Parts link to `Product`, so this pack needs the Selling pack (`erpsell`).

What is enforced (each point is tested on real MySQL):

- Registration and job card reference are unique. A job card needs a vehicle and an opened date.
- The promised date cannot be before the opened date. Labour charge cannot be negative.
- Saving a **new** job card takes every part's quantity off that product's `stock_qty`, in the same
  all-or-nothing step as the save (a `quantity_effect`, direction decrease, on the `parts` table).
- If any part would take stock below zero, the whole job card is refused: no job card is saved and
  no stock changes.
- A Collected job card cannot change its vehicle or opened date.
- A part quantity must be above zero. A new card starts as Booked or In progress.
- Status moves: Booked to In progress; In progress to Waiting for parts or Done; Waiting for parts back to
  In progress; Done to Collected or back to In progress (rework). Collected is final. Other moves are refused
  with the moves allowed.
- A Collected job card cannot change its parts or labour charge, and cannot be deleted.

How stock moves (Engine `internal/kernel/quantity_effect.go` on create, `scripts/Job Card/` after that):

- Saving a **new** job card takes its parts off stock (the card's `quantity_effect`). No stock movement row is written.
- Editing a saved job card moves each product's stock by the change in its quantity: more parts take stock
  off, fewer put it back. A change that would take any product below zero is refused, and nothing changes.
- Deleting a job card gives all its parts back to stock.
- The Engine does not enforce constraints on child-table rows, so the job card script refuses a part
  quantity of zero or less. Stock is changed only through the Product record, so a Stock Adjustment is still
  needed for counts that are not job cards.

Limits: customer vehicles and job cards that take parts from stock. No quotes or invoices from the
job card, no service reminders. A Mechanic can change a saved card's parts: the job card's script reads and moves Product stock for them.

Checks: `go test ./internal/site -run Workshop`, and `go test -tags integration ./internal/api -run TestPackScriptsJobCards`
(MySQL and PostgreSQL through `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).

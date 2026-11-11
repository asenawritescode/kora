# batch-expiry

Dispensing and medicine batches: Patient, Prescription (with Prescription Line rows) and
Medicine Batch. Medicines are `Product` records from the Selling pack (`pos-base`), so this
pack needs pos-base installed. Roles: **Pharmacist** (everything) and **Dispensing
Assistant** (reads patients and batches, creates and edits prescriptions, cannot delete).
One view, **Dispensary** at `/dispensary` (prescriptions waiting, and the batches table),
and a `dispensary_overview` report (prescriptions by status, batches by medicine).

- Prescription references and batch numbers are unique; patients are found by name or phone.
- A prescription line needs a medicine and a quantity; a batch needs an expiry date and cannot
  have a negative quantity.
- Once a prescription is Dispensed, its patient and prescription date are frozen. The
  medicine lines are not frozen, because the Engine cannot yet freeze a child table.

Limits: patients, prescriptions and medicine batches with expiry dates. No controlled-drug
register, no clinical records, no automatic stock deduction by batch, no expiry alerts: the
batches table is sorted by expiry date but nothing warns when a batch is close to expiring.
The "quantity above zero" rule on a prescription line is declared but the Engine does not yet
check field rules on child rows, so a zero quantity is accepted. Dispensing a prescription does not change a product's stock or a batch's quantity. The
Dispensing Assistant cannot register a new patient (a Pharmacist does that).

Checks: `go test ./internal/site -run Dispensary`, `go test ./internal/analytics -run Dispensary`
and `go test -tags integration ./internal/api -run Dispensary` (needs MySQL, set `KORA_TEST_DSN`).

# batch-expiry

Dispensing and medicine batches: Patient, Prescription (with Prescription Line rows) and
Medicine Batch. Medicines are `Product` records from the Selling pack (`erpsell`), so this
pack needs erpsell installed. Roles: **Pharmacist** (everything) and **Dispensing
Assistant** (reads patients and batches, creates and edits prescriptions, cannot delete).
One view, **Dispensary** at `/dispensary` (prescriptions waiting, and the batches table),
and a `batch_expiry_insights` dashboard (prescriptions by status, batches by medicine).

- Prescription references and batch numbers are unique; patients are found by name or phone.
- A prescription line needs a medicine and a quantity; a batch needs an expiry date and cannot
  have a negative quantity.
- Once a prescription is Dispensed, its patient and prescription date are frozen, and so are
  its medicine lines (product, batch, quantity), by the prescription's script.

Dispensing: each line needs a batch of the same medicine that has not expired (expiry on or
after today, UTC date) and enough quantity on hand. Dispensing is all or nothing. Each batch
keeps a **quantity on hand** (quantity received, less what prescriptions dispensed); changing
the quantity received moves it by the same difference and cannot take it below zero.
Cancelling a dispensed prescription returns its quantities. A cancelled prescription is final.

Limits: patients, prescriptions and medicine batches with expiry dates. No controlled-drug
register, no clinical records, no expiry alerts: the
batches table is sorted by expiry date but nothing warns when a batch is close to expiring.
A prescription line's quantity must be above zero (the script refuses zero, because the Engine
does not check field rules on child rows). Batch quantity on hand is kept by these scripts; a
product's stock (`stock_qty`) is not changed here, because stock has its own ledger. The
Dispensing Assistant cannot register a new patient (a Pharmacist does that). The Dispensing
Assistant may only read medicine batches; dispensing still updates them through the script.

Checks: `go test ./internal/site -run Dispensary`, and
`go test -tags integration ./internal/api -run 'TestPackScriptsBatchExpiry|Dispensary'`
(the scripts run on MySQL and PostgreSQL through `KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN`).

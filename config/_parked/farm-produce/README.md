# farm-produce

Produce intake and farmer payments for cooperatives and agribusinesses: Farmer, Produce Delivery
and Farmer Payment.
Roles: **Cooperative Manager** (manages farmers, deliveries and payments) and **Produce Clerk**
(reads farmers, enters deliveries). One view, **Produce Deliveries** at `/produce`, and a produce
insights dashboard `farm_produce_insights` (kilograms by grade and by farmer).

Depends on the Selling pack (`erpsell`): a delivery links to `Product`.

- Farmer member numbers are unique; farmers are found by name, member number, phone, village or group.
- Saving a Produce Delivery raises the product's `stock_qty` by `quantity_kg` in the same
  transaction (`quantity_effect`, increase). A refused delivery (unknown product, zero or
  negative quantity, duplicate reference) changes no stock.
- A saved delivery is fixed: its reference, farmer, product, date, quantity, grade and price cannot
  change (`immutable_after` on every status). The stock it moved must never be edited away. A
  mistake is corrected with a Stock Adjustment (Stock module).
- A delivery is never deleted, not even by the workspace administrator, because its stock would
  stay wrong. The script refuses the delete.
- The amount is quantity times price per kg, computed. The Engine refuses a delivery with a
  missing or zero quantity, and a negative quantity (it would take stock below zero).
- Not enforced by the Engine today (observed on MySQL; the same is true of Product's own
  prices): the `min` constraints on price per kg, and that the Farmer link names an existing
  farmer. The product link is checked, because the stock effect needs a real product. Enter
  prices and farmers carefully until the Engine enforces them.

**Paying farmers.** A farmer payment goes to one farmer. The person says which of that farmer's
deliveries it pays and how much of each (the payment's Deliveries paid rows). Marking the payment
Paid applies the amounts: each delivery's amount paid rises and its status follows (Received, then
Partly Paid, then Paid). Money beyond the listed deliveries is kept as farmer credit (Farmer credit
on the payment). A delivery is payable only while it is Received or Partly Paid, and only by the
farmer who delivered it. Voiding a Paid payment reverses exactly what it applied. A Paid payment's
farmer, amount and deliveries are fixed (void it instead), and it cannot be deleted. A Draft or
Voided payment can be deleted. Status is set by the person only as Draft to Paid and Paid to
Voided; a delivery's status and amount paid are set by payments, not by hand. Cooperative Manager
records payments; Produce Clerk reads them.

Limits: no farmer account balances beyond each payment's credit, no payment run, no traceability by
plot. The Engine does not sum across records, so a farmer's total owed or delivered is only in the
dashboard, not on the farmer.

Checks: `go test ./internal/site -run Produce`, `go test ./internal/site -run Farm` and
`go test -tags integration ./internal/api -run TestPackScriptsFarmProduce` (needs MySQL and
PostgreSQL, set `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).

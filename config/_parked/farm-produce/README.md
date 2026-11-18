# farm-produce

Produce intake for cooperatives and agribusinesses: Farmer and Produce Delivery.
Roles: **Cooperative Manager** (manages farmers and deliveries) and **Produce Clerk** (reads
farmers, enters deliveries). One view, **Produce Deliveries** at `/produce`, and a produce
overview report (kilograms by grade and by farmer, at `/reports/produce`).

Depends on the Selling pack (`pos-base`): a delivery links to `Product`.

- Farmer member numbers are unique; farmers are found by name, member number, phone, village or group.
- Saving a Produce Delivery raises the product's `stock_qty` by `quantity_kg` in the same
  transaction (`quantity_effect`, increase). A refused delivery (unknown product, zero or
  negative quantity, duplicate reference) changes no stock.
- A delivery is append-only. As the Engine really behaves, any later update, including a status
  change, is refused, and neither role can delete one. A mistake is corrected with a Stock
  Adjustment (Stock module). Because of this, `status` (Received or Paid) can only be chosen
  when the delivery is recorded, for example Paid for a cash purchase on the spot. It cannot be
  moved from Received to Paid afterwards.
- The amount is quantity times price per kg, computed. The Engine refuses a delivery with a
  missing or zero quantity, and a negative quantity (it would take stock below zero).
- Not enforced by the Engine today (observed on MySQL; the same is true of Product's own
  prices): the `min` constraints on price per kg, and that the Farmer link names an existing
  farmer. The product link is checked, because the stock effect needs a real product. Enter
  prices and farmers carefully until the Engine enforces them.

Limits: no farmer account balances, no payment run, no traceability by plot. The Engine does
not sum across records, so a farmer's total owed or delivered is only in the report, not on the farmer.

Checks: `go test ./internal/site -run Produce`, `go test ./internal/analytics -run Produce` and
`go test -tags integration ./internal/api -run Produce` (needs MySQL, set `KORA_TEST_DSN`).

# Small Retail Demo

A single-counter shop sample with a product catalogue, POS register, sales and payments, current on-hand quantity, and an append-only stock movement ledger.

## Workspaces

- `/pos` — add products to the basket and complete a cash sale. The transaction creates the Sale and Payment and decrements Product.stock_qty atomically.
- `/stock-movements` — review the stock movement ledger.
- `/stock-movement-entry` — record a receipt, adjustment, or recount with a signed quantity delta and reason. The Engine appends the movement and adjusts the linked product quantity in one idempotent operation.

The sale's line items remain part of the Sale record. The current transaction action reduces stock atomically but does not create a separate Stock Movement row for each sale line; the movement ledger is for receipts and manual adjustments/recounts.

## Demo records

The setup fixture documents the intended demo catalogue. `kora setup` imports schemas and views but does not load `fixtures/demo.yaml`; seed the records after site creation through Kora's authenticated resource API.

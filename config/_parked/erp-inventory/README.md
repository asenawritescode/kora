# erp-inventory

Layer on top of `erpsell` (needs its `Product`; install both). Stock changes you
can trust.

- **Stock Adjustment**: a signed change with a reason (opening balance, count
  correction, damaged, lost, found, other). Saving it changes the product's stock
  in the same step, from any screen, the API or an import. It cannot be edited or
  deleted afterwards; fix a mistake with another adjustment.
- Stock cannot go below zero. A change that would do that is refused and nothing
  is saved.
- Product stock is read-only. It changes only through sales, goods receipts
  (`erp-purchasing`) and stock adjustments, so there is one trail.

How it works: the record type declares a `quantity_effect` rule. The Engine
applies it inside the same all-or-nothing step as the save. See the Engine note in
`kernel/quantity_effect.go`.

- **Posting to the books**: with `erp-accounting` installed, a stock adjustment
  posts its value at the product's cost, in the same save. An increase debits
  Inventory and credits Stock Adjustments; a decrease does the opposite. A product
  with no cost posts nothing. A submitted stock count posts the same way: each
  line's difference at the product's cost, netted into one entry (a net gain
  debits Inventory and credits Stock Adjustments, a net loss does the opposite).
  Without the books, adjustments and counts save as before.

Dashboard `stock_adjustments_insights`: **Stock adjustments** (count and units changed, by reason).

Not here yet: a stock movement ledger (one row per change with before/after),
warehouses, transfers, stock counts, and approval for large adjustments.
Adjustments apply when created; they are not a submit-and-cancel document.

Checks: `go test ./kernel ./doctype`,
`go test -tags integration ./api -run 'TestPackScriptsInventoryPosting|TestPackScriptsStockCount'` (MySQL and PostgreSQL, see
`KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN`) and `go test -tags integration ./api -run 'TestStock|TestGoodsReceipt|TestAdjustment'`.

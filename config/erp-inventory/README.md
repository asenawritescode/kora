# erp-inventory

Layer on top of `pos-base` (needs its `Product`; install both). Stock changes you
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

Not here yet: a stock movement ledger (one row per change with before/after),
warehouses, transfers, stock counts, and approval for large adjustments.
Adjustments apply when created; they are not a submit-and-cancel document.

Checks: `go test ./kernel ./doctype` and
`go test -tags integration ./api -run 'TestStock|TestGoodsReceipt|TestAdjustment'`
(needs MySQL, see `KORA_TEST_DSN`).

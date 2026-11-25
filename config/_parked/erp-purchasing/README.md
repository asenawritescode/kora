# erp-purchasing

Layer on top of `erpsell` (needs its `Currency` and `Product` records; always
install both). Buying from suppliers and paying them, in any currency.

Records: Supplier, Purchase Order (+ item), Goods Receipt (+ item), Supplier Bill,
Supplier Payment.

**Goods Receipt raises stock.** Saving a receipt adds each line's quantity to that
product's stock in the same step (needs the Engine's `quantity_effect`). A receipt
is a fact that goods arrived, so it cannot be edited afterwards; correct it with a
Stock Adjustment (`erp-inventory`). Roles: Buyer (orders, receives) and Purchasing Manager (everything here). The
workspace administrator can do all of it.

Dashboard `purchasing_insights`: **Buying overview** (orders and supplier bills by status, in base currency).

Same money convention as `erpsell`: every money record has `currency` (empty =
base currency) and a frozen `exchange_rate`, and a computed base amount. Rates
are typed in by hand.

**Paying bills.** A supplier payment goes to one supplier. The person says which of
that supplier's open bills it pays and how much of each (the payment's Bills paid
rows; the one-bill shortcut pays up to what that bill still owes). Marking the
payment Paid applies the amounts: each bill's amount paid rises and its status
follows (Partly Paid, then Paid). Money beyond the listed bills is kept as supplier
credit (Supplier credit on the payment). A payment must be in the bill's currency.
Voiding a Paid payment reverses exactly what it applied. A bill with payments cannot
be cancelled or set back to Draft or Open; void its payments first. A bill is Paid
only through payments, and its amount paid is set by payments, not by hand.

Not here yet:
- No three-way matching, no automatic payable postings, no approvals.

Checks: `go test ./doctype -run TestERP` and
`go test -tags integration ./api -run TestERP` (needs MySQL, see `KORA_TEST_DSN`).

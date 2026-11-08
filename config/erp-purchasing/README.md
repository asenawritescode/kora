# erp-purchasing

Layer on top of `pos-base` (needs its `Currency` and `Product` records; always
install both). Buying from suppliers and paying them, in any currency.

Records: Supplier, Purchase Order (+ item), Goods Receipt (+ item), Supplier Bill,
Supplier Payment. Roles: Buyer (orders, receives), Manager (everything, shared
with pos-base).

Same money convention as `pos-base`: every money record has `currency` (empty =
base currency) and a frozen `exchange_rate`, and a computed base amount. Rates
are typed in by hand.

Not here yet:
- Receiving goods does **not** raise stock. That needs the stock-effect-on-the-record
  change in the Engine; until then stock is changed by hand on the Product.
- No three-way matching, no automatic payable postings, no approvals.
- Bills and payments are not linked by a computed balance (the Engine cannot sum
  across records yet); the status is set by a person.

Checks: `go test ./doctype -run TestERP` and
`go test -tags integration ./api -run TestERP` (needs MySQL, see `KORA_TEST_DSN`).

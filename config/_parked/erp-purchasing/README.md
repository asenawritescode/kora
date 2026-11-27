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

**Receiving against an order.** A goods receipt that names a purchase order must
come from that order's supplier, while the order is Ordered or Partly Received. Each
product on the receipt must be on the order and no more than is still due (quantities
across the order's lines for that product). The receipt then fills the order's lines
in line order (Received, per line). The order becomes Partly Received, then Received
when every line is in full. A person cannot set Partly Received or Received, and
cannot change an order's items while anything is received (no line may drop below
what it received). Draft, Ordered and Cancelled stay with the person; Cancelled and
Received are final.

**Bills against receipts and orders.** A bill naming a goods receipt must come from its
supplier, be in its currency and not exceed the receipt's total. A bill naming an order
must come from its supplier, and the order's bills that are not Cancelled, compared in
the base currency, must not exceed the total of its goods receipts. A bill naming both
must name a receipt that belongs to that order.

**Posting to the books.** With `erp-accounting` installed, each purchase event posts a
balanced entry in the same save, by posting role, once. A goods receipt: Dr Inventory,
Cr Goods Received Not Billed (at the receipt's line cost). A recognized bill (Open or
beyond): Dr Goods Received Not Billed when it names a receipt, otherwise Dr Expenses; Cr
Payable; a cancelled unpaid bill reverses. A paid supplier payment: Dr Payable, Cr the
account the money left (Cash, Bank or Cash Clearing by method); voiding it reverses.
Amounts are in the base currency. Without the books, every document still saves and
nothing posts.

Not here yet:
- Simplified posting: no input VAT, withholding tax, FX gain/loss, landed cost, supplier
  advances, settlement discounts or debit notes, and no 3-way-match price variance. These
  need fields the pack does not carry yet (#815 follow-ups).
- No approvals. Matching is by amount and supplier, not by line.

Checks: `go test ./internal/site -run Pack` and
`go test -tags integration ./internal/api -run 'TestPackScripts(PurchasingPayments|PurchaseOrders|PurchasingPosting)'`
(needs MySQL and PostgreSQL, see `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and
`KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).

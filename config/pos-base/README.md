# pos-base

A lean point-of-sale base that keeps its books in one currency and lets every
money record carry its own. It is built from what worked in `retail-shop` (small,
proven register flow: one atomic sale + payment + stock change) and `retail-pos`
(registers, till sessions, cash movements, product categories, sale statuses).

Left out on purpose: M-Pesa and other payment providers, eTIMS, exchange-rate
feeds, journals and accounts (nothing posted to them), stock ledgers (see
"Not here yet").

## What is in it

| Area | Records |
|---|---|
| Setup | Currency |
| Products | Product (category is a fixed list), Product Price (child) |
| Selling | Register, Customer, Sale, Sale Item (child), Payment |
| Cash control | Till Session, Till Count (child), Cash Movement |
| Money out | Expense |

Reports (all in base currency): Sales (by day, currency, status), Payments
received (by currency, method), Expenses (by category, day).

The **Owner Dashboard** view (`/dashboard`) shows the key figures on one screen: number of
sales, sales total, expenses total and number of products, in the base currency. It counts
and adds up every record; it has no date range yet.

Roles: **Manager** (runs the shop, owns currencies and rates) and **Cashier**
(sells, takes payments, works a till). Cashiers cannot change currencies or rates.

## How multiple currencies work

- One **base currency**, chosen once in **Settings** (the site currency). It is not
  listed as a Currency record; the Currency records are the other currencies you
  deal in, each with its rate to the base. Totals, reports and the books are in the base currency. Product prices
  (`selling_price`, `cost_price`) are in the base currency.
- Every money record (Sale, Payment, Cash Movement, Expense) has `currency` and
  `exchange_rate`. **Leave `currency` empty for the base currency.**
- `exchange_rate` is base currency per one unit of that currency. It is **frozen
  on the record**: changing a rate on the Currency record later never moves a past
  sale. When a currency is picked on a form, the rate fills in from the Currency
  record and can be edited.
- `base_total` / `base_amount` are computed (`amount x rate`, two decimals) and are
  what reports add up.
- Rates are typed in by a person on the Currency record. Nothing reads a feed, and
  there is no gain/loss on rate changes.
- A sale can be paid with several Payment records in different currencies.
- Till sessions count cash per currency (Till Count rows).
- A product can carry prices in other currencies (Product Price rows).

## Till sessions

- **The register will not sell without an open till.** The panel shows "Open till"
  (with an opening cash figure) until a Till Session is open. Each sale and its
  Payment are linked to that session, and the session's `expected_cash` goes up by
  the cash taken. The check is on the server: a sale for a missing or closed
  session is refused and nothing is saved.
- **Closing:** set the session's status to Closed and fill in `closing_cash` (the
  count). A closed till takes no more register sales and cannot be edited.
- Sales entered outside the register (back-office entries, old sales) need no
  session, so upgrading keeps every earlier sale editable.

## Limits to know

- **The register screen sells in the base currency.** Its payment panel does not
  yet send a currency or rate, so the Payment it creates is in the base currency.
  Foreign-currency sales and payments are recorded from the Sale and Payment
  forms or the API. Choosing a tender currency on the register needs a small
  change to the register runtime.
- **API callers must send `exchange_rate` with a non-base `currency`.** An empty
  rate counts as 1.
- Product stock is read-only. Set opening stock with a Stock Adjustment from
  `erp-inventory`; a sale takes stock out through the register.
- Amounts keep two decimals. Currencies without minor units, or with three
  (such as JPY or KWD), are not modelled. Exchange rates keep nine decimals.
- The register screen is tied to register code `REG-001` and cash, as in `retail-shop`.
- The panel remembers the open till for the browser session only. After closing the
  browser, "Open till" opens a new session, and the earlier one stays open until
  someone closes it from the Till Session list. Two open tills for one cashier are
  not prevented (the Engine has no composite uniqueness).
- A closed session's status can be edited back to Open by anyone who may edit Till
  Sessions; only its figures are frozen.
- `expected_cash` adds the sale total, so it is right only for base-currency cash
  sales, which is all the register takes.
- Closing needs the counted figure (a document rule), because the Engine does not
  enforce `required_if` or `mandatory_depends_on` on an empty field.
  Sale and Payment keep the register as a code (text), not a link, because a
  Register record's own name is generated and can never be `REG-001`.
- The Engine's `Date` field default `today` breaks table creation on MySQL, so
  dates are filled in by the register action or typed.

## Not here yet

A stock movement ledger (adjustments are in `erp-inventory`), purchasing, returns and refunds, approvals, automatic posting to accounts, a Z-report.

## Checks

- `go test ./doctype -run POSBase` — structure, links and the money convention.
- `go test -tags integration ./api -run TestPOSBase` — loads this pack into MySQL
  and rings up sales through the register action (needs MySQL; set `KORA_TEST_DSN`).

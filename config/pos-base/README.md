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

Roles: **Manager** (runs the shop, owns currencies and rates) and **Cashier**
(sells, takes payments, works a till). Cashiers cannot change currencies or rates.

## How multiple currencies work

- One **base currency** (the Currency record with *Base currency* ticked, rate 1).
  Totals, reports and the books are in the base currency. Product prices
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
  Sale and Payment keep the register as a code (text), not a link, because a
  Register record's own name is generated and can never be `REG-001`.
- The Engine's `Date` field default `today` breaks table creation on MySQL, so
  dates are filled in by the register action or typed.

## Not here yet

A stock movement ledger (adjustments are in `erp-inventory`), purchasing, returns and refunds, approvals, automatic posting to accounts.

## Checks

- `go test ./doctype -run POSBase` — structure, links and the money convention.
- `go test -tags integration ./api -run TestPOSBase` — loads this pack into MySQL
  and rings up sales through the register action (needs MySQL; set `KORA_TEST_DSN`).

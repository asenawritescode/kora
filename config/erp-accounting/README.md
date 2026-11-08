# erp-accounting

Layer on top of `pos-base` (needs its `Currency`; install both). The books:
Account, Accounting Period, Journal Entry (+ Journal Line). Roles: Accountant,
Manager (shared with pos-base).

- Debits must equal credits. An unbalanced entry is rejected (tested).
- Lines are in the entry currency; the entry carries a frozen `exchange_rate` and
  base-currency totals. Rates are typed in by hand; no gain/loss on rate changes.
- Entries can be linked to the record they came from (`source_type`, `source_reference`).

Not here yet: **nothing creates journal entries automatically**. A sale, bill or
payment does not post. That needs the posting work in the Engine (a time-boxed
spike first). Today entries are made by a person. No chart of accounts is
preloaded, no period-close lock, no trial balance report.

Checks: `go test ./doctype -run TestERP`, and
`go test -tags integration ./api -run TestERP` (needs MySQL, see `KORA_TEST_DSN`).

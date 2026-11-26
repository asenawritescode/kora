# erp-accounting

Layer on top of `erpsell` (needs its `Currency`; install both). The books:
Account, Accounting Period, Journal Entry (+ Journal Line). Role: Accountant
(the workspace administrator can do all of it).

- Debits must equal credits. An unbalanced entry is rejected (tested).
- Lines are in the entry currency; the entry carries a frozen `exchange_rate` and
  base-currency totals. Rates are typed in by hand; no gain/loss on rate changes.
- Entries can be linked to the record they came from (`source_type`, `source_reference`).

Rules the scripts keep (`scripts/`):
- Entry: at least two lines; each line is one side (debit or credit, not both, not negative) on an active account that takes postings.
- Period: a blank period is filled from the posting date. Once any period exists, a posting needs an Open period covering its date. Periods cannot overlap; a Closed period can be reopened.
- Posted and reversed entries are frozen (date, period, currency, rate and lines); only the memo changes. Posted can become Reversed; Reversed is final.
- Reversal: Posted to Reversed writes the opposite entry, dated today, in the same save. It is refused if today's period is Closed.
- Posted and reversed entries cannot be deleted. A child account has its parent's type. An account used on a posted entry keeps its type and cannot be deleted.

- Posting roles: an account can be marked as what postings use (Cash, Mobile Money, Bank,
  Receivable, Inventory, Supplier Advances, Payable, Goods Received Not Billed, Tax Payable,
  Cash Clearing, Sales, Cost of Sales, Stock Adjustments, Expenses). One active account per role,
  of the role's type, taking postings.

Not here yet: sales, bills, payments and stock changes posting on their own (#815, built in the
selling, purchasing and stock packs on these roles). No trial balance.

Checks: `go test ./doctype -run TestERP`, `go test ./site -run Pack`, and
`go test -tags integration ./api -run 'TestPackScriptsAccounting|TestERP'` (needs MySQL and Postgres, see `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).

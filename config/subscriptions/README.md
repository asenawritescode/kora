# subscriptions

Plans, subscribers and recurring invoices: Plan, Subscriber, Subscription (+ Subscription Line
as its extra items), Billing Schedule, Recurring Invoice, Subscription Payment, Entitlement and
Usage Record. Roles: **Subscriptions Manager** (sets up plans, everything, can delete) and
**Subscriptions Clerk** (daily records; reads plans). One view, **Subscriptions** at
`/subscriptions`, and a `subscriptions_overview` report (active subscriptions by plan, invoices
by status).

Promoted from `config/v0/subscriptions`, which stays as the legacy copy and its tests. Customer
became **Subscriber** and Payment became **Subscription Payment**, so the pack composes with
Selling in one workspace. A few rules are kept by pack scripts (`scripts/`), which need the
Engine's script runtime (`KORA_SCRIPTS_ENABLED`, on in production); the rest are the engine's own rules.

What the engine enforces:
- A subscription adds its extra items to quantity times the agreed price (`recurring_amount`).
  Dates cannot run backwards. Pausing, past due and cancelling need a reason.
- An invoice works out tax, total, balance and a payment state (Unpaid, Partially Paid, Paid).
  Once Issued, Overdue or Void, its subscription, number, subtotal, tax rate and currency are frozen.
- Saving a Subscription Payment adds the amount to the invoice's amount paid in the same step.
  A payment above the balance, and a payment on a draft or void invoice, is refused. A payment
  cannot be edited afterwards.
- Saving a Usage Record adds its quantity to the entitlement's used quantity, and is refused when
  that passes the allowance.

What the pack scripts keep (`scripts/Subscription`, `scripts/Recurring Invoice`):
- A new subscription takes its plan's currency, billing interval and interval count. A blank or 0
  unit price takes the plan's price, so a free plan gives a subscription at 0. A subscription is
  refused only when its plan has no price at all. Later edits keep the price as saved; a blank
  price is refused.
- A new or re-planned subscription needs an Active plan. Draft and Retired plans are refused.
  Existing subscriptions on a retired plan keep working.
- A new invoice takes its currency from its subscription. A subtotal left at 0 (the default) takes
  the billing schedule's amount, or the subscription's amount per period; a 0 result is fine.
- A new invoice that is not Void invoices its billing schedule: the schedule must belong to the
  invoice's subscription and be Scheduled, and it becomes Invoiced. An invoice created as Void
  leaves its schedule alone.
- Voiding an invoice makes its schedule Scheduled again.
- A Draft invoice can move to another schedule: the old one goes back to Scheduled, and the new one
  must be Scheduled and is invoiced. Once an invoice is issued (or not Draft), its schedule cannot
  change; void it and make a new one.
- Tax, total and balance are worked out again from the copied subtotal, so they match the values
  the engine would derive.

What the scripts did that is not kept: generating invoice and payment numbers (type them),
payment Pending, Failed and Reversed states and reversing a payment (a payment is final; a mistake
is handled outside the pack), and the Quoted-to-Active submit flow (status is a plain Select now,
so Subscription is no longer submittable). Subscription Line is now a table inside the subscription,
so a line has no status of its own; delete the line instead. Usage Record always names an
entitlement and has no status.

Limits: plans, subscribers and recurring invoices are recorded. Invoices are not charged and
nothing bills automatically; no card payments. Nothing creates invoices from the billing
schedule, sends them, marks them overdue, or sums payments across invoices.

Checks: `go test ./internal/site ./internal/analytics -run Subscriptions` and
`go test -tags integration ./internal/api -run 'Subscriptions|TestPackScriptsSubscriptions'`
(needs MySQL and PostgreSQL, set `KORA_TEST_DSN` and the `KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN` variables).

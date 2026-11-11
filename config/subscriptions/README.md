# subscriptions

Plans, subscribers and recurring invoices: Plan, Subscriber, Subscription (+ Subscription Line
as its extra items), Billing Schedule, Recurring Invoice, Subscription Payment, Entitlement and
Usage Record. Roles: **Subscriptions Manager** (sets up plans, everything, can delete) and
**Subscriptions Clerk** (daily records; reads plans). One view, **Subscriptions** at
`/subscriptions`, and a `subscriptions_overview` report (active subscriptions by plan, invoices
by status).

Promoted from `config/v0/subscriptions`, which stays as the legacy copy and its tests. Customer
became **Subscriber** and Payment became **Subscription Payment**, so the pack composes with
Selling in one workspace. The scripts are gone; the engine's own rules replace them.

What is enforced (without scripts):
- A subscription adds its extra items to quantity times the agreed price (`recurring_amount`).
  Dates cannot run backwards. Pausing, past due and cancelling need a reason.
- An invoice works out tax, total, balance and a payment state (Unpaid, Partially Paid, Paid).
  Once Issued, Overdue or Void, its subscription, number, subtotal, tax rate and currency are frozen.
- Saving a Subscription Payment adds the amount to the invoice's amount paid in the same step.
  A payment above the balance, and a payment on a draft or void invoice, is refused. A payment
  cannot be edited afterwards.
- Saving a Usage Record adds its quantity to the entitlement's used quantity, and is refused when
  that passes the allowance.

What the scripts did that is not kept: copying the plan's price and terms onto a new subscription
(the price is typed in, and stays as typed), generating invoice and payment numbers (type them),
filling an invoice from the subscription and its lines (type the subtotal), payment Pending,
Failed and Reversed states and reversing a payment (a payment is final; a mistake is handled
outside the pack), blocking a retired plan, and the Quoted-to-Active submit flow (status is a plain
Select now, so Subscription is no longer submittable). Subscription Line is now a table inside the
subscription, so a line has no status of its own; delete the line instead. Usage Record always
names an entitlement and has no status.

Limits: plans, subscribers and recurring invoices are recorded. Invoices are not charged and
nothing bills automatically; no card payments. Nothing creates invoices from the billing
schedule, sends them, marks them overdue, or sums payments across invoices.

Checks: `go test ./internal/site ./internal/analytics -run Subscriptions` and
`go test -tags integration ./internal/api -run Subscriptions` (needs MySQL, set `KORA_TEST_DSN`).

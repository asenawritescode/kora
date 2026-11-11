# crm

Customers and sales follow-up: Lead, Contact, Company, Deal (+ Deal Item), Sales Quote,
Activity, Qualification, Competitor, Risk and Handoff. Roles: **Sales Rep** (own leads,
contacts and deals) and **Sales Manager** (everything, closes won and lost).
One view, **Sales Pipeline** at `/crm`, and a pipeline performance report.

Promoted from `config/v0/crm`, which stays as the legacy copy and its tests. The Contact
name is built by a computed field, so nothing here needs the script engine.

- A deal adds its lines up (`subtotal`, `total` after the discount). A deal needs a contact.
- The deal stage (Lead, Qualified, Proposal, Negotiation, Closed Won, Closed Lost) is read-only
  on the form and moves from the pipeline view.

Limits: no email sync, no automatic follow-ups or reminders, no forecasting. Quotes are
records, not documents that are sent or signed.

Checks: `go test ./internal/site -run TestCRM` (validation, import, composition with the ERP
modules) and `go test -tags integration ./internal/api -run TestCRM` (needs MySQL, set
`KORA_TEST_DSN`).

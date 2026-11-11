# grants-donors

Donors, grants and reporting deadlines: Donor, Grant and Grant Report.
Roles: **Grants Manager** (everything, including delete) and **Programme Officer** (reads
donors and grants, creates and updates grant reports, cannot delete). One view,
**Grants Overview** at `/grants` (all grants, and reports still Due), and a grants overview
report (grants by status with amount, reports by status).

- Donor names are unique. A grant has a unique reference and an amount of at least 0.01.
- A grant's end date cannot be before its start date.
- A Submitted or Accepted grant report needs a "submitted on" date.
- Once a grant is Closed or Declined, its donor and amount are frozen.
- A grant report links to its grant through the field `grant_ref`: `grant` is a reserved word in MySQL and the Engine does not quote it in every query, so a column named `grant` fails on save.

Limits: donors, grants and reporting deadlines only. Funds spent against a grant are not
tracked, there are no donor statements, and nothing reminds anyone of a deadline. The
Engine cannot enforce one reference per report period (no composite uniqueness).

Checks: `go test ./internal/site -run Grants`, `go test ./internal/analytics -run Grants`
and `go test -tags integration ./internal/api -run Grants` (needs MySQL, set `KORA_TEST_DSN`).

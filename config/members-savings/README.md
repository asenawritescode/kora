# members-savings

A member register with savings entries and loans for savings societies and small lenders:
Member, Savings Entry, Loan and Loan Repayment.
Roles: **SACCO Manager** (everything except deleting) and **Teller** (reads members and
loans; records savings entries and loan repayments; cannot edit or delete). One view,
**Members Overview** at `/members`, and a `members_overview` report (members by status,
loans by status with the amount).

**What this pack does NOT do: balances, interest and loan schedules are NOT calculated, and
nothing posts to the books.** A member's savings balance is not worked out from their
entries, a withdrawal is not checked against savings, a loan's interest and repayment
schedule are not produced, and a loan's outstanding balance is not tracked. The Engine
cannot sum across records. This is a register of what was recorded, not a ledger.

- Member numbers are unique. Status is Active, Dormant or Exited.
- A **Savings Entry** (Deposit or Withdrawal) and a **Loan Repayment** are append-only: they
  cannot be edited after saving. Correct a mistake with an opposite entry.
- A **Loan** has a unique reference, an amount, an interest rate and a term in months. Once
  it is Repaid or Rejected, its member, amount, interest rate and term are frozen.
- Observed on MySQL: amounts must be at least 0.01, the interest rate cannot be negative and
  the term must be at least one month.

Limits: no balances, no interest, no schedules, no accounting posting. Marking a loan
Approved, Disbursed or Repaid is a status a person sets; nothing checks it against
repayments received. Repayments are not checked against the loan amount.

Checks: `go test ./internal/site ./internal/analytics -run Members` and
`go test -tags integration ./internal/api -run Members` (needs MySQL, set `KORA_TEST_DSN`).

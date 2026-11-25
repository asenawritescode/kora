# members-savings

A member register with savings entries and loans for savings societies and small lenders:
Member, Savings Entry, Loan and Loan Repayment.
Roles: **SACCO Manager** (everything except deleting) and **Teller** (reads members and
loans; records savings entries and loan repayments; cannot edit or delete). One view,
**Members Overview** at `/members`, and a `members_overview` report (members by status,
loans by status with the amount).

What the pack keeps:

- **Savings balance** on each Member: deposits less withdrawals. A withdrawal above the balance
  is refused, and a member with status Exited takes no new entries.
- **Amount repaid and outstanding** on each Loan. A Loan Repayment is refused unless the loan is
  Disbursed and the payment does not exceed what is still owed.
- **Repaid** is set only by repayments: when they cover the loan, it becomes Repaid in the same
  save. A person cannot set Repaid by hand.
- A new loan starts as Applied, Approved or Rejected. A person moves it Applied to Approved or
  Rejected, and Approved to Rejected or Disbursed. Once Disbursed, its member, amount, interest
  rate and term are frozen; once Repaid or Rejected, the whole loan is frozen.
- Member numbers are unique. Status is Active, Dormant or Exited.
- A **Savings Entry** and a **Loan Repayment** are append-only: they cannot be edited or deleted
  after saving. Correct a mistake with an opposite entry.
- Observed on MySQL: amounts must be at least 0.01, the interest rate cannot be negative and
  the term must be at least one month.

What it still does NOT do: interest, repayment schedules, and posting to the books.

Checks: `go test ./internal/site -run Members` (publish path) and
`go test -tags integration ./internal/api -run 'TestPackScriptsSavings|TestMembersSavings'`
(set `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`,
and `KORA_TEST_DSN` for the MySQL-only workflow test).

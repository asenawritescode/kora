# people

Staff records and time off: Department, Employee, Leave Type, Leave Request and Attendance.
Roles: **HR Manager** (everything, decides leave) and **HR User** (reads people records,
enters leave requests and attendance). One view, **People Directory** at `/people`, and a
people overview report.

- An employee's name is a computed field (first and last name). Employee numbers are unique.
- A leave request counts its own days and the last day cannot be before the first. Once a
  request is Approved or Rejected, its employee, leave type and dates are frozen.
- Attendance is one record per employee per day, entered by hand.

Limits: no payroll, no statutory deductions, no leave balances (the Engine cannot sum across
records), and one attendance entry per employee per day is not enforced (no composite
uniqueness). Approval is a status a person sets; nothing routes the request to an approver.

Checks: `go test ./internal/site -run TestPeople`, `go test ./internal/analytics -run TestPeople`
and `go test -tags integration ./internal/api -run TestPeople` (needs MySQL, set `KORA_TEST_DSN`).

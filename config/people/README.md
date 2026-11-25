# people

Staff records and time off: Department, Employee, Leave Type, Leave Request and Attendance.
Roles: **HR Manager** (everything, decides leave) and **HR User** (reads people records,
enters leave requests and attendance). One view, **People Directory** at `/people`, and a
people overview report.

- An employee's name is a computed field (first and last name). Employee numbers are unique.
- A leave request counts its own days and the last day cannot be before the first. Once a
  request is Approved or Rejected, its employee, leave type and dates are frozen.
- Attendance is one record per employee per day, entered by hand.
- A pending or approved leave request may not overlap another pending or approved request of the same employee. Requests that meet end to start (one ends the day before the other starts) are fine.
- Approving a request checks the leave type's yearly allowance (days per year, when set): approved days of that type that start in the same calendar year, plus this request, may not exceed it. A leave type without days per year has no cap.
- Approving a request that covers today moves an Active employee to On leave. Cancelling an approved request brings an employee who is On leave back to Active, unless another approved request covers today.
- Pending can become Approved, Rejected or Cancelled. Approved can only become Cancelled. Rejected and Cancelled are final.
- An attendance day inside an approved leave request cannot be Present; set On leave instead. Absent, Late and Half day are kept as entered.

Limits: no payroll, no statutory deductions, no leave balances beyond the yearly cap (the Engine cannot sum across records), and one attendance entry per employee per day is not enforced (no composite uniqueness). Approval is a status a person sets; nothing routes the request to an approver, and who may approve is a permission matter. Leave that starts in the future does not flip the employee On leave on that day: nothing runs on a schedule. Attendance entered before a leave is approved is not changed.

Checks: `go test ./internal/site -run TestPeople`, and `go test -tags integration ./internal/api -run 'TestPackScriptsPeople|TestPeople' -v` with `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN` set (see `internal/api/people_scripts_integration_test.go`).

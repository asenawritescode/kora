# students

Classes, students and a class register: Class, Student and Class Register Entry.
Roles: **School Administrator** (everything) and **Teacher** (reads classes and students,
creates and edits class register entries, deletes nothing). One view, **Students Register**
at `/students`, and a students overview report (students by class, register entries by status).

- Class names and admission numbers are unique. A student's class is a link to a Class.
- A register entry needs a student and a date; status is Present, Absent or Late.

Limits: no marks or report cards, no timetable, no fee statements (use Subscriptions for
recurring fees). One register entry per student per day is not enforced (no composite
uniqueness), a Select value outside its list is not refused by the Engine, and a class teacher is a label, not a restriction on who may edit a class.

Checks: `go test ./internal/site -run Students`, `go test ./internal/analytics -run Students`
and `go test -tags integration ./internal/api -run Students` (needs MySQL, set `KORA_TEST_DSN`).

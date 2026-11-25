# projects

Plan work and track tasks: Project, Task, Milestone and Timesheet Entry.
Roles: **Project Manager** (everything, including delete) and **Project Member** (reads
projects and milestones, creates and updates tasks and timesheet entries, cannot delete).
One view, **Projects Board** at `/projects` (a task board by status plus a table of projects),
and a `projects_overview` report (tasks by status, hours by project).

- A project's name is unique and its due date cannot be before its start date.
- Once a project is Done or Cancelled, its name and client are frozen.
- A timesheet entry is 0.25 to 24 hours; anything outside is refused.
- Tasks, milestones and timesheet entries link to a project; timesheet entries also link to a task.

Enforced by scripts (`scripts/`):
- A project cannot be set to Done or Cancelled while any of its tasks is not Done (Blocked counts as open). Tasks are not closed for you.
- A task cannot be added to, or moved into, a project that is Done or Cancelled. Changing a task's status in its own project is always allowed.
- A timesheet entry's project must be its task's project. New entries on a Done or Cancelled project are refused; hours must be more than 0 and at most 24.
- New milestones cannot be added to a Done or Cancelled project.

Limits: projects, tasks and time. No Gantt chart, no task dependencies, no automatic budget
roll-up (the Engine cannot sum across records into a project, so budget is not compared with
hours). A timesheet entry with a blank project is refused by the required-field check, so it
is never filled in from its task.

Constraint limits are written as decimals (`24.0`, not `24`): the Engine reads whole-number YAML
values as unsigned integers and then skips max constraints (filed as a separate issue).

Checks: `go test -tags integration ./internal/api -run TestPackScriptsProjects` (MySQL and PostgreSQL, see `KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN`), `go test ./internal/site -run TestProjects`, `go test ./internal/analytics -run TestProjects`
and `go test -tags integration ./internal/api -run TestProjects` (needs MySQL, set `KORA_TEST_DSN`).

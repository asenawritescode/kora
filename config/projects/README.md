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

Limits: projects, tasks and time. No Gantt chart, no task dependencies, no automatic budget
roll-up (the Engine cannot sum across records into a project, so budget is not compared with
hours). Closing a project does not close its tasks, and a timesheet entry's project is not
checked against its task's project.

Constraint limits are written as decimals (`24.0`, not `24`): the Engine reads whole-number YAML
values as unsigned integers and then skips max constraints (filed as a separate issue).

Checks: `go test ./internal/site -run TestProjects`, `go test ./internal/analytics -run TestProjects`
and `go test -tags integration ./internal/api -run TestProjects` (needs MySQL, set `KORA_TEST_DSN`).

//go:build integration

package api

import (
	"testing"
)

// The projects pack as shipped, with its scripts run the way the Engine runs them: a project
// closes only when its tasks are Done, tasks and time follow their project's state, and a
// Project Member can log work on an open project but cannot touch the project itself.
const projectsPack = "../../config/projects"

func TestPackScriptsProjects(t *testing.T) {
	eachPackDialect(t, []string{projectsPack}, func(f *packFixture) {
		runProjectsScenario(f)
	})
}

func runProjectsScenario(f *packFixture) {
	t := f.t
	member := f.as("Project Member")

	var p1, t1, t2, p3, t3, entry1 string
	t.Run("setup: an active project with two open tasks and a logged entry", func(t *testing.T) {
		p1 = f.create("Project", `{"project_name":"Clinic extension","status":"Active"}`)
		t1 = f.create("Task", `{"task_title":"Pour foundation","project":"`+p1+`"}`)
		t2 = f.create("Task", `{"task_title":"Order roofing sheets","project":"`+p1+`"}`)
		entry1 = f.create("Timesheet Entry", `{"task":"`+t1+`","project":"`+p1+`","entry_date":"2026-12-03","hours":2}`)
	})

	t.Run("closing a project with open tasks is refused and lists them", func(t *testing.T) {
		f.refused(httpMethodPut, "Project", p1, `{"status":"Done"}`, "Clinic extension has 2 open tasks: Order roofing sheets, Pour foundation.")
		f.refused(httpMethodPut, "Project", p1, `{"status":"Cancelled"}`, "has 2 open tasks")
		f.expect("Project", p1, "status", "Active")
	})

	t.Run("a Blocked task still counts as open", func(t *testing.T) {
		f.update("Task", t1, `{"status":"Blocked"}`)
		f.update("Task", t2, `{"status":"Done"}`)
		f.refused(httpMethodPut, "Project", p1, `{"status":"Done"}`, "has 1 open task: Pour foundation.")
	})

	t.Run("closing is allowed once every task is Done", func(t *testing.T) {
		f.update("Task", t1, `{"status":"Done"}`)
		f.update("Project", p1, `{"status":"Done"}`)
		f.expect("Project", p1, "status", "Done")
	})

	t.Run("saving a Done project again does not re-run the open-task check", func(t *testing.T) {
		f.update("Project", p1, `{"notes":"Handed over"}`)
		f.expect("Project", p1, "status", "Done")
	})

	t.Run("a project can be created as Cancelled with no tasks", func(t *testing.T) {
		p2 := f.create("Project", `{"project_name":"Dropped bid","status":"Cancelled"}`)
		f.expect("Project", p2, "status", "Cancelled")
	})

	t.Run("a task cannot be added to a Done project", func(t *testing.T) {
		f.refused(httpMethodPost, "Task", "", `{"task_title":"Late add","project":"`+p1+`"}`, "Clinic extension is Done; its tasks cannot be added or moved there.")
	})

	t.Run("an existing task on a Done project can still change status", func(t *testing.T) {
		f.update("Task", t2, `{"status":"In progress"}`)
		f.expect("Task", t2, "status", "In progress")
	})

	t.Run("moving a task into a Cancelled project is refused", func(t *testing.T) {
		p3 = f.create("Project", `{"project_name":"Roof works","status":"Active"}`)
		t3 = f.create("Task", `{"task_title":"Fit gutters","project":"`+p3+`"}`)
		cancelled := f.create("Project", `{"project_name":"Old bid","status":"Cancelled"}`)
		f.refused(httpMethodPut, "Task", t3, `{"project":"`+cancelled+`"}`, "Old bid is Cancelled")
		f.expect("Task", t3, "project", p3)
	})

	t.Run("timesheet entry must match its task's project", func(t *testing.T) {
		f.refused(httpMethodPost, "Timesheet Entry", "", `{"task":"`+t3+`","project":"`+p1+`","entry_date":"2026-12-03","hours":1}`, "Fit gutters belongs to "+p3)
	})

	t.Run("timesheet hours must be more than 0 and at most 24", func(t *testing.T) {
		for _, hours := range []string{"0", "0.1", "24.5", "25"} {
			f.refused(httpMethodPost, "Timesheet Entry", "", `{"task":"`+t3+`","project":"`+p3+`","entry_date":"2026-12-03","hours":`+hours+`}`, "")
		}
		f.create("Timesheet Entry", `{"task":"`+t3+`","project":"`+p3+`","entry_date":"2026-12-03","hours":0.25}`)
		f.create("Timesheet Entry", `{"task":"`+t3+`","project":"`+p3+`","entry_date":"2026-12-03","hours":24}`)
	})

	t.Run("a new timesheet entry on a Done project is refused", func(t *testing.T) {
		f.refused(httpMethodPost, "Timesheet Entry", "", `{"task":"`+t1+`","project":"`+p1+`","entry_date":"2026-12-04","hours":1}`, "Time cannot be logged to Clinic extension: it is Done.")
	})

	t.Run("an existing entry on a Done project can still be corrected", func(t *testing.T) {
		f.update("Timesheet Entry", entry1, `{"hours":2.5}`)
		if got := f.amount("Timesheet Entry", entry1, "hours"); got != 2.5 {
			t.Fatalf("corrected hours = %v, want 2.5", got)
		}
	})

	t.Run("a new milestone on a Done project is refused, on an open one allowed", func(t *testing.T) {
		f.refused(httpMethodPost, "Milestone", "", `{"milestone_name":"Handover","project":"`+p1+`","due_date":"2027-01-31"}`, "Milestones cannot be added to Clinic extension: it is Done.")
		f.create("Milestone", `{"milestone_name":"Roof on","project":"`+p3+`","due_date":"2027-01-31"}`)
	})

	t.Run("project member logs time on an open project", func(t *testing.T) {
		task := member.create("Task", `{"task_title":"Hang doors","project":"`+p3+`"}`)
		logged := member.create("Timesheet Entry", `{"task":"`+task+`","project":"`+p3+`","entry_date":"2026-12-05","hours":3}`)
		f.expect("Timesheet Entry", logged, "project", p3)
	})

	t.Run("project member cannot log time against the wrong project", func(t *testing.T) {
		task := member.create("Task", `{"task_title":"Paint walls","project":"`+p3+`"}`)
		member.refused(httpMethodPost, "Timesheet Entry", "", `{"task":"`+task+`","project":"`+p1+`","entry_date":"2026-12-05","hours":1}`, "")
	})

	t.Run("project member cannot add tasks or entries to a Done project", func(t *testing.T) {
		member.refused(httpMethodPost, "Task", "", `{"task_title":"Too late","project":"`+p1+`"}`, "its tasks cannot be added")
		member.refused(httpMethodPost, "Timesheet Entry", "", `{"task":"`+t1+`","project":"`+p1+`","entry_date":"2026-12-06","hours":1}`, "Time cannot be logged")
	})

	t.Run("project member cannot edit the project or add milestones (permission, not script)", func(t *testing.T) {
		member.refused(httpMethodPut, "Project", p3, `{"notes":"Changed by member"}`, "not permitted")
		member.refused(httpMethodPost, "Milestone", "", `{"milestone_name":"Member milestone","project":"`+p3+`","due_date":"2027-02-01"}`, "not permitted")
	})
}

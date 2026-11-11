//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Projects pack as shipped, on real MySQL: a project with tasks and time,
// hours outside 0.25 to 24 refused, a due date before the start refused, and a
// finished project's name frozen.
func TestProjectsTasksAndTimesheetWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "projects")
	c := newPackClient(t, handler, db, reg, site)

	project := c.create("Project", `{"project_name":"Clinic extension","client":"Mwangi Holdings","start_date":"2026-12-01","due_date":"2027-02-28","budget":850000}`)
	if got := c.value("Project", project, "status"); got != "Planning" {
		t.Fatalf("new project status = %q, want Planning", got)
	}
	if !c.refused(httpMethodPost, "Project", "", `{"project_name":"Clinic extension"}`) {
		t.Fatal("a second project with the same name was accepted")
	}
	if !c.refused(httpMethodPost, "Project", "", `{"project_name":"Backwards","start_date":"2026-12-10","due_date":"2026-12-01"}`) {
		t.Fatal("a project due before it starts was accepted")
	}

	foundation := c.create("Task", `{"task_title":"Pour foundation","project":"`+project+`","priority":"High","estimate_hours":40}`)
	c.create("Task", `{"task_title":"Order roofing sheets","project":"`+project+`"}`)
	if got := c.value("Task", foundation, "status"); got != "To do" {
		t.Fatalf("new task status = %q, want To do", got)
	}
	if c.refused(httpMethodPut, "Task", foundation, `{"status":"In progress"}`) {
		t.Fatal("moving a task to In progress was refused")
	}
	c.create("Milestone", `{"milestone_name":"Roof on","project":"`+project+`","due_date":"2027-01-31"}`)

	entry := func(hours string) string {
		return `{"task":"` + foundation + `","project":"` + project + `","entry_date":"2026-12-03","hours":` + hours + `}`
	}
	logged := c.create("Timesheet Entry", entry("7.5"))
	if got := c.value("Timesheet Entry", logged, "hours"); !strings.HasPrefix(got, "7.5") {
		t.Fatalf("logged hours = %q, want 7.5", got)
	}
	for _, hours := range []string{"0", "25", "24.5"} {
		if !c.refused(httpMethodPost, "Timesheet Entry", "", entry(hours)) {
			t.Fatalf("%s hours was accepted", hours)
		}
	}

	if c.refused(httpMethodPost, "Timesheet Entry", "", entry("24")) {
		t.Fatal("24 hours was refused")
	}
	if !c.refused(httpMethodPost, "Project", "", `{"project_name":"Free","budget":-5}`) {
		t.Fatal("a negative budget was accepted")
	}
	if c.refused(httpMethodPut, "Project", project, `{"project_name":"Clinic extension phase 1","status":"Active"}`) {
		t.Fatal("renaming an active project was refused")
	}
	if c.refused(httpMethodPut, "Project", project, `{"status":"Done"}`) {
		t.Fatal("finishing the project was refused")
	}
	if !c.refused(httpMethodPut, "Project", project, `{"project_name":"Renamed after the fact"}`) {
		t.Fatal("the name of a Done project was changed")
	}
	if !c.refused(httpMethodPut, "Project", project, `{"client":"Someone else"}`) {
		t.Fatal("the client of a Done project was changed")
	}
	if got := c.value("Project", project, "project_name"); got != "Clinic extension phase 1" {
		t.Fatalf("project name after refused changes = %q", got)
	}
}

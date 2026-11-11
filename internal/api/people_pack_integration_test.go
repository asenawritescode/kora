//go:build integration

package api

import (
	"strings"
	"testing"
)

// The People pack as shipped, on real MySQL: an employee gets a readable name,
// leave counts its days and cannot run backwards, a decided request is frozen.
func TestPeopleEmployeeLeaveAndAttendanceWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "people")
	c := newPackClient(t, handler, db, reg, site)

	department := c.create("Department", `{"department_name":"Sales"}`)
	employee := c.create("Employee", `{"employee_no":"E-1","first_name":"Wanjiru","last_name":"Kamau","department":"`+department+`","job_title":"Cashier"}`)
	if got := c.value("Employee", employee, "full_name"); got != "Wanjiru Kamau" {
		t.Fatalf("employee name = %q, want %q", got, "Wanjiru Kamau")
	}
	if !c.refused(httpMethodPost, "Employee", "", `{"employee_no":"E-1","first_name":"Other","last_name":"Person"}`) {
		t.Fatal("a second employee with the same number was accepted")
	}

	annual := c.create("Leave Type", `{"leave_type_name":"Annual","days_per_year":21}`)
	request := c.create("Leave Request", `{"reference":"LR-1","employee":"`+employee+`","leave_type":"`+annual+`","start_date":"2026-12-01","end_date":"2026-12-03"}`)
	if days, status := c.value("Leave Request", request, "days"), c.value("Leave Request", request, "status"); days != "3" || status != "Pending" {
		t.Fatalf("leave request = %s days, status %q, want 3 days and Pending", days, status)
	}
	if !c.refused(httpMethodPost, "Leave Request", "", `{"reference":"LR-BAD","employee":"`+employee+`","leave_type":"`+annual+`","start_date":"2026-12-05","end_date":"2026-12-01"}`) {
		t.Fatal("leave that ends before it starts was accepted")
	}

	if c.refused(httpMethodPut, "Leave Request", request, `{"status":"Approved","decision_note":"Enjoy"}`) {
		t.Fatal("approving a pending request was refused")
	}
	if !c.refused(httpMethodPut, "Leave Request", request, `{"end_date":"2026-12-10"}`) {
		t.Fatal("the dates of an approved request were changed")
	}
	if got := c.value("Leave Request", request, "end_date"); !strings.HasPrefix(got, "2026-12-03") {
		t.Fatalf("end date after a refused change = %q, want 2026-12-03", got)
	}

	entry := c.create("Attendance", `{"employee":"`+employee+`","attendance_date":"2026-12-01","status":"Late"}`)
	if got := c.value("Attendance", entry, "label"); got == "" {
		t.Fatal("attendance entry has no readable label")
	}
}

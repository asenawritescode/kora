//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Students pack as shipped, on real MySQL: classes and students are unique
// by name and admission number, a register entry keeps its date and status,
// and records missing required fields are refused.
func TestStudentsClassRegisterWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "students")
	c := newPackClient(t, handler, db, reg, site)

	class := c.create("Class", `{"class_name":"Form 1 North","year":2026}`)
	if !c.refused(httpMethodPost, "Class", "", `{"class_name":"Form 1 North"}`) {
		t.Fatal("a second class with the same name was accepted")
	}
	if !c.refused(httpMethodPost, "Class", "", `{"year":2026}`) {
		t.Fatal("a class without a name was accepted")
	}

	student := c.create("Student", `{"admission_no":"A-001","student_name":"Amina Otieno","class_group":"`+class+`","guardian_name":"J. Otieno","guardian_phone":"0700000000"}`)
	if got := c.value("Student", student, "status"); got != "Enrolled" {
		t.Fatalf("new student status = %q, want Enrolled", got)
	}
	if got := c.value("Student", student, "class_group"); got != class {
		t.Fatalf("student class = %q, want %q", got, class)
	}
	if !c.refused(httpMethodPost, "Student", "", `{"admission_no":"A-001","student_name":"Someone Else"}`) {
		t.Fatal("a second student with the same admission number was accepted")
	}

	entry := c.create("Class Register Entry", `{"student":"`+student+`","entry_date":"2026-12-01"}`)
	if got := c.value("Class Register Entry", entry, "status"); got != "Present" {
		t.Fatalf("new register entry status = %q, want Present", got)
	}
	if c.refused(httpMethodPut, "Class Register Entry", entry, `{"status":"Late","note":"Bus delay"}`) {
		t.Fatal("changing an entry to Late was refused")
	}
	if got := c.value("Class Register Entry", entry, "status"); got != "Late" {
		t.Fatalf("entry status = %q, want Late", got)
	}
	if got := c.value("Class Register Entry", entry, "entry_date"); !strings.HasPrefix(got, "2026-12-01") {
		t.Fatalf("entry date = %q, want 2026-12-01", got)
	}
	if !c.refused(httpMethodPost, "Class Register Entry", "", `{"student":"`+student+`"}`) {
		t.Fatal("a register entry without a date was accepted")
	}

	if c.refused(httpMethodPut, "Student", student, `{"status":"Graduated"}`) {
		t.Fatal("graduating a student was refused")
	}
	if got := c.value("Student", student, "status"); got != "Graduated" {
		t.Fatalf("student status = %q, want Graduated", got)
	}
}

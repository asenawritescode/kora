//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// The People pack as shipped, with its Leave Request and Attendance scripts run the way the
// Engine runs them: leave decisions, the yearly allowance, the employee's On leave status and
// attendance on approved leave, on MySQL and PostgreSQL.
const peoplePack = "../../config/people"

// peopleScripts is the people pack fixture plus a counter that keeps unique names apart.
type peopleScripts struct {
	*packFixture
	n int
}

func TestPackScriptsPeople(t *testing.T) {
	eachPackDialect(t, []string{peoplePack}, func(f *packFixture) {
		p := &peopleScripts{packFixture: f}
		p.run()
	})
}

func (p *peopleScripts) run() {
	t := p.t
	t.Run("a new request starts Pending with its days counted", func(t *testing.T) {
		e := p.employee()
		lr := p.request(e, p.leaveType(0), "2030-01-07", "2030-01-09")
		p.expect("Leave Request", lr, "status", "Pending")
		p.expect("Leave Request", lr, "days", "3")
	})
	t.Run("a request for an employee who has left is refused", func(t *testing.T) {
		e := p.employee()
		p.update("Employee", e, `{"status":"Left"}`)
		p.refusedRequest(e, p.leaveType(0), "2030-02-01", "2030-02-02", "has left")
	})
	t.Run("a new request starts Pending or Approved only", func(t *testing.T) {
		e := p.employee()
		body := fmt.Sprintf(`{"employee":%q,"leave_type":%q,"start_date":"2030-02-01","end_date":"2030-02-02","status":"Rejected"}`, e, p.leaveType(0))
		p.refused(httpMethodPost, "Leave Request", "", body, "A new leave request is Pending or Approved")
	})
	t.Run("pending and approved requests cannot overlap", func(t *testing.T) {
		e := p.employee()
		lt := p.leaveType(0)
		p.request(e, lt, "2030-12-01", "2030-12-03")
		p.refusedRequest(e, lt, "2030-12-03", "2030-12-05", "overlap")
		adjacent := p.request(e, lt, "2030-12-04", "2030-12-05")
		p.update("Leave Request", adjacent, `{"status":"Approved"}`)
		p.expect("Leave Request", adjacent, "status", "Approved")
	})
	t.Run("approval checks the yearly allowance of the leave type", func(t *testing.T) {
		e := p.employee()
		lt := p.leaveType(5)
		p.update("Leave Request", p.request(e, lt, "2030-01-10", "2030-01-12"), `{"status":"Approved"}`)
		second := p.request(e, lt, "2030-02-01", "2030-02-03")
		p.refused(httpMethodPut, "Leave Request", second, `{"status":"Approved"}`, "has 2 of 5")
		p.update("Leave Request", second, `{"status":"Rejected"}`)
		short := p.request(e, lt, "2030-02-01", "2030-02-02")
		p.update("Leave Request", short, `{"status":"Approved"}`)
		p.expect("Leave Request", short, "status", "Approved")
	})
	t.Run("the yearly allowance counts only days of the same leave type", func(t *testing.T) {
		e := p.employee()
		uncapped := p.leaveType(0)
		capped := p.leaveType(2)
		p.update("Leave Request", p.request(e, uncapped, "2030-03-01", "2030-03-10"), `{"status":"Approved"}`)
		lr := p.request(e, capped, "2030-04-01", "2030-04-02")
		p.update("Leave Request", lr, `{"status":"Approved"}`)
		p.expect("Leave Request", lr, "status", "Approved")
	})
	t.Run("approving a request covering today puts the employee on leave", func(t *testing.T) {
		e := p.employee()
		lr := p.request(e, p.leaveType(0), yesterday(), tomorrow())
		p.update("Leave Request", lr, `{"status":"Approved"}`)
		p.expect("Employee", e, "status", "On leave")
	})
	t.Run("cancelling the request covering today brings the employee back", func(t *testing.T) {
		e := p.employee()
		lr := p.request(e, p.leaveType(0), today(), today())
		p.update("Leave Request", lr, `{"status":"Approved"}`)
		p.expect("Employee", e, "status", "On leave")
		p.update("Leave Request", lr, `{"status":"Cancelled"}`)
		p.expect("Employee", e, "status", "Active")
	})
	t.Run("a future approved request does not change the employee's status", func(t *testing.T) {
		e := p.employee()
		lr := p.request(e, p.leaveType(0), "2031-06-01", "2031-06-03")
		p.update("Leave Request", lr, `{"status":"Approved"}`)
		p.expect("Employee", e, "status", "Active")
	})
	t.Run("decided requests cannot move to another status", func(t *testing.T) {
		e := p.employee()
		lt := p.leaveType(0)
		rejected := p.request(e, lt, "2030-05-01", "2030-05-02")
		p.update("Leave Request", rejected, `{"status":"Rejected"}`)
		p.refused(httpMethodPut, "Leave Request", rejected, `{"status":"Approved"}`, "cannot become approved")
		approved := p.request(e, lt, "2030-06-01", "2030-06-02")
		p.update("Leave Request", approved, `{"status":"Approved"}`)
		p.refused(httpMethodPut, "Leave Request", approved, `{"status":"Pending"}`, "cannot become pending")
		p.refused(httpMethodPut, "Leave Request", approved, `{"status":"Rejected"}`, "cannot become rejected")
	})
	t.Run("attendance on approved leave cannot be Present", func(t *testing.T) {
		e := p.employee()
		lr := p.request(e, p.leaveType(0), "2030-07-04", "2030-07-05")
		p.update("Leave Request", lr, `{"status":"Approved"}`)
		p.refused(httpMethodPost, "Attendance", "", p.attendance(e, "2030-07-04", "Present"), "approved leave")
		p.create("Attendance", p.attendance(e, "2030-07-04", "On leave"))
		p.create("Attendance", p.attendance(e, "2030-07-04", "Late"))
		p.create("Attendance", p.attendance(e, "2030-07-06", "Present"))
	})
	t.Run("an HR User's approval moves the employee on leave, which the HR User cannot edit", func(t *testing.T) {
		e := p.employee()
		fixture := p.as("HR User")
		fixture.t = t
		hr := &peopleScripts{packFixture: fixture, n: p.n}
		lr := hr.request(e, p.leaveType(0), today(), today())
		hr.update("Leave Request", lr, `{"status":"Approved"}`)
		p.expect("Employee", e, "status", "On leave")
		hr.refused(httpMethodPut, "Employee", e, `{"status":"Active"}`, "not permitted")
		hr.refused(httpMethodPost, "Attendance", "", p.attendance(e, today(), "Present"), "approved leave")
	})
}

// employee creates an active employee with a unique number.
func (p *peopleScripts) employee() string {
	p.n++
	return p.create("Employee", fmt.Sprintf(`{"employee_no":"E-%d","first_name":"Wanjiru","last_name":"Kamau"}`, p.n))
}

// leaveType creates a leave type, capped at days a year when days is above zero.
func (p *peopleScripts) leaveType(days int) string {
	p.n++
	body := map[string]any{"leave_type_name": fmt.Sprintf("Type %d", p.n)}
	if days > 0 {
		body["days_per_year"] = days
	}
	return p.create("Leave Type", jsonBody(p.t, body))
}

// request creates a Pending leave request.
func (p *peopleScripts) request(employee, leaveType, start, end string) string {
	return p.create("Leave Request", leaveBody(p.t, employee, leaveType, start, end))
}

// refusedRequest asserts a new Pending leave request is refused with want in the reply.
func (p *peopleScripts) refusedRequest(employee, leaveType, start, end, want string) {
	p.t.Helper()
	p.refused(httpMethodPost, "Leave Request", "", leaveBody(p.t, employee, leaveType, start, end), want)
}

func (p *peopleScripts) attendance(employee, day, status string) string {
	return jsonBody(p.t, map[string]any{"employee": employee, "attendance_date": day, "status": status})
}

func leaveBody(t interface{ Fatalf(string, ...any) }, employee, leaveType, start, end string) string {
	return jsonBody(t, map[string]any{"employee": employee, "leave_type": leaveType, "start_date": start, "end_date": end})
}

// jsonBody encodes a request body, failing the test on an encoding error.
func jsonBody(t interface{ Fatalf(string, ...any) }, body map[string]any) string {
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return string(raw)
}

// today, yesterday and tomorrow are UTC dates, the clock the script reads.
func today() string     { return time.Now().UTC().Format("2006-01-02") }
func yesterday() string { return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02") }
func tomorrow() string  { return time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02") }

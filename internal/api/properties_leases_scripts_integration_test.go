//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The Properties pack's lease rules, run as shipped on MySQL and PostgreSQL. The Lease before_save
// script occupies and frees units, refuses overlapping or maintenance lets, and freezes ended leases.
const leasesPack = "../../config/properties"

// leaseFields builds a JSON request body for a record from its fields.
func leaseFields(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return string(raw)
}

func TestPackScriptsLeases(t *testing.T) {
	eachPackDialect(t, []string{leasesPack}, func(f *packFixture) {
		manager := f.as("Estate Manager")
		agent := f.as("Leasing Agent")
		property := manager.create("Property", `{"property_name":"Acacia Court"}`)
		tenant := manager.create("Tenant", `{"tenant_name":"Achieng Otieno"}`)
		other := manager.create("Tenant", `{"tenant_name":"Mwangi Kariuki"}`)
		unitNo := func(no string) string {
			return manager.create("Unit", leaseFields(t, map[string]any{"unit_no": no, "property": property, "monthly_rent": 25000}))
		}
		leaseBody := func(ref, unit, tenantName, start, end, status string) string {
			fields := map[string]any{"reference": ref, "unit": unit, "tenant": tenantName, "start_date": start}
			if end != "" {
				fields["end_date"] = end
			}
			if status != "" {
				fields["status"] = status
			}
			return leaseFields(t, fields)
		}

		// lease saves a lease as the given fixture and fails the test when it is refused.
		lease := func(who *packFixture, ref, unit, tenantName, start, end, status string) string {
			return who.create("Lease", leaseBody(ref, unit, tenantName, start, end, status))
		}
		f.t.Run("an open-ended lease blocks a later overlapping lease on its unit", func(t *testing.T) {
			u := unitNo("4B")
			lease(manager, "L-OPEN", u, tenant, "2020-01-01", "", "Active")
			manager.refused(httpMethodPost, "Lease", "", leaseBody("L-OVER", u, other, "2021-01-01", "2021-02-01", "Active"),
				"Unit 4B is let to Achieng Otieno from 2020-01-01 (L-OPEN).")
			manager.expect("Unit", u, "status", "Occupied")
		})

		f.t.Run("dated leases that overlap are refused", func(t *testing.T) {
			u := unitNo("4C")
			lease(manager, "L-A", u, tenant, "2020-03-01", "2020-06-30", "Active")
			manager.refused(httpMethodPost, "Lease", "", leaseBody("L-B", u, other, "2020-06-30", "2020-12-31", "Active"),
				"Unit 4C is let to Achieng Otieno from 2020-03-01 (L-A).")
		})

		f.t.Run("leases that do not overlap are allowed before and after another", func(t *testing.T) {
			u := unitNo("4D")
			lease(manager, "L-EARLY", u, tenant, "2019-01-01", "2019-12-31", "Active")
			lease(manager, "L-LATE", u, other, "2020-01-01", "", "Active")
			lease(manager, "L-FIRST", u, other, "2018-01-01", "2018-12-31", "Active")
		})

		f.t.Run("a new active lease on a unit under maintenance is refused, an ended one is recorded", func(t *testing.T) {
			u := unitNo("4E")
			manager.update("Unit", u, `{"status":"Maintenance"}`)
			manager.refused(httpMethodPost, "Lease", "", leaseBody("L-MAINT", u, tenant, "2020-01-01", "", "Active"),
				"Unit 4E is under maintenance and cannot be let.")
			lease(manager, "L-HIST", u, tenant, "2019-01-01", "2019-06-30", "Ended")
			manager.expect("Unit", u, "status", "Maintenance")
		})

		f.t.Run("an active lease occupies its unit and ending it vacates the unit", func(t *testing.T) {
			u := unitNo("4F")
			manager.expect("Unit", u, "status", "Vacant")
			l := lease(manager, "L-F", u, tenant, "2020-01-01", "", "Active")
			manager.expect("Unit", u, "status", "Occupied")
			manager.update("Lease", l, `{"status":"Ended","end_date":"2020-06-30"}`)
			manager.expect("Unit", u, "status", "Vacant")
		})

		f.t.Run("two leases in sequence keep the unit occupied until the last one ends", func(t *testing.T) {
			u := unitNo("4G")
			first := lease(manager, "L-1", u, tenant, "2020-01-01", "2020-06-30", "Active")
			second := lease(manager, "L-2", u, other, "2020-07-01", "", "Active")
			manager.expect("Unit", u, "status", "Occupied")
			manager.update("Lease", first, `{"status":"Ended"}`)
			manager.expect("Unit", u, "status", "Occupied")
			manager.update("Lease", second, `{"status":"Ended"}`)
			manager.expect("Unit", u, "status", "Vacant")
		})

		f.t.Run("ending a lease without an end date fills in today", func(t *testing.T) {
			u := unitNo("4H")
			l := lease(manager, "L-H", u, tenant, "2020-01-01", "", "Active")
			before := time.Now().Format("2006-01-02")
			manager.update("Lease", l, `{"status":"Ended"}`)
			after := time.Now().Format("2006-01-02")
			// A DATE column reads back as a timestamp on MySQL, so compare the date part.
			got := f.value("Lease", l, "end_date")
			if !strings.HasPrefix(got, before) && !strings.HasPrefix(got, after) {
				t.Fatalf("end date after ending = %q, want today %s", got, before)
			}
		})

		f.t.Run("an ended lease cannot be reopened", func(t *testing.T) {
			u := unitNo("4I")
			l := lease(manager, "L-I", u, tenant, "2019-01-01", "2019-06-30", "Ended")
			manager.refused(httpMethodPut, "Lease", l, `{"status":"Active"}`, "Make a new lease.")
			manager.expect("Lease", l, "status", "Ended")
		})

		f.t.Run("moving an active lease to another unit frees the old one", func(t *testing.T) {
			from, to := unitNo("4J"), unitNo("4K")
			l := lease(manager, "L-J", from, tenant, "2020-01-01", "", "Active")
			manager.update("Lease", l, `{"unit":"`+to+`"}`)
			manager.expect("Unit", from, "status", "Vacant")
			manager.expect("Unit", to, "status", "Occupied")
		})

		f.t.Run("a unit that does not exist is refused", func(t *testing.T) {
			manager.refused(httpMethodPost, "Lease", "", leaseBody("L-NOUNIT", "NO-SUCH-UNIT", tenant, "2020-01-01", "", "Active"),
				"Cannot check unit NO-SUCH-UNIT.")
		})

		f.t.Run("the leasing agent's lease updates the unit it may only read", func(t *testing.T) {
			u := unitNo("4L")
			l := lease(agent, "L-AG", u, tenant, "2020-01-01", "", "Active")
			manager.expect("Unit", u, "status", "Occupied")
			agent.refused(httpMethodPost, "Lease", "", leaseBody("L-AG-OVER", u, other, "2021-01-01", "", "Active"),
				"Unit 4L is let to Achieng Otieno from 2020-01-01 (L-AG).")
			agent.update("Lease", l, `{"status":"Ended","end_date":"2020-12-31"}`)
			manager.expect("Unit", u, "status", "Vacant")
		})

		f.t.Run("the leasing agent cannot edit the unit directly", func(t *testing.T) {
			u := unitNo("4M")
			code, _, doc := agent.send(httpMethodPut, "Unit", u, `{"status":"Maintenance"}`)
			if doc != nil || code != http.StatusForbidden {
				t.Fatalf("leasing agent unit edit = %d (saved %v), want 403 refused", code, doc != nil)
			}
			manager.expect("Unit", u, "status", "Vacant")
		})
	})
}

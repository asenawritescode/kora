//go:build integration

package api

import (
	"encoding/json"
	"testing"
)

// The Appointments pack with its Appointment before_save script, run the way the Engine
// runs it on MySQL and PostgreSQL. A stylist's bookings may not overlap, an appointment
// must use an offered service, and a finished appointment cannot be reopened.
const appointmentsPack = "../../config/appointments"

func TestPackScriptsAppointments(t *testing.T) {
	eachPackDialect(t, []string{appointmentsPack}, func(f *packFixture) {
		f.t.Run("overlap is refused for the same stylist", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			book("APT-OV1", "amina@salon.test", "2026-12-01 10:00:00", "Achieng", "Booked")
			for _, when := range []string{"10:00:00", "10:15:00", "09:45:00"} {
				f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
					"reference": "APT-OVX" + when, "client_name": "Wanjiru", "service": cut,
					"staff": "amina@salon.test", "scheduled_for": "2026-12-01 " + when,
				}), "amina@salon.test has Achieng at 10:00 (APT-OV1).")
			}
		})
		f.t.Run("an appointment that ends inside or spans another is refused", func(t *testing.T) {
			colour := f.create("Service", `{"service_name":"Colour","duration_minutes":90,"price":2000}`)
			cut := f.create("Service", `{"service_name":"Cut 2","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			book("APT-OV2", "beth@salon.test", "2026-12-01 13:00:00", "Njeri", "Booked")
			f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
				"reference": "APT-OV2X", "client_name": "Mwende", "service": cut,
				"staff": "beth@salon.test", "scheduled_for": "2026-12-01 12:45:00",
			}), "beth@salon.test has Njeri at 13:00 (APT-OV2).")
			f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
				"reference": "APT-OV2Y", "client_name": "Mwende", "service": colour,
				"staff": "beth@salon.test", "scheduled_for": "2026-12-01 12:00:00",
			}), "beth@salon.test has Njeri at 13:00 (APT-OV2).")
		})
		f.t.Run("back-to-back and other stylists are allowed", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 3","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			book("APT-B1", "cara@salon.test", "2026-12-01 15:00:00", "Akinyi", "Booked")
			book("APT-B2", "cara@salon.test", "2026-12-01 15:30:00", "Atieno", "Booked")
			book("APT-B3", "cara@salon.test", "2026-12-01 14:30:00", "Ruth", "Booked")
			book("APT-B4", "dan@salon.test", "2026-12-01 15:00:00", "Kevin", "Booked")
			book("APT-B5", "", "2026-12-01 15:00:00", "Walk-in", "Booked")
			book("APT-B6", "", "2026-12-01 15:00:00", "Walk-in two", "Booked")
		})
		f.t.Run("a cancelled booking does not block its slot", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 4","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			gone := book("APT-C1", "eve@salon.test", "2026-12-01 09:00:00", "Lilian", "Booked")
			f.update("Appointment", gone, `{"status":"Cancelled"}`)
			book("APT-C2", "eve@salon.test", "2026-12-01 09:00:00", "Joy", "Booked")
		})
		f.t.Run("moving a booking onto another is refused, editing its own slot is not", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 5","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			first := book("APT-M1", "fay@salon.test", "2026-12-01 10:00:00", "Pendo", "Booked")
			second := book("APT-M2", "fay@salon.test", "2026-12-01 11:00:00", "Rose", "Booked")
			f.update("Appointment", first, `{"notes":"Bring a photo"}`)
			f.refused(httpMethodPut, "Appointment", second, `{"scheduled_for":"2026-12-01 10:10:00"}`,
				"fay@salon.test has Pendo at 10:00 (APT-M1).")
		})
		f.t.Run("time forms compare on the same clock", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 6","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			book("APT-T1", "gift@salon.test", "2026-12-01 16:00:00", "Halima", "Booked")
			for _, when := range []string{"2026-12-01T16:10:00Z", "2026-12-01T16:10", "2026-12-01T16:10:00+00:00"} {
				f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
					"reference": "APT-T1X" + when, "client_name": "Zawadi", "service": cut,
					"staff": "gift@salon.test", "scheduled_for": when,
				}), "gift@salon.test has Halima at 16:00 (APT-T1).")
			}
		})
		f.t.Run("a service with no duration counts as 30 minutes", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 7","duration_minutes":30,"price":500}`)
			// The column is NOT NULL, so "no duration" is stored as 0.
			f.exec("UPDATE "+f.quote("tabService")+" SET duration_minutes = 0 WHERE name = ?", cut)
			book := appointmentBooker(t, f, cut)
			book("APT-D1", "hana@salon.test", "2026-12-01 17:00:00", "Ada", "Booked")
			f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
				"reference": "APT-D2", "client_name": "Ida", "service": cut,
				"staff": "hana@salon.test", "scheduled_for": "2026-12-01 17:15:00",
			}), "hana@salon.test has Ada at 17:00 (APT-D1).")
			book("APT-D3", "hana@salon.test", "2026-12-01 17:30:00", "Ivy", "Booked")
		})
		f.t.Run("a retired service is refused for new and moved bookings", func(t *testing.T) {
			retired := f.create("Service", `{"service_name":"Retired Wax","duration_minutes":30,"price":300,"active":0}`)
			cut := f.create("Service", `{"service_name":"Cut 8","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
				"reference": "APT-R1", "client_name": "Kemi", "service": retired,
				"scheduled_for": "2026-12-02 10:00:00",
			}), "Retired Wax is not offered now.")
			kept := book("APT-R2", "", "2026-12-02 11:00:00", "Mary", "Booked")
			f.refused(httpMethodPut, "Appointment", kept, `{"service":"`+retired+`"}`, "Retired Wax is not offered now.")
			f.update("Service", cut, `{"active":0}`)
			f.update("Appointment", kept, `{"notes":"Still booked"}`)
			f.update("Service", cut, `{"active":1}`)
		})
		f.t.Run("a new appointment starts Booked and final statuses do not reopen", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 9","duration_minutes":30,"price":500}`)
			book := appointmentBooker(t, f, cut)
			f.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
				"reference": "APT-F0", "client_name": "Nia", "service": cut, "status": "Done",
				"scheduled_for": "2026-12-03 10:00:00",
			}), "A new appointment starts as Booked.")
			for _, final := range []string{"Done", "No show", "Cancelled"} {
				done := book("APT-F-"+final, "", "2026-12-03 10:00:00", "Esther", "Booked")
				f.update("Appointment", done, `{"status":"`+final+`"}`)
				f.refused(httpMethodPut, "Appointment", done, `{"status":"Booked"}`, "APT-F-"+final+" is "+final+"; it cannot change to Booked.")
			}
			done := book("APT-F-D2", "", "2026-12-03 12:00:00", "Grace", "Booked")
			f.update("Appointment", done, `{"status":"Done"}`)
			f.refused(httpMethodPut, "Appointment", done, `{"status":"Cancelled"}`, "APT-F-D2 is Done; it cannot change to Cancelled.")
			f.update("Appointment", done, `{"notes":"Paid in cash"}`)
		})
		f.t.Run("the front-line stylist books, is refused, and cannot change a service", func(t *testing.T) {
			cut := f.create("Service", `{"service_name":"Cut 10","duration_minutes":30,"price":500}`)
			stylist := f.as("Stylist")
			book := appointmentBooker(t, stylist, cut)
			own := book("APT-S1", "ivy@salon.test", "2026-12-04 11:00:00", "Sophie", "Booked")
			stylist.refused(httpMethodPost, "Appointment", "", appointmentBody(t, map[string]any{
				"reference": "APT-S2", "client_name": "Lynn", "service": cut,
				"staff": "ivy@salon.test", "scheduled_for": "2026-12-04 11:15:00",
			}), "ivy@salon.test has Sophie at 11:00 (APT-S1).")
			stylist.update("Appointment", own, `{"scheduled_for":"2026-12-04 12:00:00"}`)
			stylist.update("Appointment", own, `{"status":"Done"}`)
			stylist.refused(httpMethodPut, "Appointment", own, `{"status":"Booked"}`, "APT-S1 is Done; it cannot change to Booked.")
			if _, _, doc := stylist.send(httpMethodPut, "Service", cut, `{"price":1}`); doc != nil {
				t.Fatal("a stylist changed a service price")
			}
			if got := f.amount("Service", cut, "price"); got != 500 {
				t.Fatalf("service price after the stylist's refused change = %v, want 500", got)
			}
		})
	})
}

// appointmentBooker returns a function that books an appointment through the fixture and
// returns its record name. An empty staff leaves the appointment unassigned.
func appointmentBooker(t *testing.T, f *packFixture, service string) func(ref, staff, when, client, status string) string {
	t.Helper()
	return func(ref, staff, when, client, status string) string {
		t.Helper()
		fields := map[string]any{
			"reference": ref, "client_name": client, "service": service,
			"scheduled_for": when, "status": status,
		}
		if staff != "" {
			fields["staff"] = staff
		}
		return f.create("Appointment", appointmentBody(t, fields))
	}
}

func appointmentBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode appointment: %v", err)
	}
	return string(raw)
}

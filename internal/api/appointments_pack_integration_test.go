//go:build integration

package api

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// The Appointments pack as shipped, on real MySQL: services keep their minimums,
// an appointment starts Booked, references are unique, and a stylist can book
// and update appointments but not change services or delete anything.
func TestAppointmentsBookingWorkflowOnMySQL(t *testing.T) {
	pack := filepath.Join("..", "..", "config", "appointments")
	roles, permissions, err := doctype.ParseRolesDirectory(pack)
	if err != nil {
		t.Fatalf("parse appointments roles: %v", err)
	}
	handler, db, reg, site := newPackLayersHandlerWithRoles(t, roles, permissions, "appointments")
	c := newPackClient(t, handler, db, reg, site)

	service := c.create("Service", `{"service_name":"Braids","duration_minutes":120,"price":3500.5}`)
	if got := c.value("Service", service, "active"); got != "1" {
		t.Fatalf("a new service is offered = %q, want 1", got)
	}
	if !c.refused(httpMethodPost, "Service", "", `{"service_name":"Braids","duration_minutes":30,"price":100}`) {
		t.Fatal("a second service with the same name was accepted")
	}
	if !c.refused(httpMethodPost, "Service", "", `{"service_name":"Quick","duration_minutes":4,"price":100}`) {
		t.Fatal("a service shorter than 5 minutes was accepted")
	}
	if c.refused(httpMethodPost, "Service", "", `{"service_name":"Five","duration_minutes":5,"price":0}`) {
		t.Fatal("a 5 minute free service was refused")
	}
	if !c.refused(httpMethodPost, "Service", "", `{"service_name":"Negative","duration_minutes":30,"price":-1}`) {
		t.Fatal("a negative price was accepted")
	}

	appointment := c.create("Appointment", `{"reference":"AP-1","client_name":"Achieng","client_phone":"0700000000","service":"`+service+`","scheduled_for":"2026-12-03 10:00:00"}`)
	if got := c.value("Appointment", appointment, "status"); got != "Booked" {
		t.Fatalf("new appointment status = %q, want Booked", got)
	}
	if got := c.value("Appointment", appointment, "scheduled_for"); !strings.HasPrefix(got, "2026-12-03T10:00") {
		t.Fatalf("scheduled_for = %q, want 2026-12-03T10:00", got)
	}
	if !c.refused(httpMethodPost, "Appointment", "", `{"reference":"AP-1","client_name":"Other","service":"`+service+`","scheduled_for":"2026-12-04 10:00:00"}`) {
		t.Fatal("a second appointment with the same reference was accepted")
	}
	if !c.refused(httpMethodPost, "Appointment", "", `{"reference":"AP-2","client_name":"No time","service":"`+service+`"}`) {
		t.Fatal("an appointment without a time was accepted")
	}
	if !c.refused(httpMethodPut, "Appointment", appointment, `{"status":"Rescheduled"}`) {
		t.Fatal("an unknown status was accepted")
	}

	stylist := []string{"Stylist"}
	path := "/api/resource/" + url.PathEscape("Appointment")
	servicePath := "/api/resource/" + url.PathEscape("Service")
	if code := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost, path, `{"reference":"AP-3","client_name":"Wambui","service":"`+service+`","scheduled_for":"2026-12-05 09:00:00"}`, "", stylist).Code; code != 201 && code != 200 {
		t.Fatalf("a stylist could not book an appointment: %d", code)
	}
	if code := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, path+"/"+appointment, `{"status":"Done"}`, "", stylist).Code; code != 200 {
		t.Fatalf("a stylist could not mark an appointment done: %d", code)
	}
	if got := c.value("Appointment", appointment, "status"); got != "Done" {
		t.Fatalf("status after the stylist's update = %q, want Done", got)
	}
	if code := serveResourceMutationBody(t, handler, db, reg, site, httpMethodDelete, path+"/"+appointment, "", "", stylist).Code; code < 400 {
		t.Fatalf("a stylist deleted an appointment: %d", code)
	}
	if code := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, servicePath+"/"+service, `{"price":1}`, "", stylist).Code; code < 400 {
		t.Fatalf("a stylist changed a service price: %d", code)
	}
	if code := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost, servicePath, `{"service_name":"Sneaky","duration_minutes":30,"price":1}`, "", stylist).Code; code < 400 {
		t.Fatalf("a stylist created a service: %d", code)
	}
	if got := c.value("Service", service, "price"); !strings.HasPrefix(got, "3500.5") {
		t.Fatalf("service price after refused change = %q, want 3500.5", got)
	}
	if code := serveResourceMutationBody(t, handler, db, reg, site, httpMethodDelete, path+"/"+appointment, "", "", []string{"Salon Manager"}).Code; code >= 400 {
		t.Fatalf("a salon manager could not delete an appointment: %d", code)
	}
}

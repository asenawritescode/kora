//go:build integration

package api

import (
	"net/http"
	"testing"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
)

func TestPOSShiftAndTillSessionCreateActions(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	reg.Register(&doctype.DocType{Name: "Shift", Fields: []doctype.Field{
		{Fieldname: "reference", Fieldtype: "Data", Reqd: true, Unique: true},
		{Fieldname: "status", Fieldtype: "Select", Options: "Open\nClosed", Reqd: true, Default: "Open"},
		{Fieldname: "started_at", Fieldtype: "Datetime", Reqd: true},
		{Fieldname: "cashier", Fieldtype: "User"},
	}})
	reg.Register(&doctype.DocType{Name: "Till Session", Fields: []doctype.Field{
		{Fieldname: "reference", Fieldtype: "Data", Reqd: true, Unique: true},
		{Fieldname: "register", Fieldtype: "Data", Reqd: true},
		{Fieldname: "shift", Fieldtype: "Link", Options: "Shift", Reqd: true},
		{Fieldname: "status", Fieldtype: "Select", Options: "Open\nClosed", Reqd: true, Default: "Open"},
		{Fieldname: "opened_at", Fieldtype: "Datetime", Reqd: true},
		{Fieldname: "opening_cash", Fieldtype: "Currency", Reqd: true, Default: "0"},
		{Fieldname: "expected_cash", Fieldtype: "Currency", Default: "0"},
	}})
	for _, name := range []string{"Shift", "Till Session"} {
		reg.Permissions.SetPermission(&doctype.Permission{Doctype: name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true})
	}
	if err := schema.MigrateSiteFromRegistry(db, site, reg, kdb.Resolve("mysql")); err != nil {
		t.Fatalf("migrate prerequisite test schema: %v", err)
	}
	reg.Views.Register(&doctype.View{
		Name: "Point of Sale", Route: "/pos", Type: "collection", SourceDocType: "Product",
		Components: []doctype.ViewComponent{{ID: "payment", Bindings: map[string]string{"register": "literal:Main Counter"}, Actions: []doctype.ViewAction{
			{ID: "open_shift", Trigger: "on_click", Type: "create_record", Config: map[string]any{
				"target_doctype": "Shift", "context_field": "shift", "reference_prefix": "SHIFT",
				"fields": map[string]any{"reference": "$generated", "status": "Open", "started_at": "$now", "cashier": "$actor"},
			}},
			{ID: "open_till_session", Trigger: "on_click", Type: "create_record", Config: map[string]any{
				"target_doctype": "Till Session", "context_field": "till_session", "reference_prefix": "TILL",
				"depends_on": "shift", "inputs": []any{map[string]any{"field": "opening_cash", "label": "Opening Cash", "fieldtype": "Currency", "default": 0, "required": true}},
				"fields": map[string]any{"reference": "$generated", "register": "$binding.register", "shift": "$context.shift", "opened_at": "$now", "opening_cash": "$input.opening_cash", "expected_cash": "$input.opening_cash", "status": "Open"},
			}},
		}}},
	})

	shiftResponse := serveManifestAction(t, handler, site, db, reg, "open_shift", map[string]any{
		"_prerequisites": map[string]any{}, "_inputs": map[string]any{},
	})
	if shiftResponse.Code != http.StatusOK {
		t.Fatalf("open_shift status = %d, response = %s", shiftResponse.Code, shiftResponse.Body.String())
	}
	shift := requireActionDocument(t, shiftResponse.Body.Bytes())
	shiftName, _ := shift["name"].(string)
	if shiftName == "" || shift["status"] != "Open" || shift["cashier"] != "admin@example.test" {
		t.Fatalf("created shift = %#v", shift)
	}

	tillResponse := serveManifestAction(t, handler, site, db, reg, "open_till_session", map[string]any{
		"_prerequisites": map[string]any{"shift": shiftName},
		"_inputs":        map[string]any{"opening_cash": float64(0)},
	})
	if tillResponse.Code != http.StatusOK {
		t.Fatalf("open_till_session status = %d, response = %s", tillResponse.Code, tillResponse.Body.String())
	}
	till := requireActionDocument(t, tillResponse.Body.Bytes())
	if till["name"] == "" || till["shift"] != shiftName || till["register"] != "Main Counter" || till["status"] != "Open" {
		t.Fatalf("created till session = %#v", till)
	}
}

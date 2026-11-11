//go:build integration

package api

import (
	"database/sql"
	"net/http"
	"testing"
)

// The CRM pack, loaded as shipped into a throwaway MySQL: a contact gets its
// readable name, and a deal adds its lines up.
func TestCRMContactAndDealWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "crm")
	post := func(doctype, body string) (int, map[string]any) {
		recorder := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost, "/api/resource/"+doctype, body, "", []string{"Administrator"})
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
			return recorder.Code, nil
		}
		return recorder.Code, requireRESTDocument(t, recorder.Body.Bytes(), doctype)
	}

	code, contact := post("Contact", `{"first_name":"Amina","last_name":"Otieno","email":"amina@example.test"}`)
	if contact == nil {
		t.Fatalf("create contact = %d", code)
	}
	contactName, _ := contact["name"].(string)
	var fullName sql.NullString
	if err := db.QueryRow("SELECT full_name FROM `tabContact` WHERE name = ?", contactName).Scan(&fullName); err != nil || fullName.String != "Amina Otieno" {
		t.Fatalf("contact full_name = %q (%v), want %q built without any script", fullName.String, err, "Amina Otieno")
	}

	items := `[{"product_name":"Shelving","quantity":2,"unit_price":1500},{"product_name":"Delivery","quantity":1,"unit_price":500}]`
	code, deal := post("Deal", `{"deal_name":"Shop fit-out","contact":"`+contactName+`","discount":100,"items":`+items+`}`)
	if deal == nil {
		t.Fatalf("create deal = %d", code)
	}
	dealName, _ := deal["name"].(string)
	var subtotal, total float64
	var stage string
	if err := db.QueryRow("SELECT subtotal, total, pipeline_stage FROM `tabDeal` WHERE name = ?", dealName).Scan(&subtotal, &total, &stage); err != nil {
		t.Fatal(err)
	}
	if subtotal != 3500 || total != 3400 || stage != "Lead" {
		t.Fatalf("deal = subtotal %v total %v stage %q, want 3500/3400/Lead", subtotal, total, stage)
	}

	// A deal needs a contact.
	if code, refused := post("Deal", `{"deal_name":"Nobody","items":`+items+`}`); refused != nil {
		t.Fatalf("a deal with no contact was accepted (%d)", code)
	}
}

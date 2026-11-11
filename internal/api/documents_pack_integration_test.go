//go:build integration

package api

import (
	"strings"
	"testing"
)

// The Documents pack as shipped, on real MySQL: categories are unique, a
// version gets a readable label, and a signed-off document is frozen.
func TestDocumentsRegisterAndSignOffWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "documents")
	c := newPackClient(t, handler, db, reg, site)

	category := c.create("Document Category", `{"category_name":"Contracts"}`)
	if !c.refused(httpMethodPost, "Document Category", "", `{"category_name":"Contracts"}`) {
		t.Fatal("a second category with the same name was accepted")
	}
	if !c.refused(httpMethodPost, "Document", "", `{"category":"`+category+`"}`) {
		t.Fatal("a document without a title was accepted")
	}

	doc := c.create("Document", `{"title":"Lease agreement","category":"`+category+`","expiry_date":"2026-12-03","status":"Active","attachment":"lease-v1.pdf"}`)
	if got := c.value("Document", doc, "approval_status"); got != "Not needed" {
		t.Fatalf("approval status default = %q, want Not needed", got)
	}
	if got := c.value("Document", doc, "expiry_date"); !strings.HasPrefix(got, "2026-12-03") {
		t.Fatalf("expiry date = %q, want 2026-12-03", got)
	}

	version := c.create("Document Version", `{"document":"`+doc+`","version_no":2,"change_note":"Rent revised"}`)
	if got := c.value("Document Version", version, "label"); got != doc+" v2" {
		t.Fatalf("version label = %q, want %q", got, doc+" v2")
	}

	if c.refused(httpMethodPut, "Document", doc, `{"approval_status":"Waiting","approver":"boss@example.com"}`) {
		t.Fatal("sending a document for sign-off was refused")
	}
	if c.refused(httpMethodPut, "Document", doc, `{"title":"Lease agreement (draft)"}`) {
		t.Fatal("editing a document that is only waiting was refused")
	}
	if c.refused(httpMethodPut, "Document", doc, `{"approval_status":"Approved","decision_note":"Signed"}`) {
		t.Fatal("approving a waiting document was refused")
	}
	for _, body := range []string{`{"title":"Renamed"}`, `{"attachment":"other.pdf"}`} {
		if !c.refused(httpMethodPut, "Document", doc, body) {
			t.Fatalf("an approved document was changed with %s", body)
		}
	}
	if got := c.value("Document", doc, "title"); got != "Lease agreement (draft)" {
		t.Fatalf("title after refused changes = %q", got)
	}
	// Archiving and the expiry date are not frozen by sign-off.
	if c.refused(httpMethodPut, "Document", doc, `{"status":"Archived"}`) {
		t.Fatal("archiving an approved document was refused")
	}
}

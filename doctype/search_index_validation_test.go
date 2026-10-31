package doctype

import (
	"strings"
	"testing"
)

func TestDocTypeValidationRejectsGenericSearchIndexOnUserPicker(t *testing.T) {
	definition := &DocType{
		Name:   "Handoff",
		Module: "CRM",
		Fields: []Field{{Fieldname: "receiving_owner", Fieldtype: "User", SearchIndex: true}},
	}
	if err := definition.Validate(); err == nil || !strings.Contains(err.Error(), "User fields are authenticated user pickers") {
		t.Fatalf("Validate() error = %v, want User picker search-index rejection", err)
	}

	definition.Fields[0].SearchIndex = false
	if err := definition.Validate(); err != nil {
		t.Fatalf("Validate() rejected a normal User picker: %v", err)
	}
}

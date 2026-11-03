package doctype

import (
	"testing"
	"time"
)

func TestApplyDefaultValuesOnlyFillsOmittedFields(t *testing.T) {
	dt := &DocType{Fields: []Field{
		{Fieldname: "status", Fieldtype: "Data", Default: "draft"},
		{Fieldname: "created_at", Fieldtype: "Datetime", Default: "now"},
		{Fieldname: "started_at", Fieldtype: "Time", Default: "now"},
		{Fieldname: "business_date", Fieldtype: "Date", Default: "today"},
	}}
	doc := NewDocument(dt.Name)
	doc.Set("status", nil)
	before := time.Now().UTC()
	ApplyDefaultValues(dt, doc)
	after := time.Now().UTC()

	if value, exists := doc.Fields["status"]; !exists || value != nil {
		t.Fatalf("explicit nil status = %#v (present=%t), want explicit nil preserved", value, exists)
	}
	created, ok := doc.Get("created_at").(time.Time)
	if !ok || created.Before(before) || created.After(after) || created.Location() != time.UTC {
		t.Fatalf("created_at = %#v, want UTC time between %v and %v", doc.Get("created_at"), before, after)
	}
	if got := doc.GetString("started_at"); got == "" {
		t.Fatal("started_at default was not applied")
	}
	if got, want := doc.GetString("business_date"), before.Format("2006-01-02"); got != want {
		t.Fatalf("business_date = %q, want %q", got, want)
	}
}

func TestApplyDefaultValuesNilInputsAreNoops(t *testing.T) {
	ApplyDefaultValues(nil, NewDocument("Example"))
	ApplyDefaultValues(&DocType{}, nil)
}

package kernel

import (
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestRecordFieldChangesIncludesVisibleDiffsOnly(t *testing.T) {
	dt := &doctype.DocType{Name: "Contact", Fields: []doctype.Field{
		{Fieldname: "full_name", Fieldtype: "Data", Label: "Full name"},
		{Fieldname: "private_note", Fieldtype: "Data", Hidden: true},
		{Fieldname: "password", Fieldtype: "Password"},
		{Fieldname: "unchanged", Fieldtype: "Data"},
	}}
	before, after := doctype.NewDocument("Contact"), doctype.NewDocument("Contact")
	before.Set("full_name", "Amina Wanjiku")
	after.Set("full_name", "Amina Njoroge")
	before.Set("private_note", "private old")
	after.Set("private_note", "private new")
	before.Set("password", "secret old")
	after.Set("password", "secret new")
	before.Set("unchanged", "same")
	after.Set("unchanged", "same")

	got := recordFieldChanges(dt, before, after)
	want := []RecordFieldChange{{Field: "full_name", Before: "Amina Wanjiku", After: "Amina Njoroge"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recordFieldChanges() = %#v, want %#v", got, want)
	}
}

func TestRecordFieldChangesRecordsInitialValuesOnCreate(t *testing.T) {
	dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}
	after := doctype.NewDocument("Task")
	after.Set("title", "Buy groceries")
	got := recordFieldChanges(dt, nil, after)
	if len(got) != 1 || got[0].Field != "title" || got[0].Before != nil || got[0].After != "Buy groceries" {
		t.Fatalf("create changes = %#v", got)
	}
}

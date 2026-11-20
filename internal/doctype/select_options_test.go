package doctype

import (
	"reflect"
	"testing"
)

func TestSelectChoices(t *testing.T) {
	tests := []struct {
		name    string
		options string
		want    []string
	}{
		{name: "one per line", options: "Starter\nMain\nSide\nDrink\nDessert", want: []string{"Starter", "Main", "Side", "Drink", "Dessert"}},
		{name: "trailing newline", options: "Starter\nMain\n", want: []string{"Starter", "Main"}},
		{name: "blank lines and spaces dropped", options: "  Starter  \n\n   \nMain\n", want: []string{"Starter", "Main"}},
		{name: "CRLF line endings", options: "Starter\r\nMain\r\n", want: []string{"Starter", "Main"}},
		{name: "empty options", options: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &Field{Fieldtype: "Select", Options: tc.options}
			if got := f.selectChoices(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selectChoices(%q) = %q, want %q", tc.options, got, tc.want)
			}
		})
	}
}

func TestValidateDocumentSelectValues(t *testing.T) {
	dt := &DocType{
		Name: "Kitchen Order",
		Fields: []Field{
			{Fieldname: "course", Fieldtype: "Select", Label: "Course", Options: "Starter\nMain\nSide\nDrink\nDessert\n"},
			{Fieldname: "course_required", Fieldtype: "Select", Label: "Required Course", Options: "Starter\nMain", Reqd: true},
		},
	}
	tests := []struct {
		name    string
		course  any
		req     any
		wantErr bool
	}{
		{name: "listed option", course: "Main", req: "Main"},
		{name: "last option before trailing newline", course: "Dessert", req: "Starter"},
		{name: "not an option", course: "Mains", req: "Main", wantErr: true},
		{name: "case differs", course: "main", req: "Main", wantErr: true},
		{name: "leading space is not trimmed from the value", course: " Main", req: "Main", wantErr: true},
		{name: "empty string is not set", course: "", req: "Main"},
		{name: "nil is not set", course: nil, req: "Main"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := NewDocument("Kitchen Order")
			doc.Set("course", tc.course)
			doc.Set("course_required", tc.req)
			errs := ValidateDocument(dt, doc, nil, nil)
			hasSelectErr := false
			for _, e := range errs {
				if e.Field == "course" {
					hasSelectErr = true
				}
			}
			if hasSelectErr != tc.wantErr {
				t.Fatalf("course=%v: select error = %v, want %v (errors: %v)", tc.course, hasSelectErr, tc.wantErr, errs)
			}
		})
	}
}

func TestValidateDocumentSelectRequiredEmpty(t *testing.T) {
	dt := &DocType{
		Name:   "Kitchen Order",
		Fields: []Field{{Fieldname: "course", Fieldtype: "Select", Label: "Course", Options: "Main", Reqd: true}},
	}
	errs := ValidateDocument(dt, NewDocument("Kitchen Order"), nil, nil)
	if len(errs) != 1 || errs[0].Field != "course" {
		t.Fatalf("empty required select: want one required error on course, got %v", errs)
	}
}

package doctype

import "testing"

// TestImmutableAfterUsesStoredStatus checks that immutable_after freezes fields
// once the stored record has reached a listed status, not on the write that
// moves it there.
func TestImmutableAfterUsesStoredStatus(t *testing.T) {
	dt := &DocType{
		Name: "Count",
		Fields: []Field{
			{Fieldname: "status", Fieldtype: "Select", Label: "Status", Options: "Draft\nSubmitted"},
			{Fieldname: "note", Fieldtype: "Data", Label: "Note"},
		},
		DocConstraints: []DocConstraint{{
			Type: "immutable_after", StatusField: "status", StatusValues: []string{"Submitted"},
			ImmutableFields: []string{"status", "note"}, Message: "locked",
		}},
	}
	doc := func(status, note string) *Document {
		d := NewDocument("Count")
		d.Set("status", status)
		d.Set("note", note)
		return d
	}
	tests := []struct {
		name     string
		old, new *Document
		wantErr  bool
	}{
		{"draft edits freely", doc("Draft", "a"), doc("Draft", "b"), false},
		{"submitting a draft is allowed", doc("Draft", "a"), doc("Submitted", "b"), false},
		{"reopening a submitted record is refused", doc("Submitted", "a"), doc("Draft", "a"), true},
		{"editing a submitted record is refused", doc("Submitted", "a"), doc("Submitted", "b"), true},
		{"unchanged submitted record passes", doc("Submitted", "a"), doc("Submitted", "a"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateDocConstraint(&dt.DocConstraints[0], dt, tt.new, tt.old, nil, nil)
			if got := len(errs) > 0; got != tt.wantErr {
				t.Fatalf("errors = %v, want error %v", errs, tt.wantErr)
			}
		})
	}
}

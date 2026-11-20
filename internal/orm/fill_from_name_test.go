package orm

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestFillFromName(t *testing.T) {
	ref := doctype.Field{Fieldname: "reference", Fieldtype: "Data", DefaultToName: true}
	tests := []struct {
		name  string
		field doctype.Field
		value any
		want  any
	}{
		{"blank takes the name", ref, "", "TRIP-2026-00001"},
		{"spaces take the name", ref, "   ", "TRIP-2026-00001"},
		{"missing takes the name", ref, nil, "TRIP-2026-00001"},
		{"a typed reference stays", ref, "PO-7781", "PO-7781"},
		{"other fields stay blank", doctype.Field{Fieldname: "reference", Fieldtype: "Data"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &doctype.Document{Name: "TRIP-2026-00001", Fields: map[string]any{}}
			if tt.value != nil {
				doc.Set("reference", tt.value)
			}
			fillFromName(tt.field, doc)
			if got := doc.Get("reference"); got != tt.want && !(got == nil && tt.want == nil) {
				t.Fatalf("reference = %v, want %v", got, tt.want)
			}
		})
	}
}

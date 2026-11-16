package usage

import (
	"slices"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestRecordDocTypes(t *testing.T) {
	tests := []struct {
		name     string
		doctypes []*doctype.DocType
		want     []string
	}{
		{name: "empty registry", want: nil},
		{name: "parents only", doctypes: []*doctype.DocType{{Name: "Invoice"}, {Name: "Customer"}}, want: []string{"Invoice", "Customer"}},
		{name: "child and single excluded", doctypes: []*doctype.DocType{
			{Name: "Invoice"},
			{Name: "Invoice Line", IsChildTable: true},
			{Name: "Settings", IsSingle: true},
		}, want: []string{"Invoice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := doctype.NewRegistry()
			for _, dt := range tt.doctypes {
				registry.Register(dt)
			}
			got := recordDocTypes(registry)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d record doctypes, want %d", len(got), len(tt.want))
			}
			for _, dt := range got {
				if !slices.Contains(tt.want, dt.Name) {
					t.Errorf("unexpected record doctype %q", dt.Name)
				}
			}
		})
	}
}

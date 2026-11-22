package schema

import (
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestRelabelsLoginTextOnlyWithinOneColumnType(t *testing.T) {
	tests := []struct {
		name     string
		dialect  string
		from, to string
		want     bool
	}{
		{name: "data to user on mysql", dialect: "mysql", from: "Data", to: "User", want: true},
		{name: "user to data on postgres", dialect: "postgres", from: "User", to: "Data", want: true},
		{name: "data to select is a conversion", dialect: "mysql", from: "Data", to: "Select", want: false},
		{name: "data to int is a conversion", dialect: "postgres", from: "Data", to: "Int", want: false},
		{name: "text to user is a conversion", dialect: "mysql", from: "Text", to: "User", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dialect := db.Resolve(tc.dialect)
			got := relabelsLoginText(&doctype.Field{Fieldname: "cashier", Fieldtype: tc.from}, &doctype.Field{Fieldname: "cashier", Fieldtype: tc.to}, dialect)
			if got != tc.want {
				t.Fatalf("relabelsLoginText(%s -> %s) = %t, want %t", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

package orm

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestNormalizeSQLValueStoresOptionalBlankTextAsNull(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field doctype.Field
		in    any
		want  any
	}{
		{"optional Data blank", doctype.Field{Fieldtype: "Data"}, "", nil},
		{"optional Link blank", doctype.Field{Fieldtype: "Link"}, "", nil},
		{"optional Small Text blank", doctype.Field{Fieldtype: "Small Text"}, "", nil},
		{"required Data blank kept", doctype.Field{Fieldtype: "Data", Reqd: true}, "", ""},
		{"optional Data value kept", doctype.Field{Fieldtype: "Data"}, "A-1", "A-1"},
		{"whitespace kept", doctype.Field{Fieldtype: "Data"}, " ", " "},
		{"non-text blank untouched", doctype.Field{Fieldtype: "Int"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeSQLValue(tc.field, tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("normalizeSQLValue = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

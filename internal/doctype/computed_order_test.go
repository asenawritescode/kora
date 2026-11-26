package doctype

import (
	"slices"
	"testing"
)

func TestComputeOrder(t *testing.T) {
	cases := []struct {
		name   string
		fields []cfInfo
		want   []string
	}{
		{
			name: "declaration order kept when nothing reads a computed field",
			fields: []cfInfo{
				{field: &Field{Fieldname: "a"}, expr: "qty * 2"},
				{field: &Field{Fieldname: "b"}, expr: "SUM(items.line_total)"},
			},
			want: []string{"a", "b"},
		},
		{
			name: "chain declared backwards runs inputs first",
			fields: []cfInfo{
				{field: &Field{Fieldname: "grand"}, expr: "total * 2"},
				{field: &Field{Fieldname: "total"}, expr: "subtotal"},
				{field: &Field{Fieldname: "subtotal"}, expr: "SUM(items.line_total)"},
			},
			want: []string{"subtotal", "total", "grand"},
		},
		{
			name: "child column with the same name is not a dependency",
			fields: []cfInfo{
				{field: &Field{Fieldname: "discount_amount"}, expr: "SUM(items.discount_amount)"},
				{field: &Field{Fieldname: "net"}, expr: "total - discount_amount"},
				{field: &Field{Fieldname: "total"}, expr: "gross"},
			},
			want: []string{"discount_amount", "total", "net"},
		},
		{
			name: "cycle keeps declaration order",
			fields: []cfInfo{
				{field: &Field{Fieldname: "a"}, expr: "b + 1"},
				{field: &Field{Fieldname: "b"}, expr: "a + 1"},
				{field: &Field{Fieldname: "c"}, expr: "a"},
			},
			want: []string{"a", "b", "c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, cf := range computeOrder(tc.fields) {
				got = append(got, cf.field.Fieldname)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

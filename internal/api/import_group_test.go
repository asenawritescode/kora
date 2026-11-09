package api

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestAssembleImportGroups(t *testing.T) {
	sale := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "customer", Fieldtype: "Data"},
		{Fieldname: "lines", Fieldtype: "Table", Options: "SaleLine"},
	}}
	line := &doctype.DocType{Name: "SaleLine", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "sku", Fieldtype: "Data"}, {Fieldname: "qty", Fieldtype: "Int"},
	}}
	reg := doctype.NewRegistry()
	reg.Register(sale)
	reg.Register(line)
	mapping := map[string]string{"Customer": "customer", "SKU": "lines.sku", "Qty": "lines.qty"}
	head, child, table, err := groupMapping(reg, sale, mapping)
	if err != nil || table != "lines" || len(head) != 1 || len(child) != 2 {
		t.Fatalf("mapping: %v %v %v %v", head, child, table, err)
	}
	col := map[string]int{"Sale No": 0, "Customer": 1, "SKU": 2, "Qty": 3}
	tests := []struct {
		name, csv string
		first     []int
		kids      []int
		errs      []string
	}{
		{"one group three lines", "A,Bob,x,1\nA,Bob,y,2\nA,,z,3", []int{1}, []int{3}, []string{""}},
		{"two groups", "A,Bob,x,1\nB,Ann,y,2", []int{1, 2}, []int{1, 1}, []string{"", ""}},
		{"inconsistent only its group", "A,Bob,x,1\nA,Eve,y,2\nB,Ann,z,3", []int{1, 3}, []int{0, 1}, []string{"import.group_inconsistent", ""}},
		{"not contiguous", "A,Bob,x,1\nB,Ann,y,2\nA,Bob,z,3", []int{1, 2, 3}, []int{1, 1, 0}, []string{"", "", "import.group_not_contiguous"}},
		{"empty group value", "A,Bob,x,1\n,Bob,y,2", []int{1, 2}, []int{1, 0}, []string{"", "import.key_missing"}},
		{"bad child value fails group", "A,Bob,x,one", []int{1}, []int{0}, []string{"not a valid number"}},
		{"empty child cells add no row", "A,Bob,,\nA,Bob,x,1", []int{1}, []int{1}, []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rows [][]string
			for _, l := range strings.Split(tt.csv, "\n") {
				rows = append(rows, strings.Split(l, ","))
			}
			us := assembleImportGroups(sale, line, table, head, child, col, "Sale No", rows)
			if len(us) != len(tt.first) {
				t.Fatalf("units=%d want %d", len(us), len(tt.first))
			}
			for i, u := range us {
				n := 0
				if k, ok := u.Data[table].([]map[string]any); ok {
					n = len(k)
				}
				msg := ""
				if u.Err != nil {
					msg = u.Err.Message
				}
				if u.First != tt.first[i] || n != tt.kids[i] || !strings.Contains(msg, tt.errs[i]) || (tt.errs[i] != "" && msg == "") {
					t.Fatalf("unit %d: first=%d kids=%d err=%q", i, u.First, n, msg)
				}
			}
		})
	}
	if _, _, _, err := groupMapping(reg, sale, map[string]string{"A": "lines.nope"}); err == nil {
		t.Fatal("unknown child field accepted")
	}
}

package doctype

import (
	"path/filepath"
	"testing"
)

// The shop packs are parked under config/_parked. Their fact models are checked
// together, as a workspace loads them, so links into the selling layer resolve.
func TestShopPacksDeclareFactModels(t *testing.T) {
	var all []*DocType
	for _, pack := range []string{"erpsell", "erp-purchasing", "erp-inventory"} {
		doctypes, err := ParseConfigTree(filepath.Join("..", "..", "config", "_parked", pack))
		if err != nil {
			t.Fatalf("parse %s: %v", pack, err)
		}
		all = append(all, doctypes...)
	}
	for _, problem := range ValidateAll(all) {
		t.Errorf("%s", problem.Error())
	}

	byName := map[string]*DocType{}
	for _, dt := range all {
		byName[dt.Name] = dt
	}
	cases := []struct {
		doctype string
		time    string
		lines   string
		status  string
	}{
		{"Sale", "sale_date", "items", "status"},
		{"Stock Movement", "creation", "", ""},
		{"Goods Receipt", "received_on", "items", ""},
		{"Supplier Bill", "bill_date", "", "status"},
	}
	for _, tc := range cases {
		t.Run(tc.doctype, func(t *testing.T) {
			dt := byName[tc.doctype]
			if dt == nil || dt.Facts == nil {
				t.Fatalf("%s has no facts block", tc.doctype)
			}
			if dt.Facts.Time != tc.time || dt.Facts.Lines != tc.lines || dt.Facts.StatusField != tc.status {
				t.Errorf("facts time/lines/status = %q/%q/%q, want %q/%q/%q",
					dt.Facts.Time, dt.Facts.Lines, dt.Facts.StatusField, tc.time, tc.lines, tc.status)
			}
		})
	}
	// Stock Adjustment writes a Stock Movement through its quantity effect, so the
	// movement fact covers it and it declares no facts of its own.
	if adj := byName["Stock Adjustment"]; adj == nil || adj.Facts != nil {
		t.Errorf("Stock Adjustment must have no facts block (covered by Stock Movement)")
	}
}

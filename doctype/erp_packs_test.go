package doctype

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The ERP packs are built in layers on top of pos-base. Each layer is checked
// alone (the repo-wide pack audit) and here together, the way a workspace gets
// them, so a link to a record in another layer cannot dangle.

var erpPackLayers = []string{"pos-base", "erp-purchasing"}

func loadERPLayers(t *testing.T) []*DocType {
	t.Helper()
	var all []*DocType
	seen := map[string]string{}
	for _, pack := range erpPackLayers {
		doctypes, err := ParseConfigTree(filepath.Join("..", "config", pack))
		if err != nil {
			t.Fatalf("parse %s: %v", pack, err)
		}
		for _, dt := range doctypes {
			if other, dup := seen[dt.Name]; dup {
				t.Errorf("record type %q is defined in both %s and %s", dt.Name, other, pack)
			}
			seen[dt.Name] = pack
			all = append(all, dt)
		}
	}
	return all
}

func TestERPLayersComposeWithoutDanglingLinks(t *testing.T) {
	for _, problem := range ValidateAll(loadERPLayers(t)) {
		t.Errorf("%s", problem.Error())
	}
}

func TestERPLayersShareOneRoleDefinition(t *testing.T) {
	type role struct{ description string }
	seen := map[string]Role{}
	for _, pack := range erpPackLayers {
		roles, _, err := ParseRolesDirectory(filepath.Join("..", "config", pack, "doctypes"))
		if err != nil {
			t.Fatalf("roles %s: %v", pack, err)
		}
		for _, r := range roles {
			if prev, ok := seen[r.Name]; ok && (prev.Description != r.Description || prev.WorkspaceAccess != r.WorkspaceAccess) {
				t.Errorf("role %q is described differently in two layers", r.Name)
			}
			seen[r.Name] = *r
		}
	}
}

var erpMoneyRecords = map[string]string{
	"Purchase Order": "total", "Goods Receipt": "total",
	"Supplier Bill": "amount", "Supplier Payment": "amount",
}

func TestERPMoneyRecordsCarryCurrencyAndFrozenRate(t *testing.T) {
	byName := map[string]*DocType{}
	for _, dt := range loadERPLayers(t) {
		byName[dt.Name] = dt
	}
	for name, amountField := range erpMoneyRecords {
		dt := byName[name]
		if dt == nil {
			t.Errorf("%s is missing", name)
			continue
		}
		if c := dt.GetField("currency"); c == nil || c.Fieldtype != "Link" || c.Options != "Currency" {
			t.Errorf("%s needs a currency field linked to Currency", name)
		}
		if r := dt.GetField("exchange_rate"); r == nil || r.Fieldtype != "Float" || !hasMinConstraintAboveZero(r) {
			t.Errorf("%s needs a Float exchange_rate constrained to more than zero", name)
		}
		want := fmt.Sprintf("(round (* (round %s 9) (round exchange_rate 9)) 2)", amountField)
		if b := dt.GetField("base_" + amountField); b == nil || b.Computed != want || !b.ReadOnly {
			t.Errorf("%s needs a read-only base_%s computed as %q", name, amountField, want)
		}
	}
}

// The Engine fills number defaults in as text, and a calculation cannot
// multiply text. Every calculated field in the ERP layers must still work when
// each number it uses arrives as text.
func TestERPCalculatedFieldsWorkWithTextNumbers(t *testing.T) {
	for _, dt := range loadERPLayers(t) {
		for _, f := range dt.Fields {
			if f.Computed == "" || f.Fieldtype == "Table" {
				continue
			}
			t.Run(dt.Name+"."+f.Fieldname, func(t *testing.T) {
				doc := NewDocument("")
				for _, other := range dt.Fields {
					if other.Computed == "" && other.IsNumeric() && other.Fieldtype != "Table" {
						doc.Set(other.Fieldname, "2")
					}
					if other.Fieldtype == "Table" {
						var rows []*Document
						row := NewDocument("")
						childName := other.Options
						for _, child := range loadERPLayers(t) {
							if child.Name != childName {
								continue
							}
							for _, cf := range child.Fields {
								if cf.Computed == "" && cf.IsNumeric() {
									row.Set(cf.Fieldname, "2")
								}
							}
							rows = append(rows, row)
							doc.SetTable(other.Fieldname, rows)
							// Give the row its own calculated values first.
							_ = ComputeFields(child, row)
						}
					}
				}
				if err := ComputeFields(dt, doc); err != nil {
					t.Fatal(err)
				}
				if doc.Get(f.Fieldname) == nil {
					t.Fatalf("%s.%s = nil with text numbers (expression %q)", dt.Name, f.Fieldname, f.Computed)
				}
			})
		}
	}
}

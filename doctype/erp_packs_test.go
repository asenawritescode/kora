package doctype

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The ERP packs are built in layers on top of pos-base. Each layer is checked
// alone (the repo-wide pack audit) and here together, the way a workspace gets
// them, so a link to a record in another layer cannot dangle.

var erpPackLayers = []string{"pos-base", "erp-purchasing", "erp-accounting", "erp-inventory"}

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

func journalEntryFor(t *testing.T, debit, credit float64) (*DocType, *Document, *Registry) {
	t.Helper()
	byName := map[string]*DocType{}
	all := loadERPLayers(t)
	for _, dt := range all {
		byName[dt.Name] = dt
	}
	reg := NewRegistry()
	reg.LoadFull(all, []*Role{{Name: AdminRole}}, nil)
	entry, line := byName["Journal Entry"], byName["Journal Line"]
	doc := NewDocument("")
	doc.Set("reference", "JE-1")
	doc.Set("posting_date", "2026-11-08")
	var rows []*Document
	for _, amounts := range [][2]float64{{debit, 0}, {0, credit}} {
		row := NewDocument("")
		row.Set("account", "ACC-1")
		row.Set("debit", amounts[0])
		row.Set("credit", amounts[1])
		ApplyDefaultValues(line, row)
		rows = append(rows, row)
	}
	doc.SetTable("lines", rows)
	ApplyDefaultValues(entry, doc)
	if err := ComputeFields(entry, doc); err != nil {
		t.Fatal(err)
	}
	return entry, doc, reg
}

func TestERPJournalEntryMustBalance(t *testing.T) {
	cases := []struct {
		name          string
		debit, credit float64
		wantRejected  bool
	}{
		{"balanced", 150.50, 150.50, false},
		{"debit too high", 150.50, 100, true},
		{"credit too high", 100, 150.50, true},
		{"cents add up", 0.1 + 0.2, 0.3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, doc, reg := journalEntryFor(t, tc.debit, tc.credit)
			problems := ValidateDocument(entry, doc, reg, nil)
			var balance int
			for _, p := range problems {
				if p.Message == "Debits and credits must be equal." {
					balance++
				}
			}
			if tc.wantRejected && balance == 0 {
				t.Fatalf("unbalanced entry (debit %v, credit %v) was not rejected; difference=%v", tc.debit, tc.credit, doc.Get("difference"))
			}
			if !tc.wantRejected && balance > 0 {
				t.Fatalf("balanced entry was rejected: %v", problems)
			}
		})
	}
}

func TestERPJournalEntryInForeignCurrencyKeepsBooksInBase(t *testing.T) {
	_, doc, _ := journalEntryFor(t, 100, 100)
	doc.Set("exchange_rate", 129.5)
	byName := map[string]*DocType{}
	for _, dt := range loadERPLayers(t) {
		byName[dt.Name] = dt
	}
	if err := ComputeFields(byName["Journal Entry"], doc); err != nil {
		t.Fatal(err)
	}
	debit, _ := toFloat64(doc.Get("base_total_debit"))
	credit, _ := toFloat64(doc.Get("base_total_credit"))
	if debit != 12950.0 || credit != 12950.0 {
		t.Fatalf("base debit/credit = %v / %v, want 12950 / 12950", doc.Get("base_total_debit"), doc.Get("base_total_credit"))
	}
}

// The marketplace refuses a release when a Link points at a record whose title
// cannot be searched (the picker would find nothing). The title field must be
// visible, listed in search_fields, and have search_index: true; unique alone
// does not count. This mirrors site/pack_manifest.go.
func TestERPLinkTargetsHaveSearchableTitles(t *testing.T) {
	all := loadERPLayers(t)
	byName := map[string]*DocType{}
	for _, dt := range all {
		byName[dt.Name] = dt
	}
	checked := map[string]bool{}
	for _, dt := range all {
		for _, f := range dt.Fields {
			if f.Fieldtype != "Link" || checked[f.Options] {
				continue
			}
			checked[f.Options] = true
			target := byName[f.Options]
			if target == nil || target.TitleField == "" {
				continue
			}
			title := target.GetField(target.TitleField)
			listed := false
			for _, s := range strings.Split(target.SearchFields, ",") {
				if strings.TrimSpace(s) == target.TitleField {
					listed = true
				}
			}
			if !listed {
				t.Errorf("%s (linked from %s.%s): title field %q must be in search_fields", target.Name, dt.Name, f.Fieldname, target.TitleField)
			}
			if title == nil || title.Hidden || !title.SearchIndex {
				t.Errorf("%s (linked from %s.%s): title field %q must be indexed (search_index: true)", target.Name, dt.Name, f.Fieldname, target.TitleField)
			}
		}
	}
}

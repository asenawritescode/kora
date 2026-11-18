package doctype

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// factFixture is a Sale record with a child table of lines, the shape the facts tests vary.
func factFixture() (*DocType, map[string]*DocType, *FactModel) {
	sale := &DocType{Name: "Sale", Fields: []Field{
		{Fieldname: "posting_date", Fieldtype: "Date"},
		{Fieldname: "total", Fieldtype: "Currency"},
		{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nPaid"},
		{Fieldname: "register", Fieldtype: "Link", Options: "Register"},
		{Fieldname: "note", Fieldtype: "Data"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}}
	line := &DocType{Name: "Sale Item", IsChildTable: true, Fields: []Field{
		{Fieldname: "item", Fieldtype: "Link", Options: "Item"},
		{Fieldname: "amount", Fieldtype: "Currency"},
	}}
	facts := &FactModel{
		Time:        "posting_date",
		Lines:       "items",
		Measures:    []FactMeasure{{Field: "total"}, {Field: "items.amount"}},
		Dimensions:  []FactDimension{{Field: "register"}, {Field: "items.item"}},
		StatusField: "status",
	}
	return sale, map[string]*DocType{"Sale": sale, "Sale Item": line}, facts
}

func factProblems(dt *DocType, reg map[string]*DocType, f *FactModel) []string {
	var out []string
	for _, e := range f.Validate(dt, reg) {
		out = append(out, e.Message)
	}
	return out
}

func TestFactModelValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dt *DocType, reg map[string]*DocType, f *FactModel)
		want   string // substring of one problem; "" means the model is valid
	}{
		{"valid with lines and status", func(*DocType, map[string]*DocType, *FactModel) {}, ""},
		{"valid with the creation time", func(_ *DocType, _ map[string]*DocType, f *FactModel) { f.Time = "creation" }, ""},
		{"no time axis", func(_ *DocType, _ map[string]*DocType, f *FactModel) { f.Time = "" }, "must be a Date or Datetime field"},
		{"time is not a date", func(_ *DocType, _ map[string]*DocType, f *FactModel) { f.Time = "note" }, "must be a Date or Datetime field"},
		{"lines is not a table", func(_ *DocType, _ map[string]*DocType, f *FactModel) { f.Lines = "note" }, "must be a Table field"},
		{"lines table has no record type", func(dt *DocType, reg map[string]*DocType, _ *FactModel) {
			delete(reg, "Sale Item")
		}, "has no record type"},
		{"no measures", func(_ *DocType, _ map[string]*DocType, f *FactModel) { f.Measures = nil }, "at least one measure"},
		{"measure is not a number", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = []FactMeasure{{Field: "register"}}
		}, "must be an Int, Float, Currency or Percent field"},
		{"child measure without lines", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Lines = ""
			f.Measures = []FactMeasure{{Field: "items.amount"}}
		}, "is a child field, but facts has no lines"},
		{"child measure with the wrong prefix", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = []FactMeasure{{Field: "other.amount"}}
		}, "must be a field on Sale or on the items table"},
		{"child measure field missing", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = []FactMeasure{{Field: "items.nope"}}
		}, "must be an Int, Float, Currency or Percent field"},
		{"dimension is a table", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Dimensions = []FactDimension{{Field: "items"}}
		}, "must be a Link, Select, Data or Check field"},
		{"status field is not a select", func(_ *DocType, _ map[string]*DocType, f *FactModel) { f.StatusField = "register" }, "must be a Select field"},
		{"duplicate column name", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = append(f.Measures, FactMeasure{Field: "items.amount", Name: "total"})
		}, "declared more than once"},
		{"measure name clashes with a dimension", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = append(f.Measures, FactMeasure{Field: "total", Name: "register"})
		}, "declared more than once"},
		{"measure named like a fixed column", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = append(f.Measures, FactMeasure{Field: "total", Name: "owner"})
		}, "is reserved for the fact table"},
		{"dimension named like a fixed column", func(dt *DocType, _ map[string]*DocType, f *FactModel) {
			dt.Fields = append(dt.Fields, Field{Fieldname: "business_date", Fieldtype: "Data"})
			f.Dimensions = append(f.Dimensions, FactDimension{Field: "business_date"})
		}, "is reserved for the fact table"},
		{"dimension named like the status column", func(dt *DocType, _ map[string]*DocType, f *FactModel) {
			dt.Fields = append(dt.Fields, Field{Fieldname: "status", Fieldtype: "Data"})
			f.Dimensions = append(f.Dimensions, FactDimension{Field: "status"})
		}, "is reserved for the fact table"},
		{"measure named like the time field is allowed", func(dt *DocType, _ map[string]*DocType, f *FactModel) {
			f.Measures = append(f.Measures, FactMeasure{Field: "total", Name: "posting_date"})
		}, ""},
		{"dimension repeats a leaf name", func(_ *DocType, _ map[string]*DocType, f *FactModel) {
			f.Dimensions = append(f.Dimensions, FactDimension{Field: "items.amount"})
		}, "declared more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dt, reg, f := factFixture()
			tc.mutate(dt, reg, f)
			problems := factProblems(dt, reg, f)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("valid model rejected: %v", problems)
				}
				return
			}
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("want a problem containing %q, got %v", tc.want, problems)
		})
	}
}

func TestFactModelColumnsAndTableNames(t *testing.T) {
	_, _, f := factFixture()
	f.Measures = []FactMeasure{{Field: "total"}, {Field: "items.amount", Name: "line_amount"}}
	want := []FactColumn{
		{Name: "register", Type: FactColumnText},
		{Name: "item", Type: FactColumnText},
		{Name: "total", Type: FactColumnDecimal},
		{Name: "line_amount", Type: FactColumnDecimal},
	}
	if got := f.Columns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Columns() = %+v, want %+v", got, want)
	}
	dt := &DocType{Name: "Stock Entry"}
	if got := f.TableName(dt); got != "_kora_fact_stock_entry" {
		t.Fatalf("TableName = %q", got)
	}
	if got := f.LineTableName(dt); got != "_kora_fact_stock_entry_line" {
		t.Fatalf("LineTableName = %q", got)
	}
}

func TestFactModelParsesAndRoundTrips(t *testing.T) {
	src := "name: Sale\nmodule: POS\nfields:\n  - fieldname: posting_date\n    fieldtype: Date\n" +
		"facts:\n  time: posting_date\n  measures:\n    - field: total\n      name: amount\n  dimensions:\n    - field: register\n"
	dt, err := ParseYAML([]byte(src), "sale.yaml")
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if dt.Facts == nil || dt.Facts.Time != "posting_date" || dt.Facts.Measures[0].Name != "amount" {
		t.Fatalf("facts not parsed: %+v", dt.Facts)
	}
	snapshot := &ConfigSnapshot{DocTypes: []*DocType{dt}}
	sexpr := ToSExpr(snapshot)
	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr: %v", err)
	}
	if !reflect.DeepEqual(parsed.DocTypes[0].Facts, dt.Facts) {
		t.Fatalf("facts changed in the snapshot round trip:\n%+v\n%+v", parsed.DocTypes[0].Facts, dt.Facts)
	}
}

func TestFactModelUnknownKeysGetLineNumbersAndSuggestions(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"typo in facts key", "name: Sale\nmodule: POS\nfacts:\n  time: posting_date\n  measure:\n    - field: total\n",
			[]string{`line 5: unknown key "measure" in doctype`, `did you mean "measures"?`}},
		{"typo in measure item", "name: Sale\nmodule: POS\nfacts:\n  time: posting_date\n  measures:\n    - feild: total\n",
			[]string{`line 6: unknown key "feild"`, `did you mean "field"?`}},
		{"typo in dimension item", "name: Sale\nmodule: POS\nfacts:\n  time: posting_date\n  dimensions:\n    - fieldd: register\n",
			[]string{`line 6: unknown key "fieldd"`, `did you mean "field"?`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseYAML([]byte(tc.yaml), "sale.yaml")
			if err == nil {
				t.Fatal("unknown facts key accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

func TestConfigPacksDeclareValidFacts(t *testing.T) {
	cases := []struct {
		pack, doctype string
	}{
		{"pos", "Sale"},
		{"inventory", "Stock Entry"},
	}
	for _, tc := range cases {
		t.Run(tc.pack, func(t *testing.T) {
			doctypes, err := ParseConfigTree(filepath.Join("..", "..", "config", tc.pack))
			if err != nil {
				t.Fatalf("parse %s: %v", tc.pack, err)
			}
			reg := map[string]*DocType{}
			for _, dt := range doctypes {
				reg[dt.Name] = dt
			}
			found := false
			for _, dt := range doctypes {
				if dt.Name != tc.doctype {
					continue
				}
				found = true
				if dt.Facts == nil {
					t.Fatalf("%s declares no facts", tc.doctype)
				}
				if problems := factProblems(dt, reg, dt.Facts); len(problems) != 0 {
					t.Fatalf("%s facts rejected: %v", tc.doctype, problems)
				}
			}
			if !found {
				t.Fatalf("pack %s has no %s", tc.pack, tc.doctype)
			}
		})
	}
}

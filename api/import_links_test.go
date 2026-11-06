package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func linkTestRegistry() (*doctype.Registry, *doctype.DocType) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Cat", TitleField: "label", Fields: []doctype.Field{{Fieldname: "label", Fieldtype: "Data"}}})
	reg.Register(&doctype.DocType{Name: "Plain", Fields: []doctype.Field{{Fieldname: "x", Fieldtype: "Data"}}})
	reg.Register(&doctype.DocType{Name: "Line", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "cat", Fieldtype: "Link", Options: "Cat"}}})
	dt := &doctype.DocType{Name: "Thing", Fields: []doctype.Field{
		{Fieldname: "category", Fieldtype: "Link", Options: "Cat"},
		{Fieldname: "plain", Fieldtype: "Link", Options: "Plain"},
		{Fieldname: "lines", Fieldtype: "Table", Options: "Line"},
	}}
	reg.Register(dt)
	return reg, dt
}

func TestResolveImportLinks(t *testing.T) {
	// Cat records: CAT-1 "Grocery", CAT-2 "Dairy", CAT-3 and CAT-4 both "Twin".
	titles := map[string][]string{"grocery": {"CAT-1"}, "dairy": {"CAT-2"}, "twin": {"CAT-3", "CAT-4"}}
	names := map[string][]string{"cat-1": {"CAT-1"}}
	lookup := func(target *doctype.DocType, field string, values []string) (map[string][]string, error) {
		src := titles
		if field == "name" {
			src = names
		}
		out := map[string][]string{}
		for _, v := range values {
			if m, ok := src[strings.ToLower(v)]; ok {
				out[strings.ToLower(v)] = m
			}
		}
		return out, nil
	}
	tests := []struct {
		name    string
		data    map[string]any
		want    string // resolved category, or "" when the row must fail
		errPart string
	}{
		{"plain title resolves to record name", map[string]any{"category": "Grocery"}, "CAT-1", ""},
		{"title match ignores case", map[string]any{"category": "dairy"}, "CAT-2", ""},
		{"record name is kept", map[string]any{"category": "CAT-1"}, "CAT-1", ""},
		{"unknown value fails the row", map[string]any{"category": "Nope"}, "", "import.link_unresolved"},
		{"ambiguous title fails the row", map[string]any{"category": "Twin"}, "", "import.link_ambiguous"},
		{"target without title field fails the row", map[string]any{"plain": "foo"}, "", "import.link_unresolved"},
		{"empty link is untouched", map[string]any{"x": "1"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, dt := linkTestRegistry()
			units := []importUnit{{First: 3, Data: tt.data}}
			if err := resolveImportLinks(reg, dt, "", units, lookup); err != nil {
				t.Fatal(err)
			}
			u := units[0]
			if tt.errPart != "" {
				if u.Err == nil || !strings.Contains(u.Err.Message, tt.errPart) || !strings.Contains(string(u.Err.Details), `"row":3`) {
					t.Fatalf("want %s error, got %+v", tt.errPart, u.Err)
				}
				return
			}
			if u.Err != nil || (tt.want != "" && u.Data["category"] != tt.want) {
				t.Fatalf("err=%v data=%v", u.Err, u.Data)
			}
		})
	}
}

func TestResolveImportLinksChildRowsAndSkippedUnits(t *testing.T) {
	reg, dt := linkTestRegistry()
	called := 0
	lookup := func(_ *doctype.DocType, field string, values []string) (map[string][]string, error) {
		called++
		if field == "name" {
			return nil, nil
		}
		return map[string][]string{"grocery": {"CAT-1"}}, nil
	}
	units := []importUnit{
		{First: 1, Data: map[string]any{"lines": []map[string]any{{"cat": "Grocery"}, {"cat": "Missing"}}}},
		{First: 2, Err: importUnitError("bad", 2, "x"), Data: map[string]any{"category": "Ignored"}},
	}
	if err := resolveImportLinks(reg, dt, "lines", units, lookup); err != nil {
		t.Fatal(err)
	}
	if units[0].Err == nil || !strings.Contains(string(units[0].Err.Details), "lines.cat") {
		t.Fatalf("missing child link must fail its group: %+v", units[0].Err)
	}
	if units[1].Err == nil || units[1].Err.Message != "bad" || called != 2 {
		t.Fatalf("pre-failed unit changed or looked up: %+v calls=%d", units[1].Err, called)
	}
}

func TestResolveImportLinksLookupError(t *testing.T) {
	reg, dt := linkTestRegistry()
	boom := errors.New("boom")
	err := resolveImportLinks(reg, dt, "", []importUnit{{First: 1, Data: map[string]any{"category": "A"}}}, func(*doctype.DocType, string, []string) (map[string][]string, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

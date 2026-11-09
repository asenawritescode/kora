package api

import (
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func linkTitleRegistry() *doctype.Registry {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Product", TitleField: "label", Fields: []doctype.Field{{Fieldname: "label", Fieldtype: "Data"}}})
	reg.Register(&doctype.DocType{Name: "Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "qty", Fieldtype: "Float"},
	}})
	reg.Register(&doctype.DocType{Name: "Order", Fields: []doctype.Field{
		{Fieldname: "main", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "lines", Fieldtype: "Table", Options: "Line"},
		{Fieldname: "note", Fieldtype: "Data"},
	}})
	return reg
}

// TestAddLinkTitles: every Link value, on the document and in child rows, gets
// a "<field>_title" sibling; unresolved links fall back to the stored value.
func TestAddLinkTitles(t *testing.T) {
	reg := linkTitleRegistry()
	titles := map[string]string{"P-1": "Maize Flour", "P-2": "Bread"}
	for _, tc := range []struct {
		name string
		doc  map[string]any
		want map[string]any
	}{
		{
			name: "document and child rows",
			doc:  map[string]any{"main": "P-1", "lines": []map[string]any{{"product": "P-1", "qty": 2}, {"product": "P-2", "qty": 1}}},
			want: map[string]any{"main": "P-1", "main_title": "Maize Flour", "lines": []map[string]any{
				{"product": "P-1", "product_title": "Maize Flour", "qty": 2}, {"product": "P-2", "product_title": "Bread", "qty": 1}}},
		},
		{
			name: "unknown target record falls back to its name",
			doc:  map[string]any{"main": "P-9"},
			want: map[string]any{"main": "P-9", "main_title": "P-9"},
		},
		{
			name: "empty link gets no title",
			doc:  map[string]any{"main": "", "note": "n"},
			want: map[string]any{"main": "", "note": "n"},
		},
		{
			name: "an existing key is not overwritten",
			doc:  map[string]any{"main": "P-1", "main_title": "custom"},
			want: map[string]any{"main": "P-1", "main_title": "custom"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			lookup := func(target *doctype.DocType, names []string) (map[string]string, error) {
				calls++
				if target.Name != "Product" {
					t.Fatalf("lookup for %s", target.Name)
				}
				out := map[string]string{}
				for _, n := range names {
					if v, ok := titles[n]; ok {
						out[n] = v
					}
				}
				return out, nil
			}
			if err := addLinkTitles(tc.doc, reg.Get("Order"), reg, lookup); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tc.doc, tc.want) {
				t.Fatalf("doc = %#v\nwant  %#v", tc.doc, tc.want)
			}
			if calls > 1 {
				t.Fatalf("lookup called %d times, want one batch per target", calls)
			}
		})
	}
}

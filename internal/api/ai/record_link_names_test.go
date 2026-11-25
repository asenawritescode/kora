package ai

import (
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestLinkCodesGroupsDistinctCodesByTarget(t *testing.T) {
	fields := []doctype.Field{
		{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"},
		{Fieldname: "approver", Fieldtype: "Link", Options: "User"},
		{Fieldname: "reference", Fieldtype: "Data"},
		{Fieldname: "buyer", Fieldtype: "Link", Options: "Supplier"},
	}
	docs := []*doctype.Document{
		{Name: "SB-1", Fields: map[string]any{"supplier": "SUPP-1", "approver": "ana", "reference": "SUPP-9", "buyer": "SUPP-2"}},
		{Name: "SB-2", Fields: map[string]any{"supplier": "SUPP-1", "approver": "", "buyer": ""}},
		{Name: "SB-3", Fields: map[string]any{"supplier": "  ", "buyer": "SUPP-2"}},
		{Name: "SB-4", Fields: map[string]any{"supplier": 7}},
		nil,
	}
	cases := []struct {
		name   string
		fields []doctype.Field
		docs   []*doctype.Document
		want   map[string][]string
	}{
		{"de-duplicates codes across documents and fields", fields, docs,
			map[string][]string{"Supplier": {"SUPP-1", "SUPP-2"}, "User": {"ana"}}},
		{"no documents give no codes", fields, nil, map[string][]string{}},
		{"non-Link fields and empty values are skipped", []doctype.Field{{Fieldname: "reference", Fieldtype: "Data"}}, docs, map[string][]string{}},
		{"a Link without a target is skipped", []doctype.Field{{Fieldname: "supplier", Fieldtype: "Link"}}, docs, map[string][]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := linkCodes(tc.fields, tc.docs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("linkCodes() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestLinkNamesShowPrintsNameWithCode(t *testing.T) {
	names := linkNames{"supplier": {"SUPP-1": "Mr. Ouma Son", "SUPP-2": "SUPP-2"}}
	cases := []struct {
		name  string
		names linkNames
		field string
		code  string
		want  string
	}{
		{"name and code", names, "supplier", "SUPP-1", "Mr. Ouma Son (SUPP-1)"},
		{"name missing for code", names, "supplier", "SUPP-3", "SUPP-3"},
		{"name equal to code", names, "supplier", "SUPP-2", "SUPP-2"},
		{"field without names", names, "buyer", "SUPP-1", "SUPP-1"},
		{"no names at all", nil, "supplier", "SUPP-1", "SUPP-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.names.show(tc.field, tc.code); got != tc.want {
				t.Fatalf("show(%q, %q) = %q, want %q", tc.field, tc.code, got, tc.want)
			}
		})
	}
}

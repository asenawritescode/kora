package ai

import (
	"slices"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

func TestLinkValuesReadsTextOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    string
		value any
		want  []string
	}{
		{"like text", "like", "Ouma Son", []string{"Ouma Son"}},
		{"equals text", "=", "SUPP-0001", []string{"SUPP-0001"}},
		{"number is not text", "=", 4, nil},
		{"in list of text", "in", []any{"a", "b"}, []string{"a", "b"}},
		{"in list with a number", "in", []any{"a", 2}, nil},
		{"in needs a list", "in", "a", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkValues(tc.op, tc.value); len(got) != len(tc.want) {
				t.Fatalf("linkValues = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMatchNoteNamesOneOrMany(t *testing.T) {
	one := []orm.TitleMatch{{Name: "SUPP-0001", Title: "Mr. Ouma Son"}}
	many := []orm.TitleMatch{{Name: "SUPP-0001", Title: "Mr. Ouma Son"}, {Name: "SUPP-0002", Title: "Mr. Ouma Granary"}, {Name: "SUPP-0003", Title: "Patrice Ouma"}}
	for _, tc := range []struct {
		name    string
		matches []orm.TitleMatch
		want    string
	}{
		{"one record", one, `Matched supplier "oumas son" → Mr. Ouma Son (SUPP-0001).`},
		{"several records", many, `Matched supplier "oumas son" → 3 suppliers: Mr. Ouma Son (SUPP-0001), Mr. Ouma Granary (SUPP-0002), Patrice Ouma (SUPP-0003).`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchNote("supplier", "oumas son", "Supplier", tc.matches); got != tc.want {
				t.Errorf("matchNote = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoTargetMatchTextOmitsClosestWhenNone(t *testing.T) {
	closest := []orm.TitleMatch{{Name: "A", Title: "Alpha"}, {Name: "B", Title: "Beta"}, {Name: "C", Title: "Gamma"}}
	for _, tc := range []struct {
		name    string
		closest []orm.TitleMatch
		want    string
	}{
		{"with closest", closest, `No Supplier matches "zzz". Closest: Alpha, Beta, Gamma.`},
		{"without closest", nil, `No Supplier matches "zzz".`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := noTargetMatchText("Supplier", "zzz", tc.closest); got != tc.want {
				t.Errorf("noTargetMatchText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLinkSearchFieldsReadsLinksNamedInSearchFields(t *testing.T) {
	fields := []doctype.Field{
		{Fieldname: "reference", Fieldtype: "Data"},
		{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"},
		{Fieldname: "branch", Fieldtype: "Link", Options: "Branch"},
		{Fieldname: "approver", Fieldtype: "Link", Options: "User", Hidden: true},
	}
	for _, tc := range []struct {
		name         string
		searchFields string
		want         []string
	}{
		{"one link", "reference, supplier", []string{"supplier"}},
		{"spaces are trimmed", "  supplier ,branch  ", []string{"supplier", "branch"}},
		{"duplicates appear once, in declaration order", "branch, supplier, branch", []string{"supplier", "branch"}},
		{"unknown names are skipped", "missing, supplier", []string{"supplier"}},
		{"non-link names are skipped", "reference", nil},
		{"hidden links are skipped", "approver, supplier", []string{"supplier"}},
		{"empty search fields give none", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dt := &doctype.DocType{Name: "Supplier Bill", SearchFields: tc.searchFields, Fields: fields}
			var got []string
			for _, f := range linkSearchFields(dt) {
				got = append(got, f.Fieldname)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("linkSearchFields(%q) = %q, want %q", tc.searchFields, got, tc.want)
			}
		})
	}
}

package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/schema"
)

func saleType(fields ...doctype.Field) *doctype.DocType {
	return &doctype.DocType{Name: "Sale", Fields: fields}
}

func snapshotOf(types ...*doctype.DocType) *doctype.ConfigSnapshot {
	return &doctype.ConfigSnapshot{DocTypes: types}
}

func TestPackageUpgradeBlockers(t *testing.T) {
	note := doctype.Field{Fieldname: "note", Fieldtype: "Data"}
	live := map[string]*schema.TableInfo{"tabSale": {Name: "tabSale"}}
	tests := []struct {
		name     string
		current  *doctype.DocType
		incoming *doctype.DocType
		live     map[string]*schema.TableInfo
		records  int64
		want     []string
		counted  bool
	}{
		{"a required field with no default on a table with records", saleType(note), saleType(note, doctype.Field{Fieldname: "total", Fieldtype: "Currency", Reqd: true}), live, 3,
			[]string{`Sale has 3 records and the package adds required field "total"`}, true},
		{"a required field that has a default", saleType(note), saleType(note, doctype.Field{Fieldname: "total", Fieldtype: "Currency", Reqd: true, Default: "0"}), live, 3, nil, false},
		{"an optional field", saleType(note), saleType(note, doctype.Field{Fieldname: "customer", Fieldtype: "Data"}), live, 3, nil, false},
		{"a unique field on a table with records", saleType(note), saleType(note, doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true}), live, 2,
			[]string{`Sale has 2 records and the package adds unique field "reference"`}, true},
		{"a required unique field is named once", saleType(note), saleType(note, doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true, Reqd: true}), live, 2,
			[]string{`Sale has 2 records and the package adds required, unique field "reference"`}, true},
		{"the same change on an empty table", saleType(note), saleType(note, doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true, Reqd: true}), live, 0, nil, true},
		{"a table the tenant does not have yet", saleType(note), saleType(note, doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true}), map[string]*schema.TableInfo{}, 5, nil, false},
		{"a field that becomes unique", saleType(doctype.Field{Fieldname: "reference", Fieldtype: "Data"}), saleType(doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true}), live, 4,
			[]string{`Sale has 4 records and the package adds unique field "reference"`}, true},
		{"a field that is unchanged", saleType(doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true}), saleType(doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true}), live, 4, nil, false},
		{"several risky fields in one record type", saleType(note), saleType(note, doctype.Field{Fieldname: "total", Fieldtype: "Currency", Reqd: true}, doctype.Field{Fieldname: "reference", Fieldtype: "Data", Unique: true}), live, 7,
			[]string{`Sale has 7 records and the package adds required field "total", unique field "reference"`}, true},
		{"a record type the tenant's configuration does not list", nil, saleType(doctype.Field{Fieldname: "total", Fieldtype: "Currency", Reqd: true}), live, 1,
			[]string{`Sale has 1 records and the package adds required field "total"`}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counted := false
			count := func(_ context.Context, table string) (int64, error) {
				counted = true
				if table != "tabSale" {
					t.Fatalf("counted %q, want tabSale", table)
				}
				return tt.records, nil
			}
			current := snapshotOf()
			if tt.current != nil {
				current = snapshotOf(tt.current)
			}
			got, err := packageUpgradeBlockers(context.Background(), tt.live, count, current, snapshotOf(tt.incoming))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("blockers = %q, want %q", got, tt.want)
			}
			if counted != tt.counted {
				t.Fatalf("counted records = %v, want %v (a table is only counted when a package field could break it)", counted, tt.counted)
			}
		})
	}
}

func TestPackageUpgradeBlockersStopsWhenRecordsCannotBeCounted(t *testing.T) {
	boom := errors.New("database is away")
	live := map[string]*schema.TableInfo{"tabSale": {Name: "tabSale"}}
	incoming := saleType(doctype.Field{Fieldname: "total", Fieldtype: "Currency", Reqd: true})
	_, err := packageUpgradeBlockers(context.Background(), live, func(context.Context, string) (int64, error) { return 0, boom }, snapshotOf(saleType()), snapshotOf(incoming))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the counting failure, never a guess that the upgrade is safe", err)
	}
}

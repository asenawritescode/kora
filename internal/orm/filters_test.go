package orm

import (
	"reflect"
	"testing"
)

func TestFilterSetToSQL_ViewOperatorAliases(t *testing.T) {
	tests := []struct {
		name    string
		filter  Filter
		wantSQL string
		wantArg []any
	}{
		{name: "equals", filter: Filter{"is_active", "equals", true}, wantSQL: "is_active = ?", wantArg: []any{true}},
		{name: "not equals", filter: Filter{"status", "not_equals", "Inactive"}, wantSQL: "status != ?", wantArg: []any{"Inactive"}},
		{name: "greater than", filter: Filter{"stock_qty", "gt", float64(0)}, wantSQL: "stock_qty > ?", wantArg: []any{float64(0)}},
		{name: "greater than or equal", filter: Filter{"stock_qty", "gte", float64(10)}, wantSQL: "stock_qty >= ?", wantArg: []any{float64(10)}},
		{name: "less than", filter: Filter{"stock_qty", "lt", float64(5)}, wantSQL: "stock_qty < ?", wantArg: []any{float64(5)}},
		{name: "less than or equal", filter: Filter{"stock_qty", "lte", float64(20)}, wantSQL: "stock_qty <= ?", wantArg: []any{float64(20)}},
		{name: "contains", filter: Filter{"product_name", "contains", "Milk"}, wantSQL: "product_name LIKE ?", wantArg: []any{"%Milk%"}},
		{name: "starts with", filter: Filter{"product_name", "starts_with", "Fresh"}, wantSQL: "product_name LIKE ?", wantArg: []any{"Fresh%"}},
		{name: "ends with", filter: Filter{"product_name", "ends_with", "1kg"}, wantSQL: "product_name LIKE ?", wantArg: []any{"%1kg"}},
		{name: "not in", filter: Filter{"status", "not_in", []any{"Inactive", "Blocked"}}, wantSQL: "status NOT IN (?, ?)", wantArg: []any{"Inactive", "Blocked"}},
		{name: "is set", filter: Filter{"barcode", "is_set", nil}, wantSQL: "barcode IS NOT NULL", wantArg: nil},
		{name: "is not set", filter: Filter{"barcode", "is_not_set", nil}, wantSQL: "barcode IS NULL", wantArg: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &FilterSet{Filters: []Filter{tt.filter}}
			gotSQL, gotArgs, err := fs.ToSQL()
			if err != nil {
				t.Fatalf("ToSQL: %v", err)
			}
			if gotSQL != tt.wantSQL {
				t.Fatalf("SQL: got %q, want %q", gotSQL, tt.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArg) {
				t.Fatalf("args: got %#v, want %#v", gotArgs, tt.wantArg)
			}
		})
	}
}

func TestFilterSetToSQL_CombinesViewOperatorAliases(t *testing.T) {
	fs := &FilterSet{Filters: []Filter{
		{"is_active", "equals", true},
		{"category", "not_in", []any{"Toiletries", "Household"}},
	}}

	gotSQL, gotArgs, err := fs.ToSQL()
	if err != nil {
		t.Fatalf("ToSQL: %v", err)
	}
	wantSQL := "is_active = ? AND category NOT IN (?, ?)"
	if gotSQL != wantSQL {
		t.Fatalf("SQL: got %q, want %q", gotSQL, wantSQL)
	}
	wantArgs := []any{true, "Toiletries", "Household"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args: got %#v, want %#v", gotArgs, wantArgs)
	}
}

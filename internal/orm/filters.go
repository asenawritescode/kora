// Package orm — filter parsing utilities.
package orm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
)

// Filter represents a single filter condition: [field, operator, value].
type Filter [3]any

// FilterSet is a collection of filters applied together.
type FilterSet struct {
	Filters []Filter
}

// ParseFilters parses a JSON-encoded filter array.
// Format: [["field","=","value"],["field2",">",5]]
func ParseFilters(raw string) (*FilterSet, error) {
	if raw == "" || raw == "[]" {
		return &FilterSet{}, nil
	}

	var rawFilters [][]any
	if err := json.Unmarshal([]byte(raw), &rawFilters); err != nil {
		return nil, fmt.Errorf("parsing filters JSON: %w", err)
	}

	fs := &FilterSet{}
	for _, rf := range rawFilters {
		if len(rf) != 3 {
			return nil, fmt.Errorf("each filter must have exactly 3 elements [field, operator, value]")
		}
		fs.Filters = append(fs.Filters, Filter{rf[0], rf[1], rf[2]})
	}
	return fs, nil
}

// ToSQL converts the filter set to a SQL WHERE clause and arguments.
// Returns the WHERE clause string (without "WHERE") and the argument slice.
func (fs *FilterSet) ToSQL() (string, []any, error) {
	if len(fs.Filters) == 0 {
		return "1=1", nil, nil
	}

	var clauses []string
	var args []any

	for _, f := range fs.Filters {
		field, ok := f[0].(string)
		if !ok {
			return "", nil, fmt.Errorf("filter field must be a string")
		}

		op, ok := f[1].(string)
		if !ok {
			return "", nil, fmt.Errorf("filter operator must be a string")
		}

		value := f[2]

		clause, clauseArgs, err := buildClause(field, op, value)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}

	return strings.Join(clauses, " AND "), args, nil
}

// ValidateFields checks that all filter field names are valid columns in the DocType.
// This prevents SQL injection via user-supplied field names in filter clauses.
// System columns (name, owner, creation, modified, modified_by, doc_status, idx)
// and all data fields are considered valid.
func (fs *FilterSet) ValidateFields(dt *doctype.DocType) error {
	validCols := filterValidColumns(dt)
	for _, f := range fs.Filters {
		field, ok := f[0].(string)
		if !ok {
			return fmt.Errorf("filter field must be a string")
		}
		if !validCols[field] {
			return fmt.Errorf("unknown field %q in filter", field)
		}
	}
	return nil
}

// filterValidColumns returns the set of valid column names for filtering.
func filterValidColumns(dt *doctype.DocType) map[string]bool {
	cols := map[string]bool{
		"name":        true,
		"owner":       true,
		"creation":    true,
		"modified":    true,
		"modified_by": true,
		"doc_status":  true,
		"idx":         true,
	}
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			cols[f.Fieldname] = true
		}
	}
	return cols
}

func buildClause(field, op string, value any) (string, []any, error) {
	switch strings.ToLower(op) {
	case "=", "equals":
		return fmt.Sprintf("%s = ?", field), []any{value}, nil
	case "!=", "not_equals":
		return fmt.Sprintf("%s != ?", field), []any{value}, nil
	case ">", "gt":
		return fmt.Sprintf("%s > ?", field), []any{value}, nil
	case ">=", "gte":
		return fmt.Sprintf("%s >= ?", field), []any{value}, nil
	case "<", "lt":
		return fmt.Sprintf("%s < ?", field), []any{value}, nil
	case "<=", "lte":
		return fmt.Sprintf("%s <= ?", field), []any{value}, nil
	case "like":
		return fmt.Sprintf("%s LIKE ?", field), []any{value}, nil
	case "not like":
		return fmt.Sprintf("%s NOT LIKE ?", field), []any{value}, nil
	case "contains":
		return fmt.Sprintf("%s LIKE ?", field), []any{"%" + fmt.Sprint(value) + "%"}, nil
	case "starts_with":
		return fmt.Sprintf("%s LIKE ?", field), []any{fmt.Sprint(value) + "%"}, nil
	case "ends_with":
		return fmt.Sprintf("%s LIKE ?", field), []any{"%" + fmt.Sprint(value)}, nil
	case "in":
		return buildInClause(field, "IN", value)
	case "not in", "not_in":
		return buildInClause(field, "NOT IN", value)
	case "between":
		values, ok := value.([]any)
		if !ok || len(values) != 2 {
			return "", nil, fmt.Errorf("BETWEEN operator requires an array of exactly 2 values")
		}
		return fmt.Sprintf("%s BETWEEN ? AND ?", field), []any{values[0], values[1]}, nil
	case "is":
		if value == nil {
			return fmt.Sprintf("%s IS NULL", field), nil, nil
		}
		return "", nil, fmt.Errorf("IS operator only supports NULL")
	case "is not":
		if value == nil {
			return fmt.Sprintf("%s IS NOT NULL", field), nil, nil
		}
		return "", nil, fmt.Errorf("IS NOT operator only supports NULL")
	case "is_set":
		return fmt.Sprintf("%s IS NOT NULL", field), nil, nil
	case "is_not_set":
		return fmt.Sprintf("%s IS NULL", field), nil, nil
	default:
		return "", nil, fmt.Errorf("unknown operator: %s", op)
	}
}

func buildInClause(field, op string, value any) (string, []any, error) {
	values, ok := value.([]any)
	if !ok {
		return "", nil, fmt.Errorf("%s operator requires an array value", op)
	}
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}
	return fmt.Sprintf("%s %s (%s)", field, op, strings.Join(placeholders, ", ")), args, nil
}

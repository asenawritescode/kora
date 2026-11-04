package analytics

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func (qe *QueryEngine) resolveRelatedMeasure(registry *doctype.Registry, model *SemanticModel, query ModelQuery, queryTime QueryTime, measureName string, limit int) (*QueryResult, error) {
	if qe == nil || qe.DB == nil || qe.Dialect == nil || registry == nil || model == nil || model.Relation == nil {
		return nil, fmt.Errorf("related analytics query is not configured")
	}
	relation := model.Relation
	parent := registry.Get(relation.ParentDoctype)
	child := registry.Get(relation.ChildDoctype)
	if parent == nil || child == nil {
		return nil, fmt.Errorf("related analytics source DocType is unavailable")
	}
	fromDate, fromErr := time.Parse("2006-01-02", queryTime.From)
	toDate, toErr := time.Parse("2006-01-02", queryTime.To)
	if fromErr != nil || toErr != nil || queryTime.From > queryTime.To {
		return nil, fmt.Errorf("invalid related analytics date range")
	}
	if len(query.Dimensions) > 1 {
		return nil, fmt.Errorf("related model %q supports one grouping dimension per query", model.Name)
	}
	var measure *SemanticMeasure
	for i := range model.Measures {
		if model.Measures[i].Name == measureName {
			measure = &model.Measures[i]
			break
		}
	}
	if measure == nil {
		return nil, fmt.Errorf("unknown measure %q for model %q", measureName, model.Name)
	}
	q := qe.Dialect.QuoteIdent
	parentAlias, childAlias := "p", "c"
	qualified := func(alias, field string) string { return q(alias) + "." + q(field) }
	fieldSource := func(source, field string) (string, error) {
		alias := parentAlias
		if source == "child" {
			alias = childAlias
		}
		if source != "parent" && source != "child" {
			return "", fmt.Errorf("unsupported related field source %q", source)
		}
		return qualified(alias, field), nil
	}

	selectParts := make([]string, 0, 2)
	groupParts := make([]string, 0, 2)
	joins := make([]string, 0, 1)
	args := make([]any, 0, 10)
	dimensionName := ""
	if len(query.Dimensions) == 1 {
		dimensionName = query.Dimensions[0]
		var dimension *SemanticDimension
		for i := range model.Dimensions {
			if model.Dimensions[i].Name == dimensionName {
				dimension = &model.Dimensions[i]
				break
			}
		}
		if dimension == nil {
			return nil, fmt.Errorf("unknown dimension %q for model %q", dimensionName, model.Name)
		}
		dimensionExpr, err := fieldSource(dimension.Source, dimension.Field)
		if err != nil {
			return nil, err
		}
		if dimension.LinkDoctype != "" {
			linked := registry.Get(dimension.LinkDoctype)
			if linked == nil {
				return nil, fmt.Errorf("linked analytics DocType %q is unavailable", dimension.LinkDoctype)
			}
			joins = append(joins, " LEFT JOIN "+q(linked.RawTableName())+" l0 ON "+qualified("l0", "name")+" = "+dimensionExpr)
			dimensionExpr = "COALESCE(" + qualified("l0", dimension.LinkLabelField) + ", " + dimensionExpr + ")"
			groupParts = append(groupParts, fieldExprForSource(dimension.Source, dimension.Field, q), qualified("l0", "name"), qualified("l0", dimension.LinkLabelField))
		} else {
			groupParts = append(groupParts, dimensionExpr)
		}
		selectParts = append(selectParts, dimensionExpr+" AS "+q("dimension"))
	}

	measureExpr := "*"
	if measure.Field != "" {
		var err error
		measureExpr, err = fieldSource(measure.Source, measure.Field)
		if err != nil {
			return nil, err
		}
	}
	aggregate := strings.ToUpper(measure.Aggregation)
	if aggregate == "COUNT" {
		aggregate = "COUNT"
	}
	selectParts = append(selectParts, aggregate+"("+measureExpr+") AS "+q("value"))

	whereParts := []string{
		qualified(childAlias, "parent") + " = " + qualified(parentAlias, "name"),
		qualified(childAlias, "parenttype") + " = ?",
		qualified(childAlias, "parentfield") + " = ?",
		qualified(parentAlias, relation.ParentTimeField) + " >= ?",
		qualified(parentAlias, relation.ParentTimeField) + " < ?",
	}
	args = append(args, relation.ParentDoctype, relation.ChildTableField, fromDate.Format("2006-01-02"), toDate.AddDate(0, 0, 1).Format("2006-01-02"))

	for _, filter := range relation.Filters {
		expression, err := fieldSource(filter.Source, filter.Field)
		if err != nil {
			return nil, err
		}
		whereParts = append(whereParts, expression+" "+filter.Operator+" ?")
		args = append(args, filter.Value)
	}
	for _, filter := range query.Filters {
		var dimension *SemanticDimension
		for i := range model.Dimensions {
			if model.Dimensions[i].Name == filter.Field {
				dimension = &model.Dimensions[i]
				break
			}
		}
		if dimension == nil {
			return nil, fmt.Errorf("unknown filter dimension %q", filter.Field)
		}
		expression, err := fieldSource(dimension.Source, dimension.Field)
		if err != nil {
			return nil, err
		}
		op := strings.ToLower(strings.TrimSpace(filter.Operator))
		switch op {
		case "=", "==", "!=", "<>", "like", "not like":
			if len(filter.Values) != 1 {
				return nil, fmt.Errorf("filter %q requires one value", filter.Field)
			}
			if op == "==" {
				op = "="
			}
			whereParts = append(whereParts, expression+" "+strings.ToUpper(op)+" ?")
			args = append(args, filter.Values[0])
		case "in", "not in":
			if len(filter.Values) == 0 {
				return nil, fmt.Errorf("filter %q requires values", filter.Field)
			}
			placeholders := make([]string, len(filter.Values))
			for i, value := range filter.Values {
				placeholders[i] = "?"
				args = append(args, value)
			}
			whereParts = append(whereParts, expression+" "+strings.ToUpper(op)+" ("+strings.Join(placeholders, ",")+")")
		default:
			return nil, fmt.Errorf("unsupported filter operator %q", filter.Operator)
		}
	}

	querySQL := "SELECT " + strings.Join(selectParts, ", ") + " FROM " + q(parent.RawChildTableName(relation.ChildTableField)) + " " + q(childAlias) + " JOIN " + q(parent.RawTableName()) + " " + q(parentAlias) + " ON " + qualified(childAlias, "parent") + " = " + qualified(parentAlias, "name") + strings.Join(joins, "") + " WHERE " + strings.Join(whereParts, " AND ")
	if len(groupParts) > 0 {
		querySQL += " GROUP BY " + strings.Join(groupParts, ", ")
	}
	if limit <= 0 {
		limit = MaxSemanticResultRows
	}
	if limit > MaxSemanticResultRows {
		limit = MaxSemanticResultRows
	}
	if dimensionName != "" {
		querySQL += " ORDER BY " + q("dimension")
	}
	querySQL += " LIMIT ?"
	args = append(args, limit)

	rows, err := qe.DB.Query(kdb.Rebind(qe.Dialect, querySQL), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := &QueryResult{Metric: model.Name + "_" + measureName, Rows: make([]map[string]any, 0)}
	if dimensionName != "" {
		result.Columns = append(result.Columns, "dimension")
	}
	result.Columns = append(result.Columns, "value")
	for rows.Next() {
		var value float64
		row := map[string]any{"value": value}
		if dimensionName != "" {
			var dimension sql.NullString
			if err := rows.Scan(&dimension, &value); err != nil {
				return nil, err
			}
			if dimension.Valid {
				row["dimension"] = dimension.String
			} else {
				row["dimension"] = ""
			}
		} else if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		row["value"] = value
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result.Total = len(result.Rows)
	return result, nil
}

func fieldExprForSource(source, field string, quote func(string) string) string {
	alias := "p"
	if source == "child" {
		alias = "c"
	}
	return quote(alias) + "." + quote(field)
}

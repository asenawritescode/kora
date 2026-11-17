package analytics

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// liveQuery is one aggregate over a DocType table. kind names the key column
// it returns: "bucket", "dimension", or "" for a single total.
type liveQuery struct {
	statement, kind, dimension string
	args                       []any
}

// resolveLiveMeasure aggregates a measure with filters from its DocType table,
// because rollups keep one counter per dimension value and cannot apply them.
func (qe *QueryEngine) resolveLiveMeasure(call semanticCall, measure SemanticMeasure, groupBy string) (*QueryResult, error) {
	if call.registry == nil || call.model.TimeDimension == "" {
		return nil, fmt.Errorf("measure %q filters need the DocType registry and a time dimension", measure.Name)
	}
	source := call.registry.Get(call.model.SourceDoctype)
	if source == nil {
		return nil, fmt.Errorf("analytics source DocType %q is unavailable", call.model.SourceDoctype)
	}
	query, err := liveSQL(qe.Dialect, call, measure, groupBy, source)
	if err != nil {
		return nil, err
	}
	rows, err := qe.DB.Query(kdb.Rebind(qe.Dialect, query.statement), query.args...)
	if err != nil {
		return nil, fmt.Errorf("analytics live query: %w", err)
	}
	defer rows.Close()
	out, err := scanLiveRows(rows, query)
	if err != nil {
		return nil, err
	}
	return &QueryResult{Metric: call.model.Name + "_" + measure.Name, Rows: out, Total: len(out)}, nil
}

// liveSQL builds the aggregate. Identifiers come from the DocType or pass
// through the dialect; values are placeholders.
func liveSQL(d kdb.Dialect, call semanticCall, measure SemanticMeasure, groupBy string, source *doctype.DocType) (liveQuery, error) {
	q, model := d.QuoteIdent, call.model
	aggregate := "COUNT(*)"
	if measure.Aggregation != "count" {
		if field := source.GetField(measure.Field); field == nil || !field.IsNumeric() {
			return liveQuery{}, fmt.Errorf("measure %q requires a numeric field", measure.Name)
		}
		aggregate = strings.ToUpper(measure.Aggregation) + "(" + q(measure.Field) + ")"
	}
	where, args, err := liveWhere(d, call, measure, source)
	if err != nil {
		return liveQuery{}, err
	}
	query := liveQuery{args: args}
	columns, group, order := aggregate+" AS "+q("value"), "", ""
	switch {
	case groupBy != "":
		expr, err := analyticsPeriodExpression(d, semanticDimensionField(*model, model.TimeDimension), groupBy)
		if err != nil {
			return liveQuery{}, err
		}
		columns, group, order = expr+" AS "+q("bucket")+", "+columns, expr, q("bucket")+" ASC"
		query.kind = "bucket"
	case len(call.query.Dimensions) == 1:
		field := q(semanticDimensionField(*model, call.query.Dimensions[0]))
		columns, group, order = field+" AS "+q("dimension")+", "+columns, field, q("value")+" DESC, "+q("dimension")
		query.kind, query.dimension = "dimension", call.query.Dimensions[0]
	}
	query.statement = "SELECT " + columns + " FROM " + q(source.RawTableName()) + " WHERE " + where
	if group != "" {
		query.statement += " GROUP BY " + group + " ORDER BY " + order
	}
	limit := call.limit
	if limit <= 0 {
		limit = MaxSemanticResultRows
	}
	query.statement += " LIMIT ?"
	query.args = append(query.args, limit)
	return query, nil
}

// liveWhere joins the time window, the measure filters and the query filters on
// the grouping dimension, returning the predicate and its arguments.
func liveWhere(d kdb.Dialect, call semanticCall, measure SemanticMeasure, source *doctype.DocType) (string, []any, error) {
	q, model := d.QuoteIdent, call.model
	from, to, err := parseSemanticWindow(call.time.From, call.time.To)
	if err != nil {
		return "", nil, err
	}
	timeColumn := q(semanticDimensionField(*model, model.TimeDimension))
	where := []string{timeColumn + " >= ?", timeColumn + " < ?"}
	args := []any{from.Format(time.DateOnly), to.AddDate(0, 0, 1).Format(time.DateOnly)}
	scopeClause, scopeArgs, err := liveScopeClause(d, call.scope, source)
	if err != nil {
		return "", nil, err
	}
	if scopeClause != "" {
		where, args = append(where, scopeClause), append(args, scopeArgs...)
	}
	for _, filter := range measure.Filters {
		if source.GetField(filter.Field) == nil || !validSemanticFilterOperator(filter.Operator) || len(filter.Values) == 0 {
			return "", nil, fmt.Errorf("measure %q has an invalid filter on %q", measure.Name, filter.Field)
		}
		clause, values := semanticFilterSQL(d, filter.Field, "", filter)
		where, args = append(where, clause), append(args, values...)
	}
	for _, filter := range call.query.Filters {
		if len(call.query.Dimensions) != 1 || filter.Field != call.query.Dimensions[0] {
			return "", nil, fmt.Errorf("filter %q must target the grouping dimension", filter.Field)
		}
		clause, values := semanticFilterSQL(d, semanticDimensionField(*model, filter.Field), "", filter)
		where, args = append(where, clause), append(args, values...)
	}
	return strings.Join(where, " AND "), args, nil
}

// liveScopeClause restricts the aggregate to the caller's business scope the way
// rollup rows are keyed: entity-owned sources store legal_entity rows only, and
// other sources store tenant rows only. A caller whose scope kind does not match
// gets no rows, as from rollups. Owner and assignment row scope is not applied
// here, and rollups do not apply it either, so the two paths stay at parity.
func liveScopeClause(d kdb.Dialect, scope contract.BusinessScope, source *doctype.DocType) (string, []any, error) {
	entityCaller := scope.Kind == contract.ScopeKindLegalEntity
	if source.BusinessScopePolicy != contract.ScopeEntityOwned {
		if entityCaller {
			return "1 = 0", nil, nil
		}
		return "", nil, nil
	}
	if source.BusinessScopeField == "" {
		return "", nil, fmt.Errorf("entity-owned analytics source %q has no legal-entity scope field", source.Name)
	}
	if !entityCaller || scope.LegalEntityID == "" {
		return "1 = 0", nil, nil
	}
	return d.QuoteIdent(source.BusinessScopeField) + " = ?", []any{scope.LegalEntityID}, nil
}

// callScope is the caller's business scope. An engine without one is
// tenant-wide, as rollup queries are.
func (qe *QueryEngine) callScope() contract.BusinessScope {
	if qe.BusinessScope.TenantID != "" {
		return qe.BusinessScope
	}
	return contract.BusinessScope{TenantID: contract.TenantID(qe.SiteName), Kind: contract.ScopeKindTenant}
}

// scanLiveRows gives rows the rollup shape: "bucket", or "field=value".
func scanLiveRows(rows *sql.Rows, query liveQuery) ([]map[string]any, error) {
	out := []map[string]any{}
	for rows.Next() {
		var key sql.NullString
		var value sql.NullFloat64
		dest := []any{&key, &value}
		if query.kind == "" {
			dest = []any{&value}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan live analytics row: %w", err)
		}
		row := map[string]any{"value": value.Float64}
		switch query.kind {
		case "bucket":
			row["bucket"] = key.String
		case "dimension":
			row["dimension"] = query.dimension + "=" + key.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

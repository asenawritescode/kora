package metrics

import (
	"fmt"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
)

// Table name prefixes of a fact model: _kora_fact_<model> and _kora_fact_summary_<model>.
const (
	factTablePrefix    = "_kora_fact_"
	summaryTablePrefix = "_kora_fact_summary_"
)

// build renders the plan as one statement and returns its arguments in placeholder order.
// Every placeholder is "?"; db.Rebind converts them for the dialect.
func (p plan) build(d db.Dialect) (string, []any) {
	if p.summary {
		return p.summarySQL(d)
	}
	return p.factsSQL(d)
}

// summarySQL reads one row per business day and group from the daily summary.
func (p plan) summarySQL(d db.Dialect) (string, []any) {
	q := d.QuoteIdent
	var args []any
	sel := []string{q("business_date")}
	group := []string{q("business_date")}
	grain := ""
	if len(p.dims) == 1 {
		grain = p.dims[0]
		sel = append(sel, q("grain_value"))
		group = append(group, q("grain_value"))
	}
	for _, s := range p.series {
		value := q("row_count")
		if s.agg == aggSum {
			value = q(s.column)
		}
		sel = append(sel, "SUM("+p.withConds(d, "", s.conds, value, &args)+")")
	}
	args = append(args, grain)
	where := append([]string{q("grain") + " = ?"}, p.rangeConds(d, "", &args)...)
	where = append(where, p.scopeConds(d, "", false, &args)...)
	where = append(where, p.filterConds(d, "", &args)...)
	stmt := fmt.Sprintf("SELECT %s FROM %s WHERE %s GROUP BY %s",
		strings.Join(sel, ", "), q(p.summaryTable()), strings.Join(where, " AND "), strings.Join(group, ", "))
	return stmt, args
}

// factsSQL aggregates the fact rows, header and line, per day and group. Measures sum over every row,
// as the summary does. A count takes header rows only, so a record with lines counts once.
func (p plan) factsSQL(d db.Dialect) (string, []any) {
	q := d.QuoteIdent
	var args []any
	const f = "f."
	sel := []string{f + q("business_date")}
	group := []string{f + q("business_date")}
	for _, dim := range p.dims {
		sel = append(sel, f+q(dim))
		group = append(group, f+q(dim))
	}
	for _, s := range p.series {
		sel = append(sel, p.factsAggregate(d, f, s, &args))
	}
	parts := []string{p.factsPart(d, p.model.Facts.TableName(p.model), &args)}
	if p.model.Facts.Lines != "" {
		parts = append(parts, p.factsPart(d, p.model.Facts.LineTableName(p.model), &args))
	}
	stmt := fmt.Sprintf("SELECT %s FROM (%s) AS f GROUP BY %s",
		strings.Join(sel, ", "), strings.Join(parts, " UNION ALL "), strings.Join(group, ", "))
	return stmt, args
}

// factsAggregate renders one series over the derived table alias f.
func (p plan) factsAggregate(d db.Dialect, f string, s series, args *[]any) string {
	switch s.agg {
	case aggCount:
		header := append([]predicate{{field: "line_id", op: "=", value: ""}}, s.conds...)
		return "SUM(" + p.caseWhen(d, f, header, "1", args) + ")"
	case aggSum:
		return "SUM(" + p.withConds(d, f, s.conds, f+d.QuoteIdent(s.column), args) + ")"
	}
	// count_distinct: an empty value is not a value, so it is not counted.
	conds := append([]predicate{{field: s.column, op: "!=", value: ""}}, s.conds...)
	// NULL, not 0, is the else branch: Postgres rejects a CASE that mixes a text value with an integer.
	return "COUNT(DISTINCT CASE WHEN " + p.condition(d, f, conds, args) + " THEN " + f + d.QuoteIdent(s.column) + " ELSE NULL END)"
}

// factsPart selects the rows of one fact table that fall in the query's scope and range.
func (p plan) factsPart(d db.Dialect, table string, args *[]any) string {
	q := d.QuoteIdent
	cols := []string{"business_date", "line_id", "status"}
	add := func(name string) {
		if !slices.Contains(cols, name) {
			cols = append(cols, name)
		}
	}
	for _, dim := range p.dims {
		add(dim)
	}
	for _, s := range p.series {
		if s.column != "" && s.agg != aggCount {
			add(s.column)
		}
		for _, c := range s.conds {
			add(p.columnName(c.field))
		}
	}
	for _, c := range p.where {
		add(p.columnName(c.field))
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = q(c)
	}
	where := append(p.rangeConds(d, "", args), q("deleted")+" = ?")
	*args = append(*args, db.BoolParam(d, false))
	where = append(where, p.scopeConds(d, "", true, args)...)
	where = append(where, p.filterConds(d, "", args)...)
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(quoted, ", "), q(table), strings.Join(where, " AND "))
}

// rangeConds bounds business_date to From through To, both inclusive. An opening reads every day
// before From instead; its one bucket is the balance a range starts from.
func (p plan) rangeConds(d db.Dialect, prefix string, args *[]any) []string {
	col := prefix + d.QuoteIdent("business_date")
	if p.opening {
		*args = append(*args, p.from.String())
		return []string{col + " < ?"}
	}
	*args = append(*args, p.from.String(), p.to.String())
	return []string{col + " >= ?", col + " <= ?"}
}

// scopeConds are the caller's scope as conditions. Owner exists only on the fact tables.
func (p plan) scopeConds(d db.Dialect, prefix string, facts bool, args *[]any) []string {
	var conds []string
	if p.scope.LegalEntity != "" {
		conds = append(conds, prefix+d.QuoteIdent("legal_entity")+" = ?")
		*args = append(*args, p.scope.LegalEntity)
	}
	if facts && p.scope.Owner != "" {
		conds = append(conds, prefix+d.QuoteIdent("owner")+" = ?")
		*args = append(*args, p.scope.Owner)
	}
	return conds
}

// filterConds are the query's own filters.
func (p plan) filterConds(d db.Dialect, prefix string, args *[]any) []string {
	conds := make([]string, 0, len(p.where))
	for _, c := range p.where {
		conds = append(conds, p.predicateSQL(d, prefix, c, args))
	}
	return conds
}

// withConds renders value, or CASE WHEN conds THEN value ELSE 0 END when there are conds.
func (p plan) withConds(d db.Dialect, prefix string, conds []predicate, value string, args *[]any) string {
	if len(conds) == 0 {
		return value
	}
	return p.caseWhen(d, prefix, conds, value, args)
}

// caseWhen renders CASE WHEN conds THEN value ELSE 0 END, appending the conditions' arguments.
func (p plan) caseWhen(d db.Dialect, prefix string, conds []predicate, value string, args *[]any) string {
	return "CASE WHEN " + p.condition(d, prefix, conds, args) + " THEN " + value + " ELSE 0 END"
}

// condition renders conds joined with AND, appending their arguments.
func (p plan) condition(d db.Dialect, prefix string, conds []predicate, args *[]any) string {
	parts := make([]string, len(conds))
	for i, c := range conds {
		parts[i] = p.predicateSQL(d, prefix, c, args)
	}
	return strings.Join(parts, " AND ")
}

// predicateSQL renders one comparison. The operator was checked against a whitelist when planned.
func (p plan) predicateSQL(d db.Dialect, prefix string, c predicate, args *[]any) string {
	col := prefix + d.QuoteIdent(p.column(c.field))
	if c.op == "in" {
		list, _ := c.value.([]any)
		*args = append(*args, list...)
		return col + " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(list)), ", ") + ")"
	}
	*args = append(*args, c.value)
	return col + " " + c.op + " ?"
}

// columnName is the fact column a filter field reads: the status column, or the dimension itself.
func (p plan) columnName(field string) string {
	if field == p.model.Facts.StatusField {
		return "status"
	}
	return field
}

// column is columnName on the chosen source. The summary keeps its one dimension in grain_value.
func (p plan) column(field string) string {
	if p.summary && field != p.model.Facts.StatusField {
		return "grain_value"
	}
	return p.columnName(field)
}

// summaryTable is _kora_fact_summary_<model>, named as the facts package names it.
func (p plan) summaryTable() string {
	return summaryTablePrefix + strings.TrimPrefix(p.model.Facts.TableName(p.model), factTablePrefix)
}

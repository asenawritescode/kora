package facts

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// summaryTable is the daily cache of one model: _kora_fact_summary_<model>.
func summaryTable(dt *doctype.DocType) string {
	return "_kora_fact_summary_" + strings.TrimPrefix(dt.Facts.TableName(dt), "_kora_fact_")
}

// summaryDDL creates the summary table. Its key is (business_date, legal_entity, grain, grain_value):
// grain "" with value "" is the day total, and each declared dimension adds one grain per value.
func summaryDDL(dialect db.Dialect, dt *doctype.DocType) string {
	q := dialect.QuoteIdent
	defs := []string{
		columnDef(dialect, "business_date", "Date"), columnDef(dialect, "legal_entity", "Data"),
		columnDef(dialect, "grain", "Data"), columnDef(dialect, "grain_value", "Data"), columnDef(dialect, "row_count", "Int"),
	}
	for _, m := range dt.Facts.Measures {
		defs = append(defs, columnDef(dialect, m.ColumnName(), "Currency"))
	}
	defs = append(defs, fmt.Sprintf("PRIMARY KEY (%s, %s, %s, %s)", q("business_date"), q("legal_entity"), q("grain"), q("grain_value")))
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", q(summaryTable(dt)), strings.Join(defs, ",\n  "))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return ddl
}

// rebuildInsert aggregates one business day from the fact tables into summary rows: the day total
// and one grain per dimension. row_count counts header rows (line_id is empty), so a record with
// lines counts once. Tombstones (deleted) are excluded. Args bind the day and the deleted flag per part.
func rebuildInsert(dialect db.Dialect, dt *doctype.DocType, day tenanttime.Date) (string, []any) {
	f := dt.Facts
	q := dialect.QuoteIdent
	inner := []string{"business_date", "legal_entity", "line_id"}
	for _, d := range f.Dimensions {
		inner = append(inner, d.ColumnName())
	}
	for _, m := range f.Measures {
		inner = append(inner, m.ColumnName())
	}
	part := func(table string) string {
		return fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? AND %s = ?", quoteList(dialect, inner), q(table), q("business_date"), q("deleted"))
	}
	source := part(f.TableName(dt))
	partArgs := []any{day.String(), db.BoolParam(dialect, false)}
	if f.Lines != "" {
		source += " UNION ALL " + part(f.LineTableName(dt))
		partArgs = append(partArgs, day.String(), db.BoolParam(dialect, false))
	}
	// grain "" is the day total; each dimension is its own grain.
	grains := append([]string{""}, inner[3:len(inner)-len(f.Measures)]...)
	sums := []string{"SUM(CASE WHEN f.line_id = '' THEN 1 ELSE 0 END)"}
	for _, m := range f.Measures {
		sums = append(sums, "SUM(f."+q(m.ColumnName())+")")
	}
	selects := make([]string, 0, len(grains))
	var args []any
	for _, g := range grains {
		value, group := "''", q("business_date")+", "+q("legal_entity")
		if g != "" {
			value, group = "f."+q(g), group+", f."+q(g)
		}
		selects = append(selects, fmt.Sprintf("SELECT f.%s, f.%s, '%s', %s, %s FROM (%s) AS f GROUP BY %s",
			q("business_date"), q("legal_entity"), g, value, strings.Join(sums, ", "), source, group))
		args = append(args, partArgs...)
	}
	cols := []string{"business_date", "legal_entity", "grain", "grain_value", "row_count"}
	for _, m := range f.Measures {
		cols = append(cols, m.ColumnName())
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) %s", q(summaryTable(dt)), quoteList(dialect, cols), strings.Join(selects, " UNION ALL "))
	return insert, args
}

// quoteList quotes each name for the dialect and joins them with commas.
func quoteList(dialect db.Dialect, names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = dialect.QuoteIdent(name)
	}
	return strings.Join(quoted, ", ")
}

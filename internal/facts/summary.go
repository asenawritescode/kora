package facts

import (
	"context"
	"errors"
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

// summaryDDL creates the summary table. Its key is (business_date, legal_entity, status, grain, grain_value):
// grain "" with value "" is the day total for one status, and each declared dimension adds one grain per value.
func summaryDDL(dialect db.Dialect, dt *doctype.DocType) string {
	q := dialect.QuoteIdent
	defs := []string{
		columnDef(dialect, "business_date", "Date"), columnDef(dialect, "legal_entity", "Data"), columnDef(dialect, "status", "Data"),
		columnDef(dialect, "grain", "Data"), columnDef(dialect, "grain_value", "Data"), columnDef(dialect, "row_count", "Int"),
	}
	for _, m := range dt.Facts.Measures {
		defs = append(defs, columnDef(dialect, m.ColumnName(), "Currency"))
	}
	defs = append(defs, fmt.Sprintf("PRIMARY KEY (%s, %s, %s, %s, %s)", q("business_date"), q("legal_entity"), q("status"), q("grain"), q("grain_value")))
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", q(summaryTable(dt)), strings.Join(defs, ",\n  "))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return ddl
}

// summaryColumns are the summary table's columns in the order rebuildSelect returns them: the key, then
// row_count, then each measure.
func summaryColumns(dt *doctype.DocType) []string {
	cols := []string{"business_date", "legal_entity", "status", "grain", "grain_value", "row_count"}
	for _, m := range dt.Facts.Measures {
		cols = append(cols, m.ColumnName())
	}
	return cols
}

// rebuildSelect aggregates one business day from the fact tables, per status: the day total and one grain
// per dimension, with the columns named as summaryColumns. row_count counts header rows (line_id is empty),
// so a record with lines counts once. Tombstones (deleted) are excluded. Args bind the day and the deleted
// flag per part.
func rebuildSelect(dialect db.Dialect, dt *doctype.DocType, day tenanttime.Date) (string, []any) {
	f := dt.Facts
	q := dialect.QuoteIdent
	dims := make([]string, len(f.Dimensions))
	for i, d := range f.Dimensions {
		dims[i] = d.ColumnName()
	}
	inner := append([]string{"business_date", "legal_entity", "status", "line_id"}, dims...)
	for _, m := range f.Measures {
		inner = append(inner, m.ColumnName())
	}
	// A line table part keeps only rows with a line id, so a line row with an empty id never counts as a header.
	part := func(table string, lines bool) string {
		where := fmt.Sprintf("%s = ? AND %s = ?", q("business_date"), q("deleted"))
		if lines {
			where += " AND " + q("line_id") + " <> ''"
		}
		return fmt.Sprintf("SELECT %s FROM %s WHERE %s", quoteList(dialect, inner), q(table), where)
	}
	source := part(f.TableName(dt), false)
	partArgs := []any{day.String(), db.BoolParam(dialect, false)}
	if f.Lines != "" {
		source += " UNION ALL " + part(f.LineTableName(dt), true)
		partArgs = append(partArgs, day.String(), db.BoolParam(dialect, false))
	}
	// grain "" is the day total; each dimension is its own grain.
	grains := append([]string{""}, dims...)
	sums := []string{"SUM(CASE WHEN f.line_id = '' THEN 1 ELSE 0 END) AS " + q("row_count")}
	for _, m := range f.Measures {
		sums = append(sums, "SUM(f."+q(m.ColumnName())+") AS "+q(m.ColumnName()))
	}
	selects := make([]string, 0, len(grains))
	var args []any
	for _, g := range grains {
		value, group := "''", q("business_date")+", "+q("legal_entity")+", "+q("status")
		if g != "" {
			value, group = "f."+q(g), group+", f."+q(g)
		}
		selects = append(selects, fmt.Sprintf("SELECT f.%s, f.%s, f.%s, '%s' AS %s, %s AS %s, %s FROM (%s) AS f GROUP BY %s",
			q("business_date"), q("legal_entity"), q("status"), g, q("grain"), value, q("grain_value"), strings.Join(sums, ", "), source, group))
		args = append(args, partArgs...)
	}
	return strings.Join(selects, " UNION ALL "), args
}

// rebuildInsert writes one business day's aggregate into the summary table.
func rebuildInsert(dialect db.Dialect, dt *doctype.DocType, day tenanttime.Date) (string, []any) {
	selectSQL, args := rebuildSelect(dialect, dt, day)
	return fmt.Sprintf("INSERT INTO %s (%s) %s", dialect.QuoteIdent(summaryTable(dt)), quoteList(dialect, summaryColumns(dt)), selectSQL), args
}

// migrateSummary replaces a summary table that predates the status column. Summaries are caches, so the
// table is dropped and recreated, and every business date with facts is marked dirty so the refresher
// fills it again. A table that already has the column is left alone.
func (b *Builder) migrateSummary(ctx context.Context, dt *doctype.DocType) error {
	q := b.Dialect.QuoteIdent
	probe := fmt.Sprintf("SELECT %s FROM %s WHERE 1 = 0", q("status"), q(summaryTable(dt))) //nolint:gosec // identifiers only, quoted by the dialect
	if _, err := b.DB.ExecContext(ctx, probe); err == nil {
		return nil
	}
	if err := b.execDDL(ctx, []string{"DROP TABLE IF EXISTS " + q(summaryTable(dt))}); err != nil {
		return err
	}
	if err := b.execDDL(ctx, []string{summaryDDL(b.Dialect, dt)}); err != nil {
		return err
	}
	inc, ok := b.Dialect.(db.TableQualifiedIncrementDialect)
	if !ok {
		return errors.New("facts: dialect has no table-qualified increment")
	}
	mark := fmt.Sprintf("INSERT INTO %s (model, business_date, version) SELECT DISTINCT ?, business_date, 1 FROM %s %s",
		q(dirtyTable), q(dt.Facts.TableName(dt)), inc.UpsertIncrementForTable(dirtyTable, []string{"model", "business_date"}, []string{"version"}))
	if _, err := b.DB.ExecContext(ctx, db.Rebind(b.Dialect, mark), dt.Name); err != nil {
		return fmt.Errorf("facts: mark summary days dirty: %w", err)
	}
	return nil
}

// quoteList quotes each name for the dialect and joins them with commas.
func quoteList(dialect db.Dialect, names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = dialect.QuoteIdent(name)
	}
	return strings.Join(quoted, ", ")
}

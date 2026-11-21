// Package facts maps record changes to fact rows: one header row per record and,
// when FactModel.Lines is set, one row per child line. Rows are keyed by
// (record_id, line_id); the header uses line_id ”. The consumer that writes them
// lives elsewhere; this package only builds DDL and the pure row mapping.
package facts

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// fixedColumns are the columns every fact table carries, in order.
var fixedColumns = [...]struct{ name, fieldtype string }{
	{"record_id", "Data"}, {"line_id", "Data"}, {"legal_entity", "Data"}, {"owner", "Data"},
	{"business_date", "Date"}, {"occurred_at", "Datetime"}, {"status", "Data"},
	{"source_sequence", "Int"}, {"deleted", "Check"},
}

// DDL returns the statements creating dt's fact and line tables. Run once: the index is not IF NOT EXISTS.
func DDL(dialect db.Dialect, dt *doctype.DocType) []string {
	f := dt.Facts
	if f == nil {
		return nil
	}
	stmts := createTable(dialect, f.TableName(dt), f.Columns())
	if f.Lines != "" {
		stmts = append(stmts, createTable(dialect, f.LineTableName(dt), f.Columns())...)
	}
	return stmts
}

// createTable builds the table, its primary key columns and its business_date index.
func createTable(dialect db.Dialect, table string, cols []doctype.FactColumn) []string {
	defs := make([]string, 0, len(fixedColumns)+len(cols)+1)
	for _, c := range fixedColumns {
		defs = append(defs, columnDef(dialect, c.name, c.fieldtype))
	}
	for _, c := range cols {
		fieldtype := "Currency"
		if c.Type == doctype.FactColumnText {
			fieldtype = "Data"
		}
		defs = append(defs, columnDef(dialect, c.Name, fieldtype))
	}
	defs = append(defs, fmt.Sprintf("PRIMARY KEY (%s, %s)", dialect.QuoteIdent("record_id"), dialect.QuoteIdent("line_id")))
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)",
		dialect.QuoteIdent(table), strings.Join(defs, ",\n  "))
	if suffix := dialect.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	return []string{ddl, dialect.CreateIndex(table, "business_date", false), dialect.CreateIndex(table, sequenceColumn, false)}
}

// sequenceColumn is the fact column whose index lets the metrics freshness query read the newest applied
// sequence without scanning the table.
const sequenceColumn = "source_sequence"

// columnDef is one NOT NULL column whose SQL type comes from the dialect for fieldtype.
func columnDef(dialect db.Dialect, name, fieldtype string) string {
	return fmt.Sprintf("%s %s NOT NULL", dialect.QuoteIdent(name), dialect.ColumnType(&doctype.Field{Fieldtype: fieldtype}))
}

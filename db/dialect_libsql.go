package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/doctype"
)

// LibSQLDialect implements Dialect for LibSQL (Turso's SQLite fork).
// LibSQL is deployed as a managed database in Dokploy — Kora connects
// to it remotely via HTTP. This adapter handles SQL dialect differences:
// PRAGMA instead of INFORMATION_SCHEMA, SQLite types, and constraint errors.
type LibSQLDialect struct{}

func (d *LibSQLDialect) DriverName() string { return "libsql" }

func (d *LibSQLDialect) Open(cfg DBConfig) (*sql.DB, error) {
	// LibSQL connection via the go-libsql driver.
	// For remote LibSQL (Turso/Dokploy), use the URL form.
	// For local, use "file:/path/to/db?mode=rwc".
	dsn := cfg.URL
	if dsn == "" {
		dsn = fmt.Sprintf("file:%s?mode=rwc", cfg.Name)
	}
	db, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening libsql: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging libsql: %w", err)
	}
	return db, nil
}

// ---------------------------------------------------------------------------
// Schema Introspection (PRAGMA-based — no INFORMATION_SCHEMA)
// ---------------------------------------------------------------------------

func (d *LibSQLDialect) LoadSchema(db *sql.DB, _ string) (*LiveSchema, error) {
	schema := &LiveSchema{Tables: make(map[string]*LiveTable)}

	// Get all tables matching the Kora prefix.
	tableRows, err := db.Query("SELECT name FROM pragma_table_list WHERE name LIKE 'tab%'")
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	defer tableRows.Close()

	var tableNames []string
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			return nil, err
		}
		tableNames = append(tableNames, name)
	}
	if err := tableRows.Err(); err != nil {
		return nil, err
	}

	for _, tableName := range tableNames {
		table := &LiveTable{Name: tableName}

		// Read columns via PRAGMA table_info.
		colRows, err := db.Query(fmt.Sprintf("PRAGMA table_info('%s')", tableName))
		if err != nil {
			return nil, fmt.Errorf("reading columns for %s: %w", tableName, err)
		}
		for colRows.Next() {
			var cid int
			var colName, colType string
			var notNull, pk int
			var dfltValue sql.NullString
			if err := colRows.Scan(&cid, &colName, &colType, &notNull, &dfltValue, &pk); err != nil {
				colRows.Close()
				return nil, err
			}
			table.Columns = append(table.Columns, LiveColumn{
				Name:         colName,
				Type:         colType,
				IsNullable:   notNull == 0,
				DefaultValue: dfltValue.String,
			})
		}
		colRows.Close()
		if err := colRows.Err(); err != nil {
			return nil, err
		}

		// Read indexes via PRAGMA index_list.
		idxRows, err := db.Query(fmt.Sprintf("PRAGMA index_list('%s')", tableName))
		if err != nil {
			return nil, fmt.Errorf("reading indexes for %s: %w", tableName, err)
		}
		for idxRows.Next() {
			var seq, unique int
			var idxName, origin string
			var partial int
			if err := idxRows.Scan(&seq, &idxName, &unique, &origin, &partial); err != nil {
				idxRows.Close()
				return nil, err
			}
			// Get columns in this index via PRAGMA index_info.
			infoRows, err := db.Query(fmt.Sprintf("PRAGMA index_info('%s')", idxName))
			if err != nil {
				idxRows.Close()
				return nil, err
			}
			var columns []string
			for infoRows.Next() {
				var seqno, cid int
				var colName string
				if err := infoRows.Scan(&seqno, &cid, &colName); err != nil {
					infoRows.Close()
					idxRows.Close()
					return nil, err
				}
				columns = append(columns, colName)
			}
			infoRows.Close()

			table.Indexes = append(table.Indexes, LiveIndex{
				Name:     idxName,
				Columns:  columns,
				IsUnique: unique == 1,
			})
		}
		idxRows.Close()
		if err := idxRows.Err(); err != nil {
			return nil, err
		}

		schema.Tables[tableName] = table
	}

	return schema, nil
}

// ---------------------------------------------------------------------------
// DDL Generation (SQLite-compatible)
// ---------------------------------------------------------------------------

func (d *LibSQLDialect) CreateTable(dt *doctype.DocType) string {
	var cols []string

	// System columns — no ENGINE, no CHARSET, no COLLATE.
	cols = append(cols,
		`"name" TEXT NOT NULL`,
		`"owner" TEXT NOT NULL DEFAULT ''`,
		`"creation" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`,
		`"modified" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`,
		`"modified_by" TEXT NOT NULL DEFAULT ''`,
		`"doc_status" INTEGER NOT NULL DEFAULT 0`,
		`"idx" INTEGER NOT NULL DEFAULT 0`,
	)

	// Data columns.
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		col := fmt.Sprintf("%s %s", d.QuoteIdent(f.Fieldname), d.ColumnType(&f))
		if f.Reqd {
			col += " NOT NULL"
		}
		if f.Default != "" && f.Fieldtype != "Check" {
			col += fmt.Sprintf(" DEFAULT '%s'", f.Default)
		}
		cols = append(cols, col)
	}

	// Primary key.
	cols = append(cols, `PRIMARY KEY ("name")`)

	tableName := d.QuoteIdent(dt.TableName())
	ddl := fmt.Sprintf("CREATE TABLE %s (\n  %s\n)", tableName, strings.Join(cols, ",\n  "))

	// Search indexes.
	for _, f := range dt.DataFields() {
		if f.SearchIndex {
			ddl += "\n" + d.CreateIndex(dt.TableName(), f.Fieldname, false)
		}
	}

	// UNIQUE indexes.
	for _, f := range dt.DataFields() {
		if f.Unique {
			ddl += "\n" + d.CreateIndex(dt.TableName(), f.Fieldname, true)
		}
	}

	// ON UPDATE trigger for the modified column (SQLite has no ON UPDATE attribute).
	triggerName := d.QuoteIdent(dt.TableName() + "_modified_on_update")
	ddl += fmt.Sprintf("\nCREATE TRIGGER %s AFTER UPDATE ON %s FOR EACH ROW BEGIN UPDATE %s SET \"modified\" = STRFTIME('%%Y-%%m-%%d %%H:%%M:%%f', 'NOW') WHERE \"name\" = NEW.\"name\"; END",
		triggerName, tableName, tableName)

	return ddl
}

func (d *LibSQLDialect) AddColumn(tableName string, f *doctype.Field) string {
	col := fmt.Sprintf("%s %s", d.QuoteIdent(f.Fieldname), d.ColumnType(f))
	if f.Reqd {
		col += " NOT NULL"
	}
	if f.Default != "" && f.Fieldtype != "Check" {
		col += fmt.Sprintf(" DEFAULT '%s'", f.Default)
	}
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", d.QuoteIdent(tableName), col)
}

func (d *LibSQLDialect) AlterColumn(tableName string, f *doctype.Field) string {
	// SQLite does not support ALTER COLUMN. The migration system
	// should use the rebuild strategy for type changes.
	return fmt.Sprintf("-- LibSQL: ALTER COLUMN not supported for %s.%s (requires table rebuild)",
		tableName, f.Fieldname)
}

func (d *LibSQLDialect) RenameColumn(tableName, oldName, newName string) string {
	return fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
		d.QuoteIdent(tableName), d.QuoteIdent(oldName), d.QuoteIdent(newName))
}

func (d *LibSQLDialect) CreateIndex(tableName, fieldName string, unique bool) string {
	uq := ""
	if unique {
		uq = "UNIQUE "
	}
	indexName := fmt.Sprintf("idx_%s", fieldName)
	if unique {
		indexName = fmt.Sprintf("uq_%s", fieldName)
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)",
		uq, d.QuoteIdent(indexName), d.QuoteIdent(tableName), d.QuoteIdent(fieldName))
}

func (d *LibSQLDialect) DropIndex(tableName, indexName string) string {
	return fmt.Sprintf("DROP INDEX %s", d.QuoteIdent(indexName))
}

func (d *LibSQLDialect) DropColumn(tableName, columnName string) string {
	// SQLite 3.35+ supports DROP COLUMN.
	return fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", d.QuoteIdent(tableName), d.QuoteIdent(columnName))
}

func (d *LibSQLDialect) QuoteIdent(name string) string {
	return `"` + name + `"`
}

func (d *LibSQLDialect) ColumnType(f *doctype.Field) string {
	switch f.Fieldtype {
	case "Data", "Select", "Link", "Dynamic Link", "Password":
		return "TEXT"
	case "Text", "Text Editor":
		return "TEXT"
	case "Int":
		return "INTEGER"
	case "Float", "Currency", "Percent":
		return "REAL" // No DECIMAL precision enforcement — use INTEGER cents for money
	case "Check":
		return "INTEGER"
	case "Date", "Time", "Datetime":
		return "TEXT" // Stored as ISO 8601 strings
	case "Attach", "Attach Image":
		return "TEXT"
	case "JSON":
		return "TEXT" // SQLite stores JSON as TEXT
	default:
		return "TEXT"
	}
}

func (d *LibSQLDialect) NowTimestamp() string {
	return `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`
}

func (d *LibSQLDialect) ParseError(err error, dt *doctype.DocType) *doctype.ValidationError {
	if err == nil {
		return nil
	}
	msg := err.Error()

	// SQLite constraint violation messages:
	// "UNIQUE constraint failed: table.column"
	// "NOT NULL constraint failed: table.column"
	if strings.Contains(msg, "UNIQUE constraint failed") {
		fieldName := parseLibSQLConstraintField(msg, "UNIQUE constraint failed")
		if fieldName != "" {
			label := fieldName
			if f := dt.GetField(fieldName); f != nil {
				label = f.Label
			}
			return &doctype.ValidationError{
				Type:    "UniqueConstraint",
				Message: fmt.Sprintf("%s must be unique.", label),
				Field:   fieldName,
				DocType: dt.Name,
			}
		}
	}
	if strings.Contains(msg, "NOT NULL constraint failed") {
		fieldName := parseLibSQLConstraintField(msg, "NOT NULL constraint failed")
		if fieldName != "" {
			label := fieldName
			if f := dt.GetField(fieldName); f != nil {
				label = f.Label
			}
			return &doctype.ValidationError{
				Type:    "NotNullConstraint",
				Message: fmt.Sprintf("%s is required.", label),
				Field:   fieldName,
				DocType: dt.Name,
			}
		}
	}
	return nil
}

func (d *LibSQLDialect) Placeholder(n int) string { return "?" }

// parseLibSQLConstraintField extracts the column name from SQLite constraint error messages.
// Format: "UNIQUE constraint failed: table_name.column_name"
func parseLibSQLConstraintField(msg, prefix string) string {
	idx := strings.Index(msg, prefix+": ")
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(prefix)+2:]
	// The format is "table.column" — extract the column part.
	dot := strings.IndexByte(rest, '.')
	if dot < 0 {
		return rest
	}
	return rest[dot+1:]
}

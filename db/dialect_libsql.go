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

func (d *LibSQLDialect) CreateTable(dt *doctype.DocType) []string {
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

	tableName := d.QuoteIdent(dt.RawTableName())
	ddl := fmt.Sprintf("CREATE TABLE %s (\n  %s\n)", tableName, strings.Join(cols, ",\n  "))

	// Search indexes — each as a separate statement (LibSQL no multi-statement Exec).
	var statements []string
	statements = append(statements, ddl)
	for _, f := range dt.DataFields() {
		if f.SearchIndex {
			statements = append(statements, d.CreateIndex(dt.RawTableName(), f.Fieldname, false))
		}
	}

	// UNIQUE indexes — each as a separate statement.
	for _, f := range dt.DataFields() {
		if f.Unique {
			statements = append(statements, d.CreateIndex(dt.RawTableName(), f.Fieldname, true))
		}
	}

	// ON UPDATE trigger for the modified column (SQLite has no ON UPDATE attribute).
	triggerName := d.QuoteIdent(dt.RawTableName() + "_modified_on_update")
	triggerDDL := fmt.Sprintf("CREATE TRIGGER %s AFTER UPDATE ON %s FOR EACH ROW BEGIN UPDATE %s SET \"modified\" = STRFTIME('%%Y-%%m-%%d %%H:%%M:%%f', 'NOW') WHERE \"name\" = NEW.\"name\"; END",
		triggerName, tableName, tableName)
	statements = append(statements, triggerDDL)

	return statements
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

func (d *LibSQLDialect) SystemColumnDDL() []string {
	return []string{
		`"name" TEXT NOT NULL`,
		`"owner" TEXT NOT NULL DEFAULT ''`,
		`"creation" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`,
		`"modified" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`,
		`"modified_by" TEXT NOT NULL DEFAULT ''`,
		`"doc_status" INTEGER NOT NULL DEFAULT 0`,
		`"idx" INTEGER NOT NULL DEFAULT 0`,
	}
}

func (d *LibSQLDialect) ChildColumnDDL() []string {
	return []string{
		`"parent" TEXT NOT NULL DEFAULT ''`,
		`"parentfield" TEXT NOT NULL DEFAULT ''`,
		`"parenttype" TEXT NOT NULL DEFAULT ''`,
	}
}

func (d *LibSQLDialect) TableSuffix() string {
	return ""
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

func (d *LibSQLDialect) UpsertClause(conflictCols []string, updateCols []string) string {
	var quotedCols []string
	for _, col := range conflictCols {
		quotedCols = append(quotedCols, d.QuoteIdent(col))
	}
	var parts []string
	for _, col := range updateCols {
		parts = append(parts, fmt.Sprintf("%s = excluded.%s", d.QuoteIdent(col), d.QuoteIdent(col)))
	}
	return fmt.Sprintf("ON CONFLICT(%s) DO UPDATE SET %s", strings.Join(quotedCols, ", "), strings.Join(parts, ", "))
}

func (d *LibSQLDialect) UpsertIncrement(conflictCols []string, incrementCols []string) string {
	var quotedCols []string
	for _, col := range conflictCols {
		quotedCols = append(quotedCols, d.QuoteIdent(col))
	}
	var parts []string
	for _, col := range incrementCols {
		q := d.QuoteIdent(col)
		parts = append(parts, fmt.Sprintf("%s = %s + excluded.%s", q, q, q))
	}
	return fmt.Sprintf("ON CONFLICT(%s) DO UPDATE SET %s", strings.Join(quotedCols, ", "), strings.Join(parts, ", "))
}

func (d *LibSQLDialect) InsertOrIgnorePrefix() string { return "INSERT OR IGNORE" }

func (d *LibSQLDialect) NameGenQuery(tableName, prefix string) string {
	// SQLite: SUBSTR(name, INSTR(name, '-')+1) extracts text after the first dash.
	// CAST AS INTEGER converts to number.
	return fmt.Sprintf(
		"SELECT COALESCE(MAX(CAST(SUBSTR(name, INSTR(name, '-')+1) AS INTEGER)), 0) FROM %s WHERE name LIKE '%s-%%'",
		d.QuoteIdent(tableName), prefix,
	)
}

func (d *LibSQLDialect) SystemTableSQL() []string {
	return []string{
		// _kora_doctype
		`CREATE TABLE IF NOT EXISTS "_kora_doctype" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"module" TEXT NOT NULL DEFAULT '',
			"is_submittable" INTEGER NOT NULL DEFAULT 0,
			"is_child_table" INTEGER NOT NULL DEFAULT 0,
			"is_single" INTEGER NOT NULL DEFAULT 0,
			"track_changes" INTEGER NOT NULL DEFAULT 0,
			"title_field" TEXT NOT NULL DEFAULT 'name',
			"search_fields" TEXT NOT NULL DEFAULT '',
			"sort_field" TEXT NOT NULL DEFAULT 'modified',
			"sort_order" TEXT NOT NULL DEFAULT 'DESC',
			"description" TEXT,
			"config_json" TEXT,
			"version" INTEGER NOT NULL DEFAULT 1,
			"creation" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),
			"modified" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))
		)`,

		// Add version column to existing _kora_doctype tables (backwards compat).
		`ALTER TABLE "_kora_doctype" ADD COLUMN "version" INTEGER NOT NULL DEFAULT 1`,

		// _kora_field
		`CREATE TABLE IF NOT EXISTS "_kora_field" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"parent" TEXT NOT NULL,
			"fieldname" TEXT NOT NULL,
			"fieldtype" TEXT NOT NULL,
			"label" TEXT NOT NULL DEFAULT '',
			"options" TEXT,
			"reqd" INTEGER NOT NULL DEFAULT 0,
			"unique_constraint" INTEGER NOT NULL DEFAULT 0,
			"default_value" TEXT,
			"hidden" INTEGER NOT NULL DEFAULT 0,
			"read_only" INTEGER NOT NULL DEFAULT 0,
			"bold" INTEGER NOT NULL DEFAULT 0,
			"in_list_view" INTEGER NOT NULL DEFAULT 0,
			"in_standard_filter" INTEGER NOT NULL DEFAULT 0,
			"search_index" INTEGER NOT NULL DEFAULT 0,
			"description" TEXT,
			"depends_on" TEXT,
			"mandatory_depends_on" TEXT,
			"constraints_json" TEXT,
			"renamed_from" TEXT,
			"linked_field" TEXT NOT NULL DEFAULT '',
			"computed" TEXT,
			"idx" INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS "idx_parent" ON "_kora_field" ("parent")`,
		`CREATE INDEX IF NOT EXISTS "idx_parent_fieldname" ON "_kora_field" ("parent", "fieldname")`,

		// Add columns for backwards compat.
		`ALTER TABLE "_kora_field" ADD COLUMN "linked_field" TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE "_kora_field" ADD COLUMN "computed" TEXT`,

		// _kora_role
		`CREATE TABLE IF NOT EXISTS "_kora_role" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"workspace_access" INTEGER NOT NULL DEFAULT 1,
			"description" TEXT
		)`,

		// _kora_permission
		`CREATE TABLE IF NOT EXISTS "_kora_permission" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"doctype" TEXT NOT NULL,
			"role" TEXT NOT NULL,
			"can_read" INTEGER NOT NULL DEFAULT 0,
			"can_write" INTEGER NOT NULL DEFAULT 0,
			"can_create" INTEGER NOT NULL DEFAULT 0,
			"can_delete" INTEGER NOT NULL DEFAULT 0,
			"can_submit" INTEGER NOT NULL DEFAULT 0,
			"can_cancel" INTEGER NOT NULL DEFAULT 0,
			"can_amend" INTEGER NOT NULL DEFAULT 0,
			"can_export" INTEGER NOT NULL DEFAULT 0,
			"can_import" INTEGER NOT NULL DEFAULT 0,
			"can_report" INTEGER NOT NULL DEFAULT 0,
			"if_owner" INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_doctype_role" ON "_kora_permission" ("doctype", "role")`,

		// _kora_config_version
		`CREATE TABLE IF NOT EXISTS "_kora_config_version" (
			"id" TEXT NOT NULL PRIMARY KEY,
			"site" TEXT NOT NULL,
			"version" INTEGER NOT NULL,
			"created_at" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),
			"created_by" TEXT NOT NULL DEFAULT 'system',
			"label" TEXT NOT NULL DEFAULT '',
			"changelog" TEXT,
			"status" TEXT NOT NULL DEFAULT 'Draft',
			"config" TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS "idx_site_status" ON "_kora_config_version" ("site", "status")`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_version_unique" ON "_kora_config_version" ("site", "version")`,

		// Backwards compat columns.
		`ALTER TABLE "_kora_config_version" ADD COLUMN "status" TEXT NOT NULL DEFAULT 'Superseded'`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "is_active" INTEGER NOT NULL DEFAULT 0`,
		`UPDATE "_kora_config_version" SET "status" = 'Active' WHERE "is_active" = 1 AND "status" = 'Superseded'`,

		// _kora_user
		`CREATE TABLE IF NOT EXISTS "_kora_user" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"site" TEXT NOT NULL DEFAULT '',
			"email" TEXT NOT NULL DEFAULT '',
			"password_hash" TEXT NOT NULL,
			"full_name" TEXT NOT NULL DEFAULT '',
			"enabled" INTEGER NOT NULL DEFAULT 1,
			"roles" TEXT,
			"creation" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),
			"modified" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_email" ON "_kora_user" ("site", "email")`,
		`ALTER TABLE "_kora_user" ADD COLUMN "site" TEXT NOT NULL DEFAULT ''`,
		`DROP INDEX IF EXISTS "idx_email"`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_email" ON "_kora_user" ("site", "email")`,
		// _kora_session
		`CREATE TABLE IF NOT EXISTS "_kora_session" (
			"sid" TEXT NOT NULL PRIMARY KEY,
			"site" TEXT NOT NULL DEFAULT '',
			"user" TEXT NOT NULL,
			"data" TEXT,
			"expires_at" TEXT NOT NULL,
			"created_at" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))
		)`,
		`ALTER TABLE "_kora_session" ADD COLUMN "site" TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS "idx_user" ON "_kora_session" ("user")`,
		`CREATE INDEX IF NOT EXISTS "idx_expires" ON "_kora_session" ("expires_at")`,

		// _kora_workflow
		`CREATE TABLE IF NOT EXISTS "_kora_workflow" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"document_type" TEXT NOT NULL DEFAULT '',
			"is_active" INTEGER NOT NULL DEFAULT 1,
			"workflow_state_field" TEXT NOT NULL DEFAULT '',
			"config_json" TEXT
		)`,

		// _kora_workflow_state
		`CREATE TABLE IF NOT EXISTS "_kora_workflow_state" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"workflow" TEXT NOT NULL,
			"state" TEXT NOT NULL DEFAULT '',
			"doc_status" INTEGER NOT NULL DEFAULT 0,
			"allow_edit" INTEGER NOT NULL DEFAULT 1,
			"style" TEXT NOT NULL DEFAULT '',
			"idx" INTEGER NOT NULL DEFAULT 0
		)`,

		// _kora_workflow_transition
		`CREATE TABLE IF NOT EXISTS "_kora_workflow_transition" (
			"name" TEXT NOT NULL PRIMARY KEY,
			"workflow" TEXT NOT NULL,
			"action" TEXT NOT NULL DEFAULT '',
			"from_state" TEXT NOT NULL DEFAULT '',
			"to_state" TEXT NOT NULL DEFAULT '',
			"allowed" TEXT NOT NULL DEFAULT '',
			"condition_expr" TEXT,
			"require_fields" TEXT,
			"idx" INTEGER NOT NULL DEFAULT 0
		)`,

		// _kora_secret
		`CREATE TABLE IF NOT EXISTS "_kora_secret" (
			"site" TEXT NOT NULL,
			"key_name" TEXT NOT NULL,
			"encrypted_value" BLOB NOT NULL,
			"created_at" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),
			"updated_at" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),
			PRIMARY KEY ("site", "key_name")
		)`,

		// Backwards compat: add columns to existing tables.
		`ALTER TABLE "_kora_doctype" ADD COLUMN "config_json" TEXT`,
		`ALTER TABLE "_kora_workflow" ADD COLUMN "config_json" TEXT`,
	}
}

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

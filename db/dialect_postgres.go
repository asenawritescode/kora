package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"github.com/lib/pq"
)

// PostgresDialect implements Dialect for PostgreSQL.
// It is a proof-of-concept that adding a new dialect to Kora requires
// only a single file implementing the Dialect interface.
type PostgresDialect struct{}

func (d *PostgresDialect) DriverName() string { return "postgres" }

func (d *PostgresDialect) Open(cfg DBConfig) (*sql.DB, error) {
	// Build DSN: postgres://user:pass@host:port/db?sslmode=disable
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Name)
	for k, v := range cfg.Params {
		dsn += fmt.Sprintf("&%s=%s", k, v)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return db, nil
}

// ---------------------------------------------------------------------------
// Schema Introspection
// ---------------------------------------------------------------------------

func (d *PostgresDialect) LoadSchema(db *sql.DB, _ string) (*LiveSchema, error) {
	schema := &LiveSchema{Tables: make(map[string]*LiveTable)}

	// Read columns from INFORMATION_SCHEMA.
	colRows, err := db.Query(`
		SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = 'public' AND TABLE_NAME LIKE 'tab%'
		ORDER BY TABLE_NAME, ORDINAL_POSITION`)
	if err != nil {
		return nil, fmt.Errorf("reading columns: %w", err)
	}
	defer colRows.Close()

	for colRows.Next() {
		var tableName, colName, colType, isNullable string
		var colDefault sql.NullString
		if err := colRows.Scan(&tableName, &colName, &colType, &isNullable, &colDefault); err != nil {
			return nil, fmt.Errorf("scanning column: %w", err)
		}
		table := schema.ensureTable(tableName)
		table.Columns = append(table.Columns, LiveColumn{
			Name:         colName,
			Type:         colType,
			IsNullable:   isNullable == "YES",
			DefaultValue: colDefault.String,
		})
	}
	if err := colRows.Err(); err != nil {
		return nil, err
	}

	// Read indexes from pg_indexes view.
	idxRows, err := db.Query(`
		SELECT tablename, indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND tablename LIKE 'tab%'
		ORDER BY tablename, indexname`)
	if err != nil {
		return nil, fmt.Errorf("reading indexes: %w", err)
	}
	defer idxRows.Close()

	type idxKey struct{ table, name string }
	type pendingIndex struct {
		idx     *LiveIndex
		columns []string
	}
	seen := make(map[idxKey]*pendingIndex)
	var order []idxKey

	for idxRows.Next() {
		var tableName, indexName, indexDef string
		if err := idxRows.Scan(&tableName, &indexName, &indexDef); err != nil {
			return nil, fmt.Errorf("scanning index: %w", err)
		}
		key := idxKey{tableName, indexName}
		if _, ok := seen[key]; ok {
			continue
		}
		isUnique := strings.Contains(indexDef, "UNIQUE")
		cols := parsePostgresIndexColumns(indexDef)
		seen[key] = &pendingIndex{
			idx:     &LiveIndex{Name: indexName, IsUnique: isUnique},
			columns: cols,
		}
		order = append(order, key)
	}
	if err := idxRows.Err(); err != nil {
		return nil, err
	}
	for _, key := range order {
		p := seen[key]
		p.idx.Columns = p.columns
		table := schema.ensureTable(key.table)
		table.Indexes = append(table.Indexes, *p.idx)
	}

	return schema, nil
}

// parsePostgresIndexColumns extracts column names from a PostgreSQL index definition.
// Input: "CREATE UNIQUE INDEX uq_tabUser_email ON public.tabUser USING btree (email)"
// Output: ["email"]
func parsePostgresIndexColumns(indexDef string) []string {
	parenIdx := strings.LastIndex(indexDef, "(")
	if parenIdx < 0 || !strings.HasSuffix(indexDef, ")") {
		return nil
	}
	inner := indexDef[parenIdx+1 : len(indexDef)-1]
	if inner == "" {
		return nil
	}
	var cols []string
	for _, part := range strings.Split(inner, ",") {
		c := strings.TrimSpace(part)
		// Remove optional DESC/ASC/NULLS FIRST/LAST suffixes.
		c = strings.SplitN(c, " ", 2)[0]
		c = strings.Trim(c, `"`)
		if c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}

// ---------------------------------------------------------------------------
// DDL Generation
// ---------------------------------------------------------------------------

func (d *PostgresDialect) CreateTable(dt *doctype.DocType) []string {
	var cols []string

	// System columns.
	cols = append(cols,
		`"name" VARCHAR(140) NOT NULL`,
		`"owner" VARCHAR(140) NOT NULL DEFAULT ''`,
		`"creation" TIMESTAMP NOT NULL DEFAULT NOW()`,
		`"modified" TIMESTAMP NOT NULL DEFAULT NOW()`,
		`"modified_by" VARCHAR(140) NOT NULL DEFAULT ''`,
		`"doc_status" SMALLINT NOT NULL DEFAULT 0`,
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
		} else {
			col += " DEFAULT NULL"
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

	// Build statement list.
	var statements []string
	statements = append(statements, ddl)

	for _, f := range dt.DataFields() {
		if f.SearchIndex {
			statements = append(statements, d.CreateIndex(dt.RawTableName(), f.Fieldname, false))
		}
	}
	for _, f := range dt.DataFields() {
		if f.Unique {
			statements = append(statements, d.CreateIndex(dt.RawTableName(), f.Fieldname, true))
		}
	}

	return statements
}

func (d *PostgresDialect) AddColumn(tableName string, f *doctype.Field) string {
	col := fmt.Sprintf("%s %s", d.QuoteIdent(f.Fieldname), d.ColumnType(f))
	if f.Reqd {
		col += " NOT NULL"
	} else {
		col += " DEFAULT NULL"
	}
	if f.Default != "" && f.Fieldtype != "Check" {
		col += fmt.Sprintf(" DEFAULT '%s'", f.Default)
	}
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", d.QuoteIdent(tableName), col)
}

func (d *PostgresDialect) AlterColumn(tableName string, f *doctype.Field) string {
	return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s",
		d.QuoteIdent(tableName), d.QuoteIdent(f.Fieldname), d.ColumnType(f))
}

func (d *PostgresDialect) RenameColumn(tableName, oldName, newName string) string {
	return fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
		d.QuoteIdent(tableName), d.QuoteIdent(oldName), d.QuoteIdent(newName))
}

func (d *PostgresDialect) CreateIndex(tableName, fieldName string, unique bool) string {
	uq := ""
	if unique {
		uq = "UNIQUE "
	}
	indexName := fmt.Sprintf("idx_%s_%s", tableName, fieldName)
	if unique {
		indexName = fmt.Sprintf("uq_%s_%s", tableName, fieldName)
	}
	return fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)",
		uq, d.QuoteIdent(indexName), d.QuoteIdent(tableName), d.QuoteIdent(fieldName))
}

func (d *PostgresDialect) DropIndex(tableName, indexName string) string {
	// PostgreSQL uses DROP INDEX without ON clause (indexes are schema-scoped).
	return fmt.Sprintf("DROP INDEX IF EXISTS %s", d.QuoteIdent(indexName))
}

func (d *PostgresDialect) DropColumn(tableName, columnName string) string {
	return fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", d.QuoteIdent(tableName), d.QuoteIdent(columnName))
}

func (d *PostgresDialect) QuoteIdent(name string) string {
	return `"` + name + `"`
}

func (d *PostgresDialect) SystemColumnDDL() []string {
	return []string{
		`"name" VARCHAR(140) NOT NULL`,
		`"owner" VARCHAR(140) NOT NULL DEFAULT ''`,
		`"creation" TIMESTAMP NOT NULL DEFAULT NOW()`,
		`"modified" TIMESTAMP NOT NULL DEFAULT NOW()`,
		`"modified_by" VARCHAR(140) NOT NULL DEFAULT ''`,
		`"doc_status" SMALLINT NOT NULL DEFAULT 0`,
		`"idx" INTEGER NOT NULL DEFAULT 0`,
	}
}

func (d *PostgresDialect) ChildColumnDDL() []string {
	return []string{
		`"parent" VARCHAR(140) NOT NULL DEFAULT ''`,
		`"parentfield" VARCHAR(140) NOT NULL DEFAULT ''`,
		`"parenttype" VARCHAR(140) NOT NULL DEFAULT ''`,
	}
}

func (d *PostgresDialect) TableSuffix() string {
	return ""
}

func (d *PostgresDialect) ColumnType(f *doctype.Field) string {
	switch f.Fieldtype {
	case "Data", "Select", "Link", "Dynamic Link":
		return "VARCHAR(140)"
	case "Text", "Text Editor":
		return "TEXT"
	case "Int":
		return "BIGINT"
	case "Float", "Currency", "Percent":
		return "DECIMAL(21,9)"
	case "Check":
		return "BOOLEAN"
	case "Date":
		return "DATE"
	case "Time":
		return "TIME"
	case "Datetime":
		return "TIMESTAMP"
	case "Attach", "Attach Image":
		return "TEXT"
	case "JSON":
		return "JSONB"
	case "Password":
		return "VARCHAR(255)"
	default:
		return "TEXT"
	}
}

func (d *PostgresDialect) NowTimestamp() string {
	return "NOW()"
}

func (d *PostgresDialect) ParseError(err error, dt *doctype.DocType) *doctype.ValidationError {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return nil
	}
	switch pqErr.Code {
	case "23505": // unique_violation
		fieldName := parsePostgresKeyField(string(pqErr.Code), pqErr)
		if fieldName == "" {
			fieldName = pqErr.Column
		}
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
	case "23502": // not_null_violation
		fieldName := pqErr.Column
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

// parsePostgresKeyField extracts the field name from a PostgreSQL constraint name.
// Constraint names follow the pattern: uq_tab<table>_<field>
// We extract the field name by taking everything after the last underscore.
func parsePostgresKeyField(_ string, _ *pq.Error) string {
	// The pq.Error struct does not expose Constraint directly in a standard way.
	// For a complete implementation, we rely on the Column field from the error.
	// This is a placeholder for future enhancement.
	return ""
}

func (d *PostgresDialect) Placeholder(n int) string {
	return fmt.Sprintf("$%d", n)
}

func (d *PostgresDialect) UpsertClause(conflictCols []string, updateCols []string) string {
	var quotedCols []string
	for _, col := range conflictCols {
		quotedCols = append(quotedCols, d.QuoteIdent(col))
	}
	var parts []string
	for _, col := range updateCols {
		parts = append(parts, fmt.Sprintf("%s = EXCLUDED.%s", d.QuoteIdent(col), d.QuoteIdent(col)))
	}
	return fmt.Sprintf("ON CONFLICT(%s) DO UPDATE SET %s", strings.Join(quotedCols, ", "), strings.Join(parts, ", "))
}

func (d *PostgresDialect) UpsertIncrement(conflictCols []string, incrementCols []string) string {
	var quotedCols []string
	for _, col := range conflictCols {
		quotedCols = append(quotedCols, d.QuoteIdent(col))
	}
	var parts []string
	for _, col := range incrementCols {
		q := d.QuoteIdent(col)
		parts = append(parts, fmt.Sprintf("%s = %s + EXCLUDED.%s", q, q, q))
	}
	return fmt.Sprintf("ON CONFLICT(%s) DO UPDATE SET %s", strings.Join(quotedCols, ", "), strings.Join(parts, ", "))
}

func (d *PostgresDialect) InsertOrIgnorePrefix() string {
	// PostgreSQL does not have INSERT IGNORE. The equivalent is
	// INSERT ... ON CONFLICT DO NOTHING, which is a suffix, not a prefix.
	// Returning "INSERT" here means the calling code will produce a plain
	// INSERT without conflict handling. For the PoC this is acceptable —
	// the bootstrap code discards errors from this call.
	return "INSERT"
}

func (d *PostgresDialect) NameGenQuery(tableName, prefix string) string {
	// PostgreSQL: SPLIT_PART(name, '-', 2) extracts the text after the first dash.
	return fmt.Sprintf(
		"SELECT COALESCE(MAX(CAST(SPLIT_PART(name, '-', 2) AS INTEGER)), 0) FROM %s WHERE name LIKE '%s-%%'",
		d.QuoteIdent(tableName), prefix,
	)
}

// ExecuteBatch runs multiple DDL statements atomically inside a transaction.
func (d *PostgresDialect) ExecuteBatch(db *sql.DB, statements []string) error {
	if len(statements) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction for DDL batch: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("executing DDL: %w\nSQL: %s", err, stmt)
		}
	}
	return tx.Commit()
}

func (d *PostgresDialect) SystemTableSQL() []string {
	return []string{
		// _kora_doctype
		`CREATE TABLE IF NOT EXISTS "_kora_doctype" (
			"name" VARCHAR(140) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"module" VARCHAR(140) NOT NULL DEFAULT '',
			"is_submittable" SMALLINT NOT NULL DEFAULT 0,
			"is_child_table" SMALLINT NOT NULL DEFAULT 0,
			"is_single" SMALLINT NOT NULL DEFAULT 0,
			"track_changes" SMALLINT NOT NULL DEFAULT 0,
			"title_field" VARCHAR(140) NOT NULL DEFAULT 'name',
			"search_fields" VARCHAR(255) NOT NULL DEFAULT '',
			"sort_field" VARCHAR(140) NOT NULL DEFAULT 'modified',
			"sort_order" VARCHAR(4) NOT NULL DEFAULT 'DESC',
			"description" TEXT,
			"config_json" JSONB,
			"version" INTEGER NOT NULL DEFAULT 1,
			"creation" TIMESTAMP NOT NULL DEFAULT NOW(),
			"modified" TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE "_kora_doctype" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,
		`ALTER TABLE "_kora_doctype" ADD COLUMN "version" INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE "_kora_doctype" ADD COLUMN "config_json" JSONB`,
		`CREATE INDEX IF NOT EXISTS "idx_doctype_site" ON "_kora_doctype" ("site")`,

		// _kora_field
		`CREATE TABLE IF NOT EXISTS "_kora_field" (
			"name" VARCHAR(140) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"parent" VARCHAR(140) NOT NULL,
			"fieldname" VARCHAR(140) NOT NULL,
			"fieldtype" VARCHAR(50) NOT NULL,
			"label" VARCHAR(255) NOT NULL DEFAULT '',
			"options" TEXT,
			"reqd" SMALLINT NOT NULL DEFAULT 0,
			"unique_constraint" SMALLINT NOT NULL DEFAULT 0,
			"default_value" VARCHAR(255),
			"hidden" SMALLINT NOT NULL DEFAULT 0,
			"read_only" SMALLINT NOT NULL DEFAULT 0,
			"bold" SMALLINT NOT NULL DEFAULT 0,
			"in_list_view" SMALLINT NOT NULL DEFAULT 0,
			"in_standard_filter" SMALLINT NOT NULL DEFAULT 0,
			"search_index" SMALLINT NOT NULL DEFAULT 0,
			"description" TEXT,
			"depends_on" TEXT,
			"mandatory_depends_on" TEXT,
			"constraints_json" JSONB,
			"renamed_from" VARCHAR(140),
			"linked_field" VARCHAR(255) NOT NULL DEFAULT '',
			"computed" TEXT,
			"idx" INTEGER NOT NULL DEFAULT 0
		)`,
		`ALTER TABLE "_kora_field" ADD COLUMN "linked_field" VARCHAR(255) NOT NULL DEFAULT ''`,
		`ALTER TABLE "_kora_field" ADD COLUMN "computed" TEXT`,
		`ALTER TABLE "_kora_field" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS "idx_parent" ON "_kora_field" ("parent")`,
		`CREATE INDEX IF NOT EXISTS "idx_parent_fieldname" ON "_kora_field" ("parent", "fieldname")`,

		// _kora_role
		`CREATE TABLE IF NOT EXISTS "_kora_role" (
			"name" VARCHAR(140) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"workspace_access" SMALLINT NOT NULL DEFAULT 1,
			"description" TEXT
		)`,
		`ALTER TABLE "_kora_role" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,

		// _kora_permission
		`CREATE TABLE IF NOT EXISTS "_kora_permission" (
			"name" VARCHAR(140) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"doctype" VARCHAR(140) NOT NULL,
			"role" VARCHAR(140) NOT NULL,
			"can_read" SMALLINT NOT NULL DEFAULT 0,
			"can_write" SMALLINT NOT NULL DEFAULT 0,
			"can_create" SMALLINT NOT NULL DEFAULT 0,
			"can_delete" SMALLINT NOT NULL DEFAULT 0,
			"can_submit" SMALLINT NOT NULL DEFAULT 0,
			"can_cancel" SMALLINT NOT NULL DEFAULT 0,
			"can_amend" SMALLINT NOT NULL DEFAULT 0,
			"can_export" SMALLINT NOT NULL DEFAULT 0,
			"can_import" SMALLINT NOT NULL DEFAULT 0,
			"can_report" SMALLINT NOT NULL DEFAULT 0,
			"if_owner" SMALLINT NOT NULL DEFAULT 0
		)`,
		`ALTER TABLE "_kora_permission" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_doctype_role" ON "_kora_permission" ("doctype", "role")`,

		// _kora_config_version
		`CREATE TABLE IF NOT EXISTS "_kora_config_version" (
			"id" VARCHAR(36) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL,
			"version" INTEGER NOT NULL,
			"created_at" TIMESTAMP NOT NULL DEFAULT NOW(),
			"created_by" VARCHAR(140) NOT NULL DEFAULT 'system',
			"label" VARCHAR(255) NOT NULL DEFAULT '',
			"changelog" JSONB,
			"status" VARCHAR(20) NOT NULL DEFAULT 'Draft',
			"config" JSONB,
			"change_list" JSONB,
			"config_hash" VARCHAR(64) NOT NULL DEFAULT '',
			"base_version_id" VARCHAR(36) NOT NULL DEFAULT '',
			"min_kora_version" VARCHAR(20) NOT NULL DEFAULT ''
		)`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "status" VARCHAR(20) NOT NULL DEFAULT 'Superseded'`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "is_active" SMALLINT NOT NULL DEFAULT 0`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "change_list" JSONB`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "config_hash" VARCHAR(64) NOT NULL DEFAULT ''`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "base_version_id" VARCHAR(36) NOT NULL DEFAULT ''`,
		`ALTER TABLE "_kora_config_version" ADD COLUMN "min_kora_version" VARCHAR(20) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS "idx_site_status" ON "_kora_config_version" ("site", "status")`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_version_unique" ON "_kora_config_version" ("site", "version")`,

		// _kora_user
		`CREATE TABLE IF NOT EXISTS "_kora_user" (
			"name" VARCHAR(140) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"email" VARCHAR(255) NOT NULL DEFAULT '',
			"password_hash" VARCHAR(255) NOT NULL,
			"full_name" VARCHAR(255) NOT NULL DEFAULT '',
			"enabled" SMALLINT NOT NULL DEFAULT 1,
			"roles" TEXT,
			"creation" TIMESTAMP NOT NULL DEFAULT NOW(),
			"modified" TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE "_kora_user" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_email" ON "_kora_user" ("site", "email")`,

		// _kora_session
		`CREATE TABLE IF NOT EXISTS "_kora_session" (
			"sid" VARCHAR(255) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"user" VARCHAR(140) NOT NULL,
			"data" JSONB,
			"expires_at" TIMESTAMP NOT NULL,
			"created_at" TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE "_kora_session" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS "idx_user" ON "_kora_session" ("user")`,
		`CREATE INDEX IF NOT EXISTS "idx_expires" ON "_kora_session" ("expires_at")`,

		// _kora_workflow
		`CREATE TABLE IF NOT EXISTS "_kora_workflow" (
			"name" VARCHAR(140) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"document_type" VARCHAR(140) NOT NULL DEFAULT '',
			"is_active" SMALLINT NOT NULL DEFAULT 1,
			"workflow_state_field" VARCHAR(140) NOT NULL DEFAULT '',
			"config_json" JSONB
		)`,
		`ALTER TABLE "_kora_workflow" ADD COLUMN "site" VARCHAR(140) NOT NULL DEFAULT ''`,
		`ALTER TABLE "_kora_workflow" ADD COLUMN "config_json" JSONB`,

		// _kora_workflow_state
		`CREATE TABLE IF NOT EXISTS "_kora_workflow_state" (
			"name" VARCHAR(255) NOT NULL,
			"workflow" VARCHAR(140) NOT NULL,
			"state" VARCHAR(140) NOT NULL DEFAULT '',
			"doc_status" SMALLINT NOT NULL DEFAULT 0,
			"allow_edit" SMALLINT NOT NULL DEFAULT 1,
			"style" VARCHAR(255) NOT NULL DEFAULT '',
			"idx" INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY ("name")
		)`,

		// _kora_workflow_transition
		`CREATE TABLE IF NOT EXISTS "_kora_workflow_transition" (
			"name" VARCHAR(255) NOT NULL,
			"workflow" VARCHAR(140) NOT NULL,
			"action" VARCHAR(140) NOT NULL DEFAULT '',
			"from_state" VARCHAR(255) NOT NULL DEFAULT '',
			"to_state" VARCHAR(255) NOT NULL DEFAULT '',
			"allowed" VARCHAR(255) NOT NULL DEFAULT '',
			"condition_expr" TEXT,
			"require_fields" TEXT,
			"idx" INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY ("name")
		)`,

		// _kora_secret
		`CREATE TABLE IF NOT EXISTS "_kora_secret" (
			"site" VARCHAR(140) NOT NULL,
			"key_name" VARCHAR(140) NOT NULL,
			"encrypted_value" BYTEA NOT NULL,
			"created_at" TIMESTAMP NOT NULL DEFAULT NOW(),
			"updated_at" TIMESTAMP NOT NULL DEFAULT NOW(),
			PRIMARY KEY ("site", "key_name")
		)`,
	}
}

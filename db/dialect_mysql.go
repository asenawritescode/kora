package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/asenawritescode/kora/doctype"
)

// MySQLDialect implements Dialect for MySQL 8.0.
type MySQLDialect struct{}

func (d *MySQLDialect) DriverName() string { return "mysql" }

func (d *MySQLDialect) Open(cfg DBConfig) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Name)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging mysql: %w", err)
	}
	return db, nil
}

// ---------------------------------------------------------------------------
// Schema Introspection
// ---------------------------------------------------------------------------

func (d *MySQLDialect) LoadSchema(db *sql.DB, dbName string) (*LiveSchema, error) {
	schema := &LiveSchema{Tables: make(map[string]*LiveTable)}

	// Read columns from INFORMATION_SCHEMA.
	colRows, err := db.Query(`
		SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'tab%'
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, dbName)
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

	// Read indexes from INFORMATION_SCHEMA.
	idxRows, err := db.Query(`
		SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE
		FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'tab%'
		ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, dbName)
	if err != nil {
		return nil, fmt.Errorf("reading indexes: %w", err)
	}
	defer idxRows.Close()

	type idxKey struct{ table, name string }
	seen := make(map[idxKey]*LiveIndex)
	var order []idxKey
	for idxRows.Next() {
		var tableName, indexName, columnName string
		var nonUnique int
		if err := idxRows.Scan(&tableName, &indexName, &columnName, &nonUnique); err != nil {
			return nil, fmt.Errorf("scanning index: %w", err)
		}
		key := idxKey{tableName, indexName}
		idx, ok := seen[key]
		if !ok {
			idx = &LiveIndex{Name: indexName, IsUnique: nonUnique == 0}
			seen[key] = idx
			order = append(order, key)
		}
		idx.Columns = append(idx.Columns, columnName)
	}
	if err := idxRows.Err(); err != nil {
		return nil, err
	}
	for _, key := range order {
		table := schema.ensureTable(key.table)
		table.Indexes = append(table.Indexes, *seen[key])
	}

	return schema, nil
}

func (s *LiveSchema) ensureTable(name string) *LiveTable {
	t, ok := s.Tables[name]
	if !ok {
		t = &LiveTable{Name: name}
		s.Tables[name] = t
	}
	return t
}

// ---------------------------------------------------------------------------
// DDL Generation
// ---------------------------------------------------------------------------

func (d *MySQLDialect) CreateTable(dt *doctype.DocType) []string {
	var cols []string

	// System columns.
	cols = append(cols,
		"name VARCHAR(140) NOT NULL",
		"owner VARCHAR(140) NOT NULL DEFAULT ''",
		"creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)",
		"modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)",
		"modified_by VARCHAR(140) NOT NULL DEFAULT ''",
		"doc_status TINYINT(1) NOT NULL DEFAULT 0",
		"idx INT NOT NULL DEFAULT 0",
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
	cols = append(cols, "PRIMARY KEY (name)")

	tableName := d.QuoteIdent(dt.RawTableName())
	ddl := fmt.Sprintf("CREATE TABLE %s (\n  %s\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		tableName, strings.Join(cols, ",\n  "))

	// Build statement list — separate CREATE INDEX statements for MySQL compatibility.
	// MySQL does not support multi-statement Exec calls without multiStatements=true.
	statements := []string{ddl}

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

func (d *MySQLDialect) AddColumn(tableName string, f *doctype.Field) string {
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

func (d *MySQLDialect) AlterColumn(tableName string, f *doctype.Field) string {
	col := fmt.Sprintf("%s %s", d.QuoteIdent(f.Fieldname), d.ColumnType(f))
	if f.Reqd {
		col += " NOT NULL"
	} else {
		col += " DEFAULT NULL"
	}
	return fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s", d.QuoteIdent(tableName), col)
}

func (d *MySQLDialect) RenameColumn(tableName, oldName, newName string) string {
	return fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
		d.QuoteIdent(tableName), d.QuoteIdent(oldName), d.QuoteIdent(newName))
}

func (d *MySQLDialect) CreateIndex(tableName, fieldName string, unique bool) string {
	uq := ""
	if unique {
		uq = "UNIQUE "
	}
	// Include table name for consistency with LibSQL and to avoid
	// confusion when multiple tables have identically-named fields.
	indexName := fmt.Sprintf("idx_%s_%s", tableName, fieldName)
	if unique {
		indexName = fmt.Sprintf("uq_%s_%s", tableName, fieldName)
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)",
		uq, d.QuoteIdent(indexName), d.QuoteIdent(tableName), d.QuoteIdent(fieldName))
}

func (d *MySQLDialect) DropIndex(tableName, indexName string) string {
	return fmt.Sprintf("DROP INDEX %s ON %s", d.QuoteIdent(indexName), d.QuoteIdent(tableName))
}

func (d *MySQLDialect) DropColumn(tableName, columnName string) string {
	return fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", d.QuoteIdent(tableName), d.QuoteIdent(columnName))
}

func (d *MySQLDialect) QuoteIdent(name string) string {
	return "`" + name + "`"
}

func (d *MySQLDialect) SystemColumnDDL() []string {
	return []string{
		"name VARCHAR(140) NOT NULL",
		"owner VARCHAR(140) NOT NULL DEFAULT ''",
		"creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)",
		"modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)",
		"modified_by VARCHAR(140) NOT NULL DEFAULT ''",
		"doc_status TINYINT(1) NOT NULL DEFAULT 0",
		"idx INT NOT NULL DEFAULT 0",
	}
}

func (d *MySQLDialect) ChildColumnDDL() []string {
	return []string{
		"parent VARCHAR(140) NOT NULL DEFAULT ''",
		"parentfield VARCHAR(140) NOT NULL DEFAULT ''",
		"parenttype VARCHAR(140) NOT NULL DEFAULT ''",
	}
}

func (d *MySQLDialect) TableSuffix() string {
	return "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"
}

func (d *MySQLDialect) ColumnType(f *doctype.Field) string {
	switch f.Fieldtype {
	case "Data", "Select", "Link", "Dynamic Link":
		return "VARCHAR(140)"
	case "Text":
		return "TEXT"
	case "Text Editor":
		return "LONGTEXT"
	case "Int":
		return "BIGINT"
	case "Float", "Currency", "Percent":
		return "DECIMAL(21,9)"
	case "Check":
		return "TINYINT(1)"
	case "Date":
		return "DATE"
	case "Time":
		return "TIME(6)"
	case "Datetime":
		return "DATETIME(6)"
	case "Attach", "Attach Image":
		return "TEXT"
	case "JSON":
		return "JSON"
	case "Password":
		return "VARCHAR(255)"
	default:
		return "TEXT"
	}
}

func (d *MySQLDialect) NowTimestamp() string {
	return "CURRENT_TIMESTAMP(6)"
}

func (d *MySQLDialect) ParseError(err error, dt *doctype.DocType) *doctype.ValidationError {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return nil
	}
	switch mysqlErr.Number {
	case 1062: // Duplicate entry
		fieldName := parseMySQLKeyField(mysqlErr.Message)
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
	case 1364, 1048: // Field doesn't have a default value / Column cannot be null
		fieldName := parseMySQLNotNullField(mysqlErr.Message)
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

func (d *MySQLDialect) Placeholder(n int) string { return "?" }

func (d *MySQLDialect) UpsertClause(conflictCols []string, updateCols []string) string {
	var parts []string
	for _, col := range updateCols {
		parts = append(parts, fmt.Sprintf("%s = VALUES(%s)", d.QuoteIdent(col), d.QuoteIdent(col)))
	}
	return "ON DUPLICATE KEY UPDATE " + strings.Join(parts, ", ")
}

func (d *MySQLDialect) UpsertIncrement(conflictCols []string, incrementCols []string) string {
	var parts []string
	for _, col := range incrementCols {
		q := d.QuoteIdent(col)
		parts = append(parts, fmt.Sprintf("%s = %s + VALUES(%s)", q, q, q))
	}
	return "ON DUPLICATE KEY UPDATE " + strings.Join(parts, ", ")
}

func (d *MySQLDialect) InsertOrIgnorePrefix() string { return "INSERT IGNORE" }

func (d *MySQLDialect) NameGenQuery(tableName, prefix string) string {
	return fmt.Sprintf(
		"SELECT COALESCE(MAX(CAST(SUBSTRING_INDEX(name, '-', -1) AS UNSIGNED)), 0) FROM %s WHERE name LIKE '%s-%%'",
		d.QuoteIdent(tableName), prefix,
	)
}

// ExecuteBatch runs multiple DDL statements atomically inside a transaction.
func (d *MySQLDialect) ExecuteBatch(db *sql.DB, statements []string) error {
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

func (d *MySQLDialect) SystemTableSQL() []string {
	return []string{
		// _kora_doctype
		"CREATE TABLE IF NOT EXISTS _kora_doctype (\n\t\t\tname VARCHAR(140) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tmodule VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tis_submittable TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tis_child_table TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tis_single TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\ttrack_changes TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\ttitle_field VARCHAR(140) NOT NULL DEFAULT 'name',\n\t\t\tsearch_fields VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tsort_field VARCHAR(140) NOT NULL DEFAULT 'modified',\n\t\t\tsort_order VARCHAR(4) NOT NULL DEFAULT 'DESC',\n\t\t\tdescription TEXT,\n\t\t\tconfig_json JSON,\n\t\t\tversion INT NOT NULL DEFAULT 1,\n\t\t\tcreation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),\n\t\t\tmodified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",

		// Add version + config_json columns to existing tables (backwards compat).
		"ALTER TABLE _kora_doctype ADD COLUMN version INT NOT NULL DEFAULT 1",
		"ALTER TABLE _kora_doctype ADD COLUMN config_json JSON",
		"ALTER TABLE _kora_doctype ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT ''",
		"ALTER TABLE _kora_workflow ADD COLUMN config_json JSON",

		// _kora_field
		"CREATE TABLE IF NOT EXISTS _kora_field (\n\t\t\tname VARCHAR(140) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tparent VARCHAR(140) NOT NULL,\n\t\t\tfieldname VARCHAR(140) NOT NULL,\n\t\t\tfieldtype VARCHAR(50) NOT NULL,\n\t\t\tlabel VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\toptions TEXT,\n\t\t\treqd TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tunique_constraint TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tdefault_value VARCHAR(255),\n\t\t\thidden TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tread_only TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tbold TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tin_list_view TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tin_standard_filter TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tsearch_index TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tdescription TEXT,\n\t\t\tdepends_on TEXT,\n\t\t\tmandatory_depends_on TEXT,\n\t\t\tconstraints_json JSON,\n\t\t\trenamed_from VARCHAR(140),\n\t\t\tlinked_field VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tcomputed TEXT,\n\t\t\tidx INT NOT NULL DEFAULT 0,\n\t\t\tINDEX idx_parent (parent),\n\t\t\tINDEX idx_parent_fieldname (parent, fieldname)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",

		// Add columns for backwards compat.
		"ALTER TABLE _kora_field ADD COLUMN linked_field VARCHAR(255) NOT NULL DEFAULT ''",
		"ALTER TABLE _kora_field ADD COLUMN computed TEXT",
		"ALTER TABLE _kora_field ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT ''",

		// _kora_role
		"CREATE TABLE IF NOT EXISTS _kora_role (\n\t\t\tname VARCHAR(140) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tworkspace_access TINYINT(1) NOT NULL DEFAULT 1,\n\t\t\tdescription TEXT\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		"ALTER TABLE _kora_role ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT ''",

		// _kora_permission
		"CREATE TABLE IF NOT EXISTS _kora_permission (\n\t\t\tname VARCHAR(140) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tdoctype VARCHAR(140) NOT NULL,\n\t\t\trole VARCHAR(140) NOT NULL,\n\t\t\tcan_read TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_write TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_create TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_delete TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_submit TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_cancel TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_amend TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_export TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_import TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tcan_report TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tif_owner TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tUNIQUE KEY idx_doctype_role (doctype, role)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		"ALTER TABLE _kora_permission ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT ''",

		// _kora_config_version
		"CREATE TABLE IF NOT EXISTS _kora_config_version (\n\t\t\tid VARCHAR(36) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL,\n\t\t\tversion INT NOT NULL,\n\t\t\tcreated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),\n\t\t\tcreated_by VARCHAR(140) NOT NULL DEFAULT 'system',\n\t\t\tlabel VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tchangelog JSON,\n\t\t\tstatus VARCHAR(20) NOT NULL DEFAULT 'Draft',\n\t\t\tconfig LONGTEXT,\n\t\t\tchange_list JSON,\n\t\t\tconfig_hash VARCHAR(64) NOT NULL DEFAULT '',\n\t\t\tbase_version_id VARCHAR(36) NOT NULL DEFAULT '',\n\t\t\tmin_kora_version VARCHAR(20) NOT NULL DEFAULT '',\n\t\t\tINDEX idx_site_status (site, status),\n\t\t\tUNIQUE INDEX idx_site_version_unique (site, version)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",

		// Backwards compat columns.
		"ALTER TABLE _kora_config_version ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'Superseded'",
		"ALTER TABLE _kora_config_version ADD COLUMN is_active TINYINT(1) NOT NULL DEFAULT 0",
		"ALTER TABLE _kora_config_version ADD COLUMN change_list JSON",
		"ALTER TABLE _kora_config_version MODIFY COLUMN config LONGTEXT",
		"ALTER TABLE _kora_config_version ADD COLUMN config_hash VARCHAR(64) NOT NULL DEFAULT ''",
		"ALTER TABLE _kora_config_version ADD COLUMN base_version_id VARCHAR(36) NOT NULL DEFAULT ''",
		"ALTER TABLE _kora_config_version ADD COLUMN min_kora_version VARCHAR(20) NOT NULL DEFAULT ''",
		"UPDATE _kora_config_version SET status = 'Active' WHERE is_active = 1 AND status = 'Superseded'",

		// _kora_user
		"CREATE TABLE IF NOT EXISTS _kora_user (\n\t\t\tname VARCHAR(140) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\temail VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tpassword_hash VARCHAR(255) NOT NULL,\n\t\t\tfull_name VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tenabled TINYINT(1) NOT NULL DEFAULT 1,\n\t\t\troles TEXT,\n\t\t\tcreation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),\n\t\t\tmodified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),\n\t\t\tUNIQUE KEY idx_site_email (site, email)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		"ALTER TABLE _kora_user ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT ''",
		"ALTER TABLE _kora_user DROP INDEX idx_email",
		"ALTER TABLE _kora_user ADD UNIQUE KEY idx_site_email (site, email)",

		// _kora_session
		"CREATE TABLE IF NOT EXISTS _kora_session (\n\t\t\tsid VARCHAR(255) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tuser VARCHAR(140) NOT NULL,\n\t\t\tdata JSON,\n\t\t\texpires_at DATETIME(6) NOT NULL,\n\t\t\tcreated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),\n\t\t\tINDEX idx_user (user),\n\t\t\tINDEX idx_expires (expires_at)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		"ALTER TABLE _kora_session ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT ''",

		// _kora_workflow
		"CREATE TABLE IF NOT EXISTS _kora_workflow (\n\t\t\tname VARCHAR(140) NOT NULL,\n\t\t\tsite VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tdocument_type VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tis_active TINYINT(1) NOT NULL DEFAULT 1,\n\t\t\tworkflow_state_field VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tconfig_json JSON,\n\t\t\tPRIMARY KEY (name)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		"ALTER TABLE _kora_workflow ADD COLUMN site VARCHAR(140) NOT NULL DEFAULT '',",

		// _kora_workflow_state
		"CREATE TABLE IF NOT EXISTS _kora_workflow_state (\n\t\t\tname VARCHAR(255) NOT NULL,\n\t\t\tworkflow VARCHAR(140) NOT NULL,\n\t\t\tstate VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tdoc_status TINYINT(1) NOT NULL DEFAULT 0,\n\t\t\tallow_edit TINYINT(1) NOT NULL DEFAULT 1,\n\t\t\tstyle VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tidx INT NOT NULL DEFAULT 0,\n\t\t\tPRIMARY KEY (name)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",

		// _kora_workflow_transition
		"CREATE TABLE IF NOT EXISTS _kora_workflow_transition (\n\t\t\tname VARCHAR(255) NOT NULL,\n\t\t\tworkflow VARCHAR(140) NOT NULL,\n\t\t\taction VARCHAR(140) NOT NULL DEFAULT '',\n\t\t\tfrom_state VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tto_state VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tallowed VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tcondition_expr TEXT,\n\t\t\trequire_fields TEXT,\n\t\t\tidx INT NOT NULL DEFAULT 0,\n\t\t\tPRIMARY KEY (name)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",

		// _kora_secret
		"CREATE TABLE IF NOT EXISTS _kora_secret (\n\t\t\tsite VARCHAR(140) NOT NULL,\n\t\t\tkey_name VARCHAR(140) NOT NULL,\n\t\t\tencrypted_value BLOB NOT NULL,\n\t\t\tcreated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),\n\t\t\tupdated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),\n\t\t\tPRIMARY KEY (site, key_name)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
	}
}

func parseMySQLKeyField(msg string) string {
	const prefix = "key 'uq_"
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		idx = strings.Index(msg, "key '")
		if idx < 0 {
			return ""
		}
		idx += len("key '")
	} else {
		idx += len(prefix)
	}
	end := strings.IndexByte(msg[idx:], '\'')
	if end < 0 {
		return ""
	}
	return msg[idx : idx+end]
}

func parseMySQLNotNullField(msg string) string {
	for _, prefix := range []string{"Field '", "Column '"} {
		idx := strings.Index(msg, prefix)
		if idx >= 0 {
			start := idx + len(prefix)
			end := strings.IndexByte(msg[start:], '\'')
			if end >= 0 {
				return msg[start : start+end]
			}
		}
	}
	return ""
}

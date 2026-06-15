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

func (d *MySQLDialect) CreateTable(dt *doctype.DocType) string {
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

	tableName := d.QuoteIdent(dt.TableName())
	ddl := fmt.Sprintf("CREATE TABLE %s (\n  %s\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		tableName, strings.Join(cols, ",\n  "))

	// Search indexes for fields with search_index: true.
	for _, f := range dt.DataFields() {
		if f.SearchIndex {
			ddl += "\n" + d.CreateIndex(dt.TableName(), f.Fieldname, false)
		}
	}

	// UNIQUE indexes for fields with unique: true.
	for _, f := range dt.DataFields() {
		if f.Unique {
			ddl += "\n" + d.CreateIndex(dt.TableName(), f.Fieldname, true)
		}
	}

	return ddl
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
	indexName := fmt.Sprintf("idx_%s", fieldName)
	if unique {
		indexName = fmt.Sprintf("uq_%s", fieldName)
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

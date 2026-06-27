// Package db provides database abstraction via a Dialect interface.
// Implementations for MySQL, LibSQL, and PostgreSQL live in separate files.
// The Dialect is selected at startup via the KORA_DB_TYPE env var.
package db

import (
	"database/sql"

	"github.com/asenawritescode/kora/doctype"
)

// Queryer is satisfied by *sql.DB and *sql.Tx.
// It provides Exec, Query, and QueryRow for use by packages that need
// to run SQL against either a database or a transaction.
type Queryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// SchemaDialect is the DDL subset of Dialect (table/column/index management).
type SchemaDialect interface {
	CreateTable(dt *doctype.DocType) []string
	AddColumn(tableName string, f *doctype.Field) string
	AlterColumn(tableName string, f *doctype.Field) string
	RenameColumn(tableName, oldName, newName string) string
	CreateIndex(tableName, fieldName string, unique bool) string
	DropIndex(tableName, indexName string) string
	DropColumn(tableName, columnName string) string
	ColumnType(f *doctype.Field) string
	SystemColumnDDL() []string
	ChildColumnDDL() []string
	QuoteIdent(name string) string
}

// QueryDialect is the DML subset (insert/update/select helpers).
type QueryDialect interface {
	UpsertClause(conflictCols []string, updateCols []string) string
	UpsertIncrement(conflictCols []string, incrementCols []string) string
	InsertOrIgnorePrefix() string
	NameGenQuery(tableName, prefix string) string
	Placeholder(n int) string
	NowTimestamp() string
}

// ErrorDialect is the error-parsing subset.
type ErrorDialect interface {
	ParseError(err error, dt *doctype.DocType) *doctype.ValidationError
}

// Dialect abstracts database-specific SQL generation, schema introspection,
// and error parsing. It composes SchemaDialect, QueryDialect, and ErrorDialect.
// Implementations are in dialect_*.go files.
type Dialect interface {
	SchemaDialect
	QueryDialect
	ErrorDialect

	// DriverName returns the database/sql driver name (e.g., "mysql", "libsql").
	DriverName() string

	// Open opens a database connection using the given config.
	Open(cfg DBConfig) (*sql.DB, error)

	// LoadSchema introspects the live database schema for comparison with
	// the doctype registry (used by schema migration).
	LoadSchema(db *sql.DB, dbName string) (*LiveSchema, error)

	// QuoteIdent quotes an identifier (table or column name) for safe embedding in SQL.
	QuoteIdent(name string) string

	// TableSuffix returns the table options suffix (ENGINE/CHARSET for MySQL, empty for LibSQL).
	TableSuffix() string

	// SystemTableSQL returns the CREATE TABLE IF NOT EXISTS DDL for all _kora_*
	// system tables. Each dialect provides its own version.
	SystemTableSQL() []string
}

// ---------------------------------------------------------------------------
// Schema introspection types
// ---------------------------------------------------------------------------

// LiveSchema represents the current state of the database as discovered by
// the Dialect's LoadSchema method.
type LiveSchema struct {
	Tables map[string]*LiveTable
}

// LiveTable represents a database table discovered at runtime.
type LiveTable struct {
	Name    string
	Columns []LiveColumn
	Indexes []LiveIndex
}

// LiveColumn represents a column discovered from the live database.
type LiveColumn struct {
	Name         string
	Type         string // raw column type as reported by the DB
	IsNullable   bool
	DefaultValue string
}

// LiveIndex represents an index discovered from the live database.
type LiveIndex struct {
	Name     string
	Columns  []string
	IsUnique bool
}

// ---------------------------------------------------------------------------
// Database configuration
// ---------------------------------------------------------------------------

// DBConfig holds the connection parameters for any database.
// It replaces the MySQL-specific DSN construction in site/site.go.
type DBConfig struct {
	Type     string // "mysql", "libsql", "postgres"
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	// LibSQL-specific: URL for remote connection (e.g., "libsql://db.turso.io")
	URL string
	// Additional parameters appended to the DSN.
	Params map[string]string
}

// Resolve returns the Dialect for the given database type.
// Default is "mysql" for backward compatibility.
//
//	KORA_DB_TYPE=mysql    → MySQL dialect (default)
//	KORA_DB_TYPE=libsql   → LibSQL dialect
//	KORA_DB_TYPE=postgres → PostgreSQL dialect (future)
func Resolve(dbType string) Dialect {
	switch dbType {
	case "libsql":
		return &LibSQLDialect{}
	case "postgres":
		// return &PostgresDialect{}
		panic("postgres dialect not yet implemented")
	default:
		return &MySQLDialect{}
	}
}

// MySQL returns the MySQL dialect (for callers that don't have DBType info).
func MySQL() Dialect { return &MySQLDialect{} }

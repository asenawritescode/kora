// Package db provides database abstraction via a Dialect interface.
// Implementations for MySQL, LibSQL, and PostgreSQL live in separate files.
// The Dialect is selected at startup via the KORA_DB_TYPE env var.
package db

import (
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
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

// SearchIndexDialect may provide a non-unique index compatible with the
// dialect's prefix-search predicates. Unique constraints remain separate and
// keep their existing comparison rules.
type SearchIndexDialect interface {
	CreateSearchIndex(tableName, fieldName string) string
	SearchIndexName(tableName, fieldName string) string
}

// NullableColumnDialect relaxes an existing NOT NULL column to accept NULL,
// keeping its type and default, for a field that is no longer required.
// liveType is the column type the database reports; empty means the field's
// own column type.
type NullableColumnDialect interface {
	DropNotNull(tableName string, f *doctype.Field, liveType string) string
}

// NumberBlockRow is one reserved range of a naming series, stored in _kora_number_block.
type NumberBlockRow struct {
	Doctype string
	Prefix  string
	Owner   string
	State   string
	First   int64
	Last    int64
}

// QueryDialect is the DML subset (insert/update/select helpers).
type QueryDialect interface {
	UpsertClause(conflictCols []string, updateCols []string) string
	UpsertIncrement(conflictCols []string, incrementCols []string) string
	InsertOrIgnorePrefix() string
	// SharedRowLock is the suffix that makes a read take a shared row lock held
	// to the end of the transaction: writers may share it, UPDATE waits for it.
	// It is "" where the engine has no row locks (LibSQL); callers then take a
	// write lock another way.
	SharedRowLock() string
	AllocateNameNumber(tx *sql.Tx, doctypeName, prefix, tableName string) (int64, error)
	// AllocateSeriesNumber atomically increments the counter for one naming
	// series key. seed is called only when the counter row does not exist yet;
	// it returns the highest number already stored, and the first issued
	// number is that value plus one. All values are bound.
	AllocateSeriesNumber(tx *sql.Tx, doctypeName, key string, seed func() (int64, error)) (int64, error)
	// ReserveSeriesRange moves the counter for one series key forward by size
	// and returns the inclusive first and last numbers of the range it took.
	// Concurrent callers get disjoint ranges. seed is used as above.
	ReserveSeriesRange(tx *sql.Tx, doctypeName, key string, size int64, seed func() (int64, error)) (first, last int64, err error)
	// RecordNumberBlock inserts one reserved range into _kora_number_block.
	RecordNumberBlock(tx *sql.Tx, row NumberBlockRow) error
	// OpenNumberBlocks returns the open ranges of one doctype reserved for owner.
	OpenNumberBlocks(tx *sql.Tx, doctypeName, owner string) ([]NumberBlockRow, error)
	// NameGenQuery returns the highest PREFIX-NNNN number in tableName. The
	// single placeholder takes the LIKE pattern "PREFIX-%".
	NameGenQuery(tableName string) string
	Placeholder(n int) string
	NowTimestamp() string
	// NowMinusSeconds is the database clock minus one bound seconds placeholder.
	NowMinusSeconds() string
}

// BoolParam returns the bound representation used by Kora's boolean-like SQL
// columns. PostgreSQL's system tables store several of these flags as SMALLINT
// for compatibility with existing schemas; MySQL and LibSQL accept bool.
func BoolParam(dialect QueryDialect, value bool) any {
	named, ok := dialect.(interface{ DriverName() string })
	if ok && named.DriverName() == "postgres" {
		if value {
			return int64(1)
		}
		return int64(0)
	}
	return value
}

// TableQualifiedIncrementDialect provides an incrementing upsert expression
// that can qualify the existing target row. PostgreSQL needs this when the
// inserted and existing rows both expose the incremented column name.
type TableQualifiedIncrementDialect interface {
	UpsertIncrementForTable(tableName string, conflictCols []string, incrementCols []string) string
}

// ErrorDialect is the error-parsing subset.
type ErrorDialect interface {
	ParseError(err error, dt *doctype.DocType) *doctype.ValidationError
	// IsWriteConflict identifies database-native optimistic/serialization
	// conflicts so callers can preserve a stable contract without inspecting
	// driver error strings outside the database boundary.
	IsWriteConflict(err error) bool
	// IsAbortedTransaction reports SQLSTATE 25P02: a statement ran after an earlier
	// statement in the same transaction failed. It is not a dependency outage.
	IsAbortedTransaction(err error) bool
	// IsUniqueViolation reports a unique-key conflict on insert, so a caller that
	// raced another writer can read the row the winner stored.
	IsUniqueViolation(err error) bool
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

	// ExecuteBatch runs multiple DDL statements as a single atomic operation.
	// For LibSQL: joins statements with semicolons for a single HTTP round-trip.
	// For MySQL: wraps in a transaction.
	// For embedded SQLite: wraps in a transaction.
	ExecuteBatch(db *sql.DB, statements []string) error

	// DatabaseSizeQuery returns a query yielding one BIGINT row: the current
	// database's data and index size in bytes.
	DatabaseSizeQuery() string
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
//	KORA_DB_TYPE=postgres → PostgreSQL dialect
func Resolve(dbType string) Dialect {
	switch dbType {
	case "libsql":
		return &LibSQLDialect{}
	case "postgres":
		return &PostgresDialect{}
	default:
		return &MySQLDialect{}
	}
}

// MySQL returns the MySQL dialect (for callers that don't have DBType info).
func MySQL() Dialect { return &MySQLDialect{} }

// scanNumberBlocks reads rows selected as doctype, prefix, owner, state, first and last.
func scanNumberBlocks(rows *sql.Rows, err error) ([]NumberBlockRow, error) {
	if err != nil {
		return nil, fmt.Errorf("reading number blocks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []NumberBlockRow
	for rows.Next() {
		var r NumberBlockRow
		if err := rows.Scan(&r.Doctype, &r.Prefix, &r.Owner, &r.State, &r.First, &r.Last); err != nil {
			return nil, fmt.Errorf("scanning number block: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

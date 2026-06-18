# DB Compatibility Skill

Ensures all SQL in the Kora codebase works across MySQL and LibSQL (SQLite via Turso).
Invoke this skill when writing new SQL, reviewing PRs that touch database code, or debugging
database errors that differ between MySQL and LibSQL.

## Rule: All SQL goes through the Dialect

The `db.Dialect` interface is the single source of truth for database-specific SQL.
**Never write raw SQL with database-specific syntax directly.** If a dialect method doesn't
exist for what you need, add it to the interface and both implementations — don't hardcode.

### Dialect at a glance

```
db.Resolve(dbType) → Dialect
├── MySQLDialect  (KORA_DB_TYPE=mysql or default)
└── LibSQLDialect (KORA_DB_TYPE=libsql)
```

## Compatibility Checklist

When writing or reviewing SQL, check every item:

### 1. UPSERT — use `dialect.UpsertClause()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `INSERT INTO t ... ON DUPLICATE KEY UPDATE col = VALUES(col)` | `fmt.Sprintf("INSERT INTO t ... %s", dialect.UpsertClause(conflictCols, updateCols))` |
| `INSERT INTO t ... ON CONFLICT(col) DO UPDATE SET col = excluded.col` | Same — dialect handles both |

**MySQL output**: `ON DUPLICATE KEY UPDATE col = VALUES(col)`  
**LibSQL output**: `ON CONFLICT(col) DO UPDATE SET col = excluded.col`

Files that had this bug: `secret/secret.go`, `configstore/store.go`.

### 2. INSERT OR IGNORE — use `dialect.InsertOrIgnorePrefix()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `INSERT IGNORE INTO t ...` | `fmt.Sprintf("%s INTO t ...", dialect.InsertOrIgnorePrefix())` |
| `INSERT OR IGNORE INTO t ...` | Same — dialect handles both |

### 3. Identifier quoting — use `dialect.QuoteIdent()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `` `table_name` `` (backticks) | `dialect.QuoteIdent("table_name")` |
| `"table_name"` (double quotes) | Same — dialect handles both |

**MySQL output**: `` `table_name` ``  
**LibSQL output**: `"table_name"`

Files that had this bug: `schema/migrator.go` (`generateAddColumn`).

**⚠️ Critical: Never pre-quote a name before passing it to `QuoteIdent()`.**

`doctype.DocType.TableName()` returns a **backtick-quoted** name (e.g. `` `tabCustomer` ``) —
it's designed for raw interpolation into ORM SQL strings. Do NOT pass it through
`QuoteIdent()` in DDL code — that wraps it again, producing invalid SQL like
`"`tabCustomer`"` (LibSQL) or `` `` `tabCustomer` `` `` (MySQL).

| ❌ Wrong | ✅ Right |
|----------|---------|
| `d.QuoteIdent(dt.TableName())` | `d.QuoteIdent(dt.RawTableName())` |
| `d.CreateIndex(dt.TableName(), ...)` | `d.CreateIndex(dt.RawTableName(), ...)` |

**Rule**: If a value will pass through `QuoteIdent()`, use `RawTableName()` / `RawChildTableName()`.
If it will be interpolated directly into SQL, use `TableName()` / `ChildTableName()` (which include
the quoting).

### 4. DDL column types — use `dialect.ColumnType()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `VARCHAR(140)`, `BIGINT`, `TINYINT(1)` | `dialect.ColumnType(&field)` |

Key type differences:
| MySQL | LibSQL |
|-------|--------|
| `VARCHAR(140)` | `TEXT` |
| `BIGINT` | `INTEGER` |
| `DECIMAL(21,9)` | `REAL` |
| `TINYINT(1)` | `INTEGER` |
| `DATETIME(6)` | `TEXT` |
| `JSON` | `TEXT` |

### 5. System columns — use `dialect.SystemColumnDDL()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)` | `dialect.SystemColumnDDL()` |
| `modified DATETIME(6) ... ON UPDATE CURRENT_TIMESTAMP(6)` | Same |

**MySQL output**:
```sql
creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
```

**LibSQL output**:
```sql
"creation" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))
"modified" TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))
```

Key difference: SQLite has no `ON UPDATE` — LibSQL's `CreateTable()` adds an `AFTER UPDATE` trigger for the `modified` column. When using `SystemColumnDDL()` the trigger must be handled separately via `dialect.CreateTable()`.

**Note on trigger generation**: The trigger is returned as a **separate element** in the `[]string`
returned by `CreateTable()` — never concatenated with the `CREATE TABLE` DDL into a single string.
LibSQL/SQLite drivers do not support multiple SQL statements in one `db.Exec()` call.

### 6. Table suffix — use `dialect.TableSuffix()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci` | `dialect.TableSuffix()` |

**MySQL output**: `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`  
**LibSQL output**: `""` (empty — SQLite has no table options)

### 7. Indexes — use `dialect.CreateIndex()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `INDEX idx_name (col)` inline in CREATE TABLE | `dialect.CreateIndex(table, col, unique)` as separate statement |
| `CREATE UNIQUE INDEX ... ON \`t\` (...)` | Same — dialect handles quoting and syntax |

**Note**: Inline index definitions (`INDEX ...` inside CREATE TABLE body) are MySQL-specific.
SQLite doesn't support them. Always use standalone `CREATE INDEX` statements.

### 8. Timestamps — use `dialect.NowTimestamp()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `CURRENT_TIMESTAMP(6)` | `dialect.NowTimestamp()` |

**MySQL output**: `CURRENT_TIMESTAMP(6)`  
**LibSQL output**: `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`

### 9. Error parsing — use `dialect.ParseError()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `if mysqlErr, ok := err.(*mysql.MySQLError) ...` | `dialect.ParseError(err, dt)` |

MySQL uses error codes (1062 = duplicate, 1364/1048 = not null).
LibSQL uses string matching in error messages ("UNIQUE constraint failed", "NOT NULL constraint failed").

### 10. Schema introspection — use `dialect.LoadSchema()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `SELECT ... FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ?` | `dialect.LoadSchema(db, dbName)` |
| `PRAGMA table_info(...)` | Same — dialect handles both |

### 11. Name generation — use `dialect.NameGenQuery()`

| ❌ Wrong | ✅ Right |
|----------|---------|
| `SUBSTRING_INDEX(name, '-', -1)` | `dialect.NameGenQuery(tableName, prefix)` |

**MySQL**: Uses `SUBSTRING_INDEX` + `CAST AS UNSIGNED`  
**LibSQL**: Uses `SUBSTR` + `INSTR` + `CAST AS INTEGER`

### 12. System tables — use `dialect.SystemTableSQL()` + `BootstrapSystemTables()`

Never write `CREATE TABLE IF NOT EXISTS _kora_*` directly in application code.
Use `site.BootstrapSystemTables(db, dialect)` which iterates `dialect.SystemTableSQL()`.
This ensures all 11 system tables are created with correct types per database.

## File-specific rules

### `secret/secret.go`
- `EnsureTable()` uses portable SQL (no `DATETIME(6)`, no `ENGINE=InnoDB`)
- `Set()` uses SELECT-then-UPDATE-or-INSERT, not dialect-specific upsert
- Reason: Store doesn't hold a Dialect reference (by design — minimal dependency)

### `configstore/store.go`
- Already holds `Dialect` via `Store.Dialect`
- **Every SQL statement must use the dialect** — `UpsertClause()`, `QuoteIdent()`, etc.
- When adding new methods to Store, check: does this SQL work on both DBs?

### `schema/migrator.go`
- Receives `dialect db.Dialect` as parameter
- Use `dialect.SystemColumnDDL()`, `dialect.ChildColumnDDL()`, `dialect.TableSuffix()`
- Use `dialect.ColumnType()` for data columns
- Use `dialect.CreateIndex()` for indexes (standalone, not inline)
- Use `dialect.QuoteIdent()` for all identifier quoting

### `orm/query.go`
- Uses `d.Dialect.UpsertClause()` and `d.Dialect.Placeholder()` — already correct

## Common anti-patterns

### "Multi-statement Exec" trap

LibSQL/SQLite drivers do **not** support multiple SQL statements in a single `db.Exec()` call.
If you concatenate two statements with `\n` and pass them to `db.Exec()`, it will fail with:

```
SQL string could not be parsed: near CREATE, "None": syntax error
```

| ❌ Wrong | ✅ Right |
|----------|---------|
| `db.Exec("CREATE TABLE t (...)\nCREATE TRIGGER ...")` | Two separate `db.Exec()` calls |
| `CreateTable()` returning `string` with `\n` joining multiple statements | `CreateTable()` returning `[]string` — each element executed separately |

**Rule**: Each element in the `[]string` returned by `GenerateDDL()` must be exactly **one**
executable SQL statement. DDL methods that need to emit multiple statements (e.g.,
`CREATE TABLE` + `CREATE TRIGGER`) must return them as separate slice elements.

### "It's just one query" trap
Even a single hardcoded SQL statement can break the other database.
Every `db.Exec()` or `db.Query()` call is a potential compatibility issue.

### "I'll fix it later" trap
LibSQL errors surface at runtime (DDL errors on table creation, syntax errors on upsert).
They're not caught at compile time. Test on both databases.

### "It works on my machine" trap
MySQL is the default. If you only test with MySQL, LibSQL paths silently rot.
The `KORA_DB_TYPE` env var controls which dialect is used.

## When to add to the Dialect interface

If you need SQL that differs between MySQL and LibSQL:

1. Add the method signature to `db/dialect.go` interface
2. Implement in `db/dialect_mysql.go`
3. Implement in `db/dialect_libsql.go`
4. Use through the dialect in your code

Do NOT:
- Type-switch on dialect type (`switch dialect.(type) { case *MySQLDialect: ... }`)
- Check `DBType` string and write different SQL per branch
- Write two separate SQL strings and pick one

## Quick audit command

To find hardcoded MySQL-specific SQL anywhere in the codebase:

```bash
grep -rn 'ON DUPLICATE KEY UPDATE\|ENGINE=InnoDB\|CHARSET=\|COLLATE=\|DATETIME(\|CURRENT_TIMESTAMP(\|AUTO_INCREMENT' \
  --include="*.go" . \
  | grep -v dialect_mysql.go \
  | grep -v _test.go \
  | grep -v node_modules
```

Any hit in a non-dialect file is a compatibility bug.

## Runtime behavior differences

These aren't caught by the Dialect but matter for correctness:

### Foreign keys — OFF by default in SQLite

```go
// Required per-connection in LibSQL:
db.Exec("PRAGMA foreign_keys = ON")
```

MySQL/InnoDB enforces FKs by default. SQLite parses FK constraints but silently ignores
them unless this PRAGMA is set on every connection. Bootstrap code should set this.

### PRIMARY KEY allows NULL in SQLite

```sql
-- Works in SQLite, fails in MySQL:
INSERT INTO t (id) VALUES (NULL);  -- id is INTEGER PRIMARY KEY
```

Always add `NOT NULL` to PK columns explicitly.

### Single-writer model (SQLITE_BUSY)

SQLite serializes all writes. Concurrent writes get `SQLITE_BUSY`.
MySQL uses row-level locking and handles concurrency transparently.

- LibSQL connections should use WAL mode journal
- Write-heavy code paths need retry logic with exponential backoff
- `SetMaxOpenConns(1)` is a deliberate choice for LibSQL (avoids connection pool contention)

### DDL in transactions

| MySQL | SQLite |
|-------|--------|
| DDL does implicit COMMIT — not transactional | DDL **is fully transactional** — can be rolled back |

This is an advantage for LibSQL (schema changes are atomic), but code that relies
on MySQL's implicit-commit behavior may need adjustment.

### Nested transactions

```sql
BEGIN;
  INSERT INTO t ...
  BEGIN;  -- MySQL: commits outer tx silently. SQLite: ERROR.
```

Use `SAVEPOINT`/`RELEASE` for nested transactions — works on both.

### Expression defaults must be parenthesized in SQLite

```sql
-- MySQL: both work
DEFAULT CURRENT_TIMESTAMP
DEFAULT (CURRENT_TIMESTAMP)

-- SQLite: only parenthesized form is valid
DEFAULT (CURRENT_TIMESTAMP)
```

### String functions that differ

| MySQL | Portable alternative |
|-------|---------------------|
| `IF(cond, a, b)` | `CASE WHEN cond THEN a ELSE b END` |
| `CONCAT(a, b)` | `a \|\| b` (standard SQL) |
| `IFNULL(a, b)` | `COALESCE(a, b)` (standard SQL) |
| `SUBSTRING_INDEX(...)` | Use Go-side string manipulation or dialect method |

### LibSQL-specific capabilities

LibSQL extends SQLite with `ALTER TABLE ... ALTER COLUMN ... TO ...` for type/constraint
changes — use `dialect.AlterColumn()` which respects this.

### STRICT tables

SQLite 3.37+ supports `STRICT` tables that enforce rigid typing like MySQL:
```sql
CREATE TABLE t (id INT, name TEXT) STRICT;
```
Consider for new tables to catch type errors early. MySQL ignores the `STRICT` keyword
(unknown table option — harmless).

## Testing both databases

```bash
# Test with MySQL (default)
go test ./...

# Test with LibSQL — use a local file
KORA_DB_TYPE=libsql DB_DSN="file:test.db?mode=memory" go test ./...
```

Both should pass. If they don't, there's a compatibility bug.

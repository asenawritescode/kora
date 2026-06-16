// Package schema manages database schema migration.
// It compares the DocType registry against the live database schema
// and generates/applies DDL to make them match.
package schema

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// ColumnInfo represents a column in the live database schema.
type ColumnInfo struct {
	Name     string
	Type     string
	Nullable bool
	Default  sql.NullString
	Indexed  bool
}

// TableInfo represents a table in the live database schema.
type TableInfo struct {
	Name    string
	Columns map[string]*ColumnInfo
}

// Diff represents the difference between the registry and the live schema.
type Diff struct {
	NewTables     []string                  // Tables to CREATE
	NewColumns    map[string][]ColumnAdd     // Table → columns to ADD
	NewIndexes    map[string][]IndexAdd      // Table → indexes to CREATE
	RenameColumns map[string][]ColumnRename  // Table → columns to RENAME
	Orphaned      []OrphanedColumn           // Columns in DB but not in registry
}

// ColumnAdd describes a column to be added to an existing table.
type ColumnAdd struct {
	Name     string
	Type     string
	Nullable bool
	Default  string
}

// ColumnRename describes a column to be renamed (from renamed_from).
type ColumnRename struct {
	OldName string
	NewName string
}

// IndexAdd describes an index to be created.
type IndexAdd struct {
	Table   string
	Columns []string
	Unique  bool
}

// OrphanedColumn is a column that exists in the database but not in the registry.
type OrphanedColumn struct {
	Table  string
	Column string
}

// LoadLiveSchema reads the current database schema using the dialect.
// Delegates to db.Dialect.LoadSchema and converts to the local TableInfo format.
func LoadLiveSchema(database *sql.DB, dbName string, dialect db.Dialect) (map[string]*TableInfo, error) {
	liveSchema, err := dialect.LoadSchema(database, dbName)
	if err != nil {
		return nil, fmt.Errorf("loading schema: %w", err)
	}

	tables := make(map[string]*TableInfo)
	for _, lt := range liveSchema.Tables {
		cols := make(map[string]*ColumnInfo)
		for _, lc := range lt.Columns {
			cols[lc.Name] = &ColumnInfo{
				Name:     lc.Name,
				Type:     lc.Type,
				Nullable: lc.IsNullable,
				Default:  sql.NullString{String: lc.DefaultValue, Valid: lc.DefaultValue != ""},
			}
		}
		// Mark indexed columns.
		for _, idx := range lt.Indexes {
			for _, idxCol := range idx.Columns {
				if col, ok := cols[idxCol]; ok {
					col.Indexed = true
				}
			}
		}
		tables[lt.Name] = &TableInfo{Name: lt.Name, Columns: cols}
	}

	return tables, nil
}

// ComputeDiff// ComputeDiff compares the registry DocTypes against the live database schema
// and produces a Diff of changes needed.
func ComputeDiff(registry *doctype.Registry, liveSchema map[string]*TableInfo, dialect db.Dialect) *Diff {
	diff := &Diff{
		NewColumns:    make(map[string][]ColumnAdd),
		NewIndexes:    make(map[string][]IndexAdd),
		RenameColumns: make(map[string][]ColumnRename),
	}

	for _, dt := range registry.All() {
		tableName := dt.RawTableName()
		liveTable, exists := liveSchema[tableName]

		if !exists {
			// Table doesn't exist — needs to be created.
			diff.NewTables = append(diff.NewTables, tableName)
			continue
		}

		// Check for new columns. Handle renames (renamed_from).
		for _, field := range dt.DataFields() {
			if field.Fieldtype == "Table" {
				// Table fields are handled as separate child tables.
				continue
			}
			if _, exists := liveTable.Columns[field.Fieldname]; !exists {
				// If the field has renamed_from and the old column exists, use RENAME instead of ADD.
				if field.RenamedFrom != "" {
					if _, oldExists := liveTable.Columns[field.RenamedFrom]; oldExists {
						diff.RenameColumns[tableName] = append(diff.RenameColumns[tableName], ColumnRename{
							OldName: field.RenamedFrom,
							NewName: field.Fieldname,
						})
						continue
					}
				}
				colAdd := ColumnAdd{
					Name:     field.Fieldname,
					Type:     dialect.ColumnType(&field),
					Nullable: !field.Reqd,
				}
				if field.Default != "" {
					colAdd.Default = field.Default
				}
				diff.NewColumns[tableName] = append(diff.NewColumns[tableName], colAdd)
			}
		}

		// Check for new indexes.
		for _, field := range dt.DataFields() {
			if field.SearchIndex && field.IsDataField() {
				if col, exists := liveTable.Columns[field.Fieldname]; exists && !col.Indexed {
					diff.NewIndexes[tableName] = append(diff.NewIndexes[tableName], IndexAdd{
						Table:   tableName,
						Columns: []string{field.Fieldname},
						Unique:  field.Unique,
					})
				}
			}
		}

		// Detect orphaned columns (columns in DB but not in registry).
		registryFields := make(map[string]bool)
		for _, f := range dt.DataFields() {
			registryFields[f.Fieldname] = true
		}
		// Add system columns.
		for _, sc := range doctype.SystemColumns() {
			registryFields[sc.Name] = true
		}

		for colName := range liveTable.Columns {
			if !registryFields[colName] {
				diff.Orphaned = append(diff.Orphaned, OrphanedColumn{
					Table:  tableName,
					Column: colName,
				})
			}
		}
	}

	// Check for child tables (separate from main loop — applies even when parent is new).
	for _, dt := range registry.All() {
		for _, field := range dt.TableFields() {
			childTableName := dt.RawChildTableName(field.Fieldname)
			if _, exists := liveSchema[childTableName]; !exists {
				// Avoid duplicate entries.
				found := false
				for _, nt := range diff.NewTables {
					if nt == childTableName {
						found = true
						break
					}
				}
				if !found {
					diff.NewTables = append(diff.NewTables, childTableName)
				}
			}
		}
	}

	return diff
}

// IsEmpty returns true if there are no changes to apply.
func (d *Diff) IsEmpty() bool {
	return len(d.NewTables) == 0 && len(d.NewColumns) == 0 && len(d.NewIndexes) == 0 && len(d.RenameColumns) == 0
}

// GenerateDDL produces the SQL statements to apply the diff.
func (d *Diff) GenerateDDL(registry *doctype.Registry, dialect db.Dialect) []string {
	var statements []string

	// CREATE TABLE statements.
	for _, tableName := range d.NewTables {
		// Find the DocType for this table and use dialect DDL.
		var stmt string
		var foundDT *doctype.DocType
		for _, dt := range registry.All() {
			if dt.RawTableName() == tableName {
				foundDT = dt
				break
			}
		}
		if foundDT != nil {
			stmt = dialect.CreateTable(foundDT)
		} else {
			stmt = generateCreateTable(tableName, registry, dialect)
		}
		statements = append(statements, stmt)
	}

	// ALTER TABLE ADD COLUMN statements.
	for tableName, cols := range d.NewColumns {
		for _, col := range cols {
			var stmt string
			var foundF *doctype.Field
			for _, dt := range registry.All() {
				if dt.RawTableName() == tableName {
					if f := dt.GetField(col.Name); f != nil {
						foundF = f
					}
					break
				}
			}
			if foundF != nil {
				stmt = dialect.AddColumn(dialect.QuoteIdent(tableName), foundF)
			} else {
				stmt = generateAddColumn(tableName, col, dialect)
			}
			statements = append(statements, stmt)
		}
	}

	// ALTER TABLE RENAME COLUMN statements (from renamed_from).
	for tableName, renames := range d.RenameColumns {
		for _, r := range renames {
			stmt := dialect.RenameColumn(tableName, r.OldName, r.NewName)
			statements = append(statements, stmt)
		}
	}

	// CREATE INDEX statements.
	for tableName, idxs := range d.NewIndexes {
		for _, idx := range idxs {
			stmt := dialect.CreateIndex(tableName, idx.Columns[0], idx.Unique)
			statements = append(statements, stmt)
		}
	}

	return statements
}

func generateCreateTable(tableName string, registry *doctype.Registry, dialect db.Dialect) string {
	// Determine if this is a regular table or a child table.
	var dt *doctype.DocType
	var isChild bool
	var parentDT *doctype.DocType
	var parentField string

	for _, d := range registry.All() {
		if d.RawTableName() == tableName {
			dt = d
			break
		}
		// Check child tables.
		for _, f := range d.TableFields() {
			if d.RawChildTableName(f.Fieldname) == tableName {
				isChild = true
				parentDT = d
				parentField = f.Fieldname
				dt = registry.Get(f.Options)
				break
			}
		}
		if dt != nil {
			break
		}
	}

	// Use quoted table name for SQL.
	sqlTableName := dialect.QuoteIdent(tableName)

	var cols []string

	// System columns first.
	cols = append(cols, "name VARCHAR(140) NOT NULL")
	cols = append(cols, "owner VARCHAR(140) NOT NULL DEFAULT ''")
	cols = append(cols, "creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)")
	cols = append(cols, "modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)")
	cols = append(cols, "modified_by VARCHAR(140) NOT NULL DEFAULT ''")
	cols = append(cols, "doc_status TINYINT(1) NOT NULL DEFAULT 0")
	cols = append(cols, "idx INT NOT NULL DEFAULT 0")

	// Child table system columns.
	if isChild {
		_ = parentDT
		_ = parentField
		cols = append(cols, "parent VARCHAR(140) NOT NULL DEFAULT ''")
		cols = append(cols, "parentfield VARCHAR(140) NOT NULL DEFAULT ''")
		cols = append(cols, "parenttype VARCHAR(140) NOT NULL DEFAULT ''")
	}

	// Data columns from the DocType.
	if dt != nil {
		for _, f := range dt.DataFields() {
			if f.Fieldtype == "Table" {
				continue
			}
			dbType := dialect.ColumnType(&f)
			if dbType == "" {
				continue
			}
			nullable := ""
			if !f.Reqd {
				nullable = " DEFAULT NULL"
			} else {
				nullable = " NOT NULL"
			}
			if f.Default != "" && f.Reqd {
				nullable = fmt.Sprintf(" NOT NULL DEFAULT '%s'", escapeSQL(f.Default))
			} else if f.Default != "" {
				nullable = fmt.Sprintf(" DEFAULT '%s'", escapeSQL(f.Default))
			}
			cols = append(cols, fmt.Sprintf("%s %s%s", f.Fieldname, dbType, nullable))
		}
	}

	// Primary key.
	cols = append(cols, "PRIMARY KEY (name)")

	// Indexes.
	if dt != nil {
		for _, f := range dt.DataFields() {
			if f.SearchIndex {
				idxCols := []string{f.Fieldname}
				cols = append(cols, fmt.Sprintf("INDEX idx_%s (%s)", f.Fieldname, strings.Join(idxCols, ", ")))
			}
			if f.Unique {
				cols = append(cols, fmt.Sprintf("UNIQUE KEY uq_%s (%s)", f.Fieldname, f.Fieldname))
			}
		}
	}

	// Child table indexes.
	if isChild {
		cols = append(cols, "INDEX idx_parent (parent)")
	}

	return fmt.Sprintf("CREATE TABLE %s (\n  %s\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		sqlTableName, strings.Join(cols, ",\n  "))
}

func generateAddColumn(tableName string, col ColumnAdd, dialect db.Dialect) string {
	nullable := ""
	if !col.Nullable {
		nullable = " NOT NULL"
	}
	if col.Default != "" {
		if col.Nullable {
			nullable = fmt.Sprintf(" DEFAULT '%s'", escapeSQL(col.Default))
		} else {
			nullable = fmt.Sprintf(" NOT NULL DEFAULT '%s'", escapeSQL(col.Default))
		}
	}
	return fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN %s %s%s", tableName, col.Name, col.Type, nullable)
}

func generateCreateIndex(tableName string, idx IndexAdd) string {
	uniqueStr := ""
	if idx.Unique {
		uniqueStr = "UNIQUE "
	}
	colStr := strings.Join(idx.Columns, ", ")
	return fmt.Sprintf("CREATE %sINDEX idx_%s ON `%s` (%s)", uniqueStr, strings.Join(idx.Columns, "_"), tableName, colStr)
}

func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ApplyDDL executes the DDL statements against the database.
func ApplyDDL(db *sql.DB, statements []string) error {
	for _, stmt := range statements {
		slog.Debug("applying DDL", "sql", stmt)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("executing DDL: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

// MigrateSite computes the schema diff for a site and applies it.
func MigrateSite(database *sql.DB, dbName string, registry *doctype.Registry, dialect db.Dialect) error {
	liveSchema, err := LoadLiveSchema(database, dbName, dialect)
	if err != nil {
		return fmt.Errorf("loading live schema: %w", err)
	}

	diff := ComputeDiff(registry, liveSchema, dialect)
	if diff.IsEmpty() {
		slog.Info("schema is up to date, no migrations needed")
		return nil
	}

	slog.Info("schema diff computed",
		"new_tables", len(diff.NewTables),
		"new_columns", len(diff.NewColumns),
		"new_indexes", len(diff.NewIndexes),
		"renamed_columns", len(diff.RenameColumns),
		"orphaned_columns", len(diff.Orphaned),
	)

	ddl := diff.GenerateDDL(registry, dialect)
	for _, stmt := range ddl {
		slog.Info("DDL", "sql", stmt)
	}

	return ApplyDDL(database, ddl)
}

// TieredChange describes a single schema change with its safety classification.
type TieredChange struct {
	Tier    string `json:"tier"`    // "safe", "warning", "blocked"
	DocType string `json:"doctype"`
	Field   string `json:"field,omitempty"`
	Change  string `json:"change"`
	Rows    int    `json:"rows"`
	DDL     string `json:"ddl,omitempty"`
	Message string `json:"message"`
}

// ActivationPreview contains the full impact analysis for activating a config change.
type ActivationPreview struct {
	DDL      []string       `json:"ddl"`
	Changes  []TieredChange `json:"changes"`
	Blocked  []TieredChange `json:"blocked"`
	Warnings []TieredChange `json:"warnings"`
}

// AnalyzeImpact compares a proposed doctype against the existing one (if any)
// and classifies each change into safety tiers. It also counts affected rows.
func AnalyzeImpact(database *sql.DB, oldDT, newDT *doctype.DocType, reg *doctype.Registry, dialect db.Dialect) *ActivationPreview {
	preview := &ActivationPreview{}

	if oldDT == nil {
		// New doctype — always safe.
		tableName := newDT.RawTableName()
		preview.Changes = append(preview.Changes, TieredChange{
			Tier:    "safe",
			DocType: newDT.Name,
			Change:  "Create new doctype",
			Message: "New table will be created",
		})
		ddl := generateCreateTable(tableName, reg, dialect)
		preview.DDL = append(preview.DDL, ddl)

		// Also check child tables.
		for _, f := range newDT.TableFields() {
			childDT := reg.Get(f.Options)
			if childDT != nil {
				childTableName := newDT.RawChildTableName(f.Fieldname)
				childDDL := generateCreateTable(childTableName, reg, dialect)
				preview.DDL = append(preview.DDL, childDDL)
			}
		}
		return preview
	}

	// Count existing rows.
	var rowCount int
	tableName := oldDT.RawTableName()
	query := "SELECT COUNT(*) FROM `" + tableName + "`"
	_ = database.QueryRow(query).Scan(&rowCount)

	// Build field maps.
	oldFields := make(map[string]*doctype.Field)
	newFields := make(map[string]*doctype.Field)
	for i := range oldDT.Fields {
		oldFields[oldDT.Fields[i].Fieldname] = &oldDT.Fields[i]
	}
	for i := range newDT.Fields {
		newFields[newDT.Fields[i].Fieldname] = &newDT.Fields[i]
	}

	// Detect added fields.
	for name, f := range newFields {
		if _, exists := oldFields[name]; !exists {
			change := TieredChange{
				DocType: oldDT.Name,
				Field:   name,
				Change:  fmt.Sprintf("Add field %s (%s)", name, f.Fieldtype),
				Rows:    rowCount,
			}
			if f.Reqd && f.Default == "" {
				change.Tier = "warning"
				change.Message = fmt.Sprintf("Required field with no default — %d existing rows will get empty values", rowCount)
			} else {
				change.Tier = "safe"
				if f.Default != "" {
					change.Message = fmt.Sprintf("%d existing rows backfilled with default '%s'", rowCount, f.Default)
				} else {
					change.Message = fmt.Sprintf("%d existing rows get NULL", rowCount)
				}
			}
			ddl := generateAddColumn(tableName, ColumnAdd{
				Name:     f.Fieldname,
				Type:     dialect.ColumnType(f),
				Nullable: !f.Reqd,
				Default:  f.Default,
			}, dialect)
			change.DDL = ddl
			preview.DDL = append(preview.DDL, ddl)
			preview.Changes = append(preview.Changes, change)
		}
	}

	// Detect removed/orphaned fields.
	for name, f := range oldFields {
		if _, exists := newFields[name]; !exists {
			change := TieredChange{
				Tier:    "warning",
				DocType: oldDT.Name,
				Field:   name,
				Change:  fmt.Sprintf("Remove field %s (%s)", name, f.Fieldtype),
				Rows:    rowCount,
				Message: fmt.Sprintf("Column '%s' will be orphaned — %d rows of data preserved but hidden", name, rowCount),
			}
			preview.Changes = append(preview.Changes, change)
		}
	}

	// Detect type changes.
	for name, newF := range newFields {
		oldF, exists := oldFields[name]
		if !exists {
			continue
		}
		if oldF.Fieldtype != newF.Fieldtype {
			change := TieredChange{
				Tier:    "blocked",
				DocType: oldDT.Name,
				Field:   name,
				Change:  fmt.Sprintf("Change field type %s → %s", oldF.Fieldtype, newF.Fieldtype),
				Rows:    rowCount,
				Message: fmt.Sprintf("Type change from %s to %s requires data conversion for %d rows", oldF.Fieldtype, newF.Fieldtype, rowCount),
			}
			preview.Changes = append(preview.Changes, change)
		}
		// Detect not-required → required changes.
		if !oldF.Reqd && newF.Reqd {
			change := TieredChange{
				DocType: oldDT.Name,
				Field:   name,
				Change:  "Field is now required",
				Rows:    rowCount,
			}
			if newF.Default != "" {
				change.Tier = "safe"
				change.Message = fmt.Sprintf("%d rows backfilled with default", rowCount)
			} else {
				change.Tier = "warning"
				change.Message = fmt.Sprintf("%d rows may have empty values", rowCount)
			}
			preview.Changes = append(preview.Changes, change)
		}
	}

	// Classify into blocked/warnings.
	for _, c := range preview.Changes {
		switch c.Tier {
		case "blocked":
			preview.Blocked = append(preview.Blocked, c)
		case "warning":
			preview.Warnings = append(preview.Warnings, c)
		}
	}

	return preview
}

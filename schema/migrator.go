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
	Name       string
	Type       string
	Nullable   bool
	Default    sql.NullString
	Indexed    bool
	IndexNames []string // names of indexes covering this column (for old→new name migration)
}

// TableInfo represents a table in the live database schema.
type TableInfo struct {
	Name    string
	Columns map[string]*ColumnInfo
}

// IndexDrop describes an index to be dropped.
type IndexDrop struct {
	Table string
	Name  string
}

// Diff represents the difference between the registry and the live schema.
type Diff struct {
	NewTables     []string                  // Tables to CREATE
	NewColumns    map[string][]ColumnAdd     // Table → columns to ADD
	NewIndexes    map[string][]IndexAdd      // Table → indexes to CREATE
	DropIndexes   []IndexDrop               // Indexes to DROP (old-style names)
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
		// Mark indexed columns and record index names for migration detection.
		for _, idx := range lt.Indexes {
			for _, idxCol := range idx.Columns {
				if col, ok := cols[idxCol]; ok {
					col.Indexed = true
					col.IndexNames = append(col.IndexNames, idx.Name)
				}
			}
		}
		tables[lt.Name] = &TableInfo{Name: lt.Name, Columns: cols}
	}

	return tables, nil
}

// ComputeDiff compares a set of DocTypes against the live database schema
// and produces a Diff of changes needed.
// doctypes is the target config (from the snapshot being activated).
// getDocType looks up a DocType by name (for child table resolution).
func ComputeDiff(doctypes []*doctype.DocType, getDocType func(string) *doctype.DocType, liveSchema map[string]*TableInfo, dialect db.SchemaDialect) *Diff {
	diff := &Diff{
		NewColumns:    make(map[string][]ColumnAdd),
		NewIndexes:    make(map[string][]IndexAdd),
		RenameColumns: make(map[string][]ColumnRename),
	}

	for _, dt := range doctypes {
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

		// Check for new indexes and detect old-style index names that need migration.
		for _, field := range dt.DataFields() {
			if field.SearchIndex && field.IsDataField() {
				col, exists := liveTable.Columns[field.Fieldname]
				if !exists {
					continue
				}

				// Expected new-style names (include table name for global uniqueness).
				expectedIdxName := fmt.Sprintf("idx_%s_%s", tableName, field.Fieldname)
				expectedUqName := fmt.Sprintf("uq_%s_%s", tableName, field.Fieldname)
				// Old-style names (field name only — conflict-prone).
				oldIdxName := fmt.Sprintf("idx_%s", field.Fieldname)
				oldUqName := fmt.Sprintf("uq_%s", field.Fieldname)

				hasNewStyle := false
				var oldStyleName string
				for _, idxName := range col.IndexNames {
					if idxName == expectedIdxName || idxName == expectedUqName {
						hasNewStyle = true
						break
					}
					if idxName == oldIdxName || idxName == oldUqName {
						oldStyleName = idxName
					}
				}

				if !col.Indexed || (!hasNewStyle && oldStyleName != "") {
					// Either the column isn't indexed at all, or it's indexed
					// with an old-style name that needs migration.
					diff.NewIndexes[tableName] = append(diff.NewIndexes[tableName], IndexAdd{
						Table:   tableName,
						Columns: []string{field.Fieldname},
						Unique:  field.Unique,
					})
				}

				// Drop old-style index so it doesn't conflict globally.
				if oldStyleName != "" {
					diff.DropIndexes = append(diff.DropIndexes, IndexDrop{
						Table: tableName,
						Name:  oldStyleName,
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
	for _, dt := range doctypes {
		for _, field := range dt.TableFields() {
			childTableName := dt.RawChildTableName(field.Fieldname)
			childDT := getDocType(field.Options)
			if _, exists := liveSchema[childTableName]; !exists {
				// Child table doesn't exist yet. Only create it if the child
				// doctype is available (otherwise defer until child is activated).
				if childDT == nil {
					slog.Warn("schema: deferring child table creation — child doctype not yet activated",
						"child_table", childTableName, "parent", dt.Name, "field", field.Fieldname)
					continue
				}
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
			} else if childDT != nil {
				// Child table already exists AND child doctype is now available.
				// Detect missing data columns and add them. This recovers from
				// cases where a parent was activated before its child doctype,
				// creating a skeleton child table without data columns.
				existingTable := liveSchema[childTableName]
				for _, cf := range childDT.DataFields() {
					if cf.Fieldtype == "Table" {
						continue
					}
					if _, exists := existingTable.Columns[cf.Fieldname]; !exists {
						diff.NewColumns[childTableName] = append(
							diff.NewColumns[childTableName],
							ColumnAdd{
								Name:     cf.Fieldname,
								Type:     dialect.ColumnType(&cf),
								Nullable: !cf.Reqd,
								Default:  cf.Default,
							},
						)
						slog.Info("schema: backfilling missing column in existing child table",
							"child_table", childTableName,
							"column", cf.Fieldname,
							"child_doctype", childDT.Name)
					}
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
// doctypes is the target config, getDocType looks up child doctypes by name.
func (d *Diff) GenerateDDL(doctypes []*doctype.DocType, getDocType func(string) *doctype.DocType, dialect db.Dialect) []string {
	var statements []string

	// CREATE TABLE statements.
	for _, tableName := range d.NewTables {
		// Find the DocType for this table and use dialect DDL.
		var foundDT *doctype.DocType
		for _, dt := range doctypes {
			if dt.RawTableName() == tableName {
				foundDT = dt
				break
			}
		}
		if foundDT != nil {
			statements = append(statements, dialect.CreateTable(foundDT)...)
		} else {
			ddl := generateCreateTableForDoctypes(tableName, doctypes, getDocType, dialect)
			if ddl != "" {
				statements = append(statements, ddl)
			}
		}
	}

	// ALTER TABLE ADD COLUMN statements.
	for tableName, cols := range d.NewColumns {
		for _, col := range cols {
			var stmt string
			var foundF *doctype.Field
			for _, dt := range doctypes {
				if dt.RawTableName() == tableName {
					if f := dt.GetField(col.Name); f != nil {
						foundF = f
					}
					break
				}
			}
			if foundF != nil {
				stmt = dialect.AddColumn(tableName, foundF)
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

	// Drop deprecated (old-style) indexes before creating replacements.
	// This handles migration from idx_<field> to idx_<table>_<field> naming.
	for _, idx := range d.DropIndexes {
		stmt := dialect.DropIndex(idx.Table, idx.Name)
		statements = append(statements, stmt)
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

// generateCreateTable is a backward-compat wrapper. Prefer generateCreateTableForDoctypes.
func generateCreateTable(tableName string, registry *doctype.Registry, dialect db.Dialect) string {
	return generateCreateTableForDoctypes(tableName, registry.All(), registry.Get, dialect)
}

func generateCreateTableForDoctypes(tableName string, doctypes []*doctype.DocType, getDocType func(string) *doctype.DocType, dialect db.Dialect) string {
	// Determine if this is a regular table or a child table.
	var dt *doctype.DocType
	var isChild bool
	var parentDT *doctype.DocType
	var parentField string

	for _, d := range doctypes {
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
				dt = getDocType(f.Options)
				break
			}
		}
		if dt != nil {
			break
		}
	}

	// If this is a child table but the child doctype doesn't exist yet,
	// log a warning and skip — the table will be created when the child
	// doctype is activated, with full column information available.
	if isChild && dt == nil {
		slog.Warn("schema: skipping child table creation — child doctype not yet activated",
			"child_table", tableName,
			"parent", parentDT.Name,
			"field", parentField)
		return ""
	}

	// Use quoted table name for SQL.
	sqlTableName := dialect.QuoteIdent(tableName)

	var cols []string

	// System columns from dialect.
	cols = append(cols, dialect.SystemColumnDDL()...)

	// Child table system columns.
	if isChild {
		_ = parentDT
		_ = parentField
		cols = append(cols, dialect.ChildColumnDDL()...)
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

	suffix := dialect.TableSuffix()
	if suffix != "" {
		suffix = " " + suffix
	}
	return fmt.Sprintf("CREATE TABLE %s (\n  %s\n)%s",
		sqlTableName, strings.Join(cols, ",\n  "), suffix)
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
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s%s", dialect.QuoteIdent(tableName), col.Name, col.Type, nullable)
}


func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ApplyDDL executes the DDL statements against the database.
// Uses the dialect's ExecuteBatch for atomic execution (single HTTP
// round-trip for LibSQL, single transaction for MySQL/SQLite).
func ApplyDDL(db *sql.DB, statements []string, dialect db.Dialect) error {
	if len(statements) == 0 {
		return nil
	}
	return dialect.ExecuteBatch(db, statements)
}

// MigrateSite computes the schema diff for a site and applies it.
// newDoctypes is the target config (from the snapshot being activated).
// existingDoctypes is used for child-table lookups (finding child DocTypes
// by name). The registry itself is NOT mutated — callers update it after
// successful migration.
func MigrateSite(database *sql.DB, dbName string, newDoctypes []*doctype.DocType, existingDoctypes *doctype.Registry, dialect db.Dialect) error {
	liveSchema, err := LoadLiveSchema(database, dbName, dialect)
	if err != nil {
		return fmt.Errorf("loading live schema: %w", err)
	}

	diff := ComputeDiff(newDoctypes, existingDoctypes.Get, liveSchema, dialect)
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

	ddl := diff.GenerateDDL(newDoctypes, existingDoctypes.Get, dialect)
	for _, stmt := range ddl {
		slog.Info("DDL", "sql", stmt)
	}

	return ApplyDDL(database, ddl, dialect)
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

// ---------------------------------------------------------------------------
// Backward-compat wrappers — these accept *doctype.Registry and delegate
// to the new doctypes-slice functions. Existing test and integration code
// can keep using these without changes.
// ---------------------------------------------------------------------------

// ComputeDiffFromRegistry is a backward-compat wrapper. Prefer calling
// ComputeDiff directly with doctype slices.
func ComputeDiffFromRegistry(registry *doctype.Registry, liveSchema map[string]*TableInfo, dialect db.SchemaDialect) *Diff {
	return ComputeDiff(registry.All(), registry.Get, liveSchema, dialect)
}

// GenerateDDLFromRegistry is a backward-compat wrapper. Prefer calling
// GenerateDDL directly with doctype slices.
func (d *Diff) GenerateDDLFromRegistry(registry *doctype.Registry, dialect db.Dialect) []string {
	return d.GenerateDDL(registry.All(), registry.Get, dialect)
}

// MigrateSiteFromRegistry is a backward-compat wrapper. Prefer calling
// MigrateSite directly with doctype slices.
func MigrateSiteFromRegistry(database *sql.DB, dbName string, registry *doctype.Registry, dialect db.Dialect) error {
	return MigrateSite(database, dbName, registry.All(), registry, dialect)
}

// ApplyDDLSequential is a backward-compat wrapper that executes DDL
// statements one at a time. Prefer ApplyDDL with ExecuteBatch.
func ApplyDDLSequential(db *sql.DB, statements []string) error {
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("executing DDL: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

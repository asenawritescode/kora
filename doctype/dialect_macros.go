package doctype

import (
	"fmt"
)

// DDLDialect is the subset of the db.Dialect interface needed for DDL generation.
// It is defined locally to avoid a circular import (db imports doctype).
// Any db.Dialect satisfies this interface since it embeds SchemaDialect.
type DDLDialect interface {
	DriverName() string
	CreateTable(dt *DocType) []string
	AddColumn(tableName string, f *Field) string
	AlterColumn(tableName string, f *Field) string
	RenameColumn(tableName, oldName, newName string) string
	CreateIndex(tableName, fieldName string, unique bool) string
	DropIndex(tableName, indexName string) string
	DropColumn(tableName, columnName string) string
}

// GenerateDDLFromDiff generates DDL statements from a change list for a specific dialect.
// Only DDL-relevant change types produce output (add-doctype, add-field, remove-field,
// rename-field, change-field-type, add-index, remove-index). Non-DDL changes like
// role/permission/workflow changes produce no output.
func GenerateDDLFromDiff(changes []Change, dialect DDLDialect) ([]string, error) {
	var statements []string
	fnMap := ddlFuncMap(dialect.DriverName())

	for _, c := range changes {
		fn, ok := fnMap[c.Type]
		if !ok {
			continue // skip non-DDL change types (roles, perms, workflows, etc.)
		}
		stmts := fn(c, dialect)
		statements = append(statements, stmts...)
	}

	return statements, nil
}

// ddlFuncMap returns the DDL generation function map for the given dialect driver.
func ddlFuncMap(driverName string) map[string]func(Change, DDLDialect) []string {
	switch driverName {
	case "libsql":
		return libsqlDDLMap
	default:
		return mysqlDDLMap
	}
}

// mysqlDDLMap maps change types to MySQL DDL generation functions.
var mysqlDDLMap = map[string]func(Change, DDLDialect) []string{
	"add-doctype":       mysqlCreateTable,
	"add-field":         mysqlAddColumn,
	"remove-field":      mysqlDropColumn,
	"rename-field":      mysqlRenameColumn,
	"change-field-type": mysqlAlterColumnDDL,
	"add-index":         mysqlCreateIndex,
	"remove-index":      mysqlDropIndex,
}

// libsqlDDLMap maps change types to LibSQL DDL generation functions.
var libsqlDDLMap = map[string]func(Change, DDLDialect) []string{
	"add-doctype":       libsqlCreateTable,
	"add-field":         libsqlAddColumn,
	"remove-field":      libsqlDropColumn,
	"rename-field":      libsqlRenameColumn,
	"change-field-type": libsqlAlterColumnDDL,
	"add-index":         libsqlCreateIndex,
	"remove-index":      libsqlDropIndex,
}

// ---------------------------------------------------------------------------
// Attr helpers
// ---------------------------------------------------------------------------

func getStringAttr(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getBoolAttr(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// fieldFromChange constructs a Field from a Change's Attrs map for use with
// the dialect's ColumnType, AddColumn, etc.
func fieldFromChange(c Change, fieldname string) *Field {
	f := &Field{
		Fieldname: fieldname,
		Fieldtype: getStringAttr(c.Attrs, ":type", "Data"),
		Reqd:      getBoolAttr(c.Attrs, ":required", false),
		Default:   getStringAttr(c.Attrs, ":default", ""),
		Unique:    getBoolAttr(c.Attrs, ":unique", false),
	}
	if v, ok := c.Attrs[":search_index"]; ok {
		if b, ok := v.(bool); ok {
			f.SearchIndex = b
		}
	}
	return f
}

// buildDocTypeFromAttrs builds a DocType from an add-doctype change's attrs.
func buildDocTypeFromAttrs(c Change) *DocType {
	dt := &DocType{
		Name: c.Entity,
	}

	if fieldsRaw, ok := c.Attrs[":fields"]; ok {
		if fieldsSlice, ok := fieldsRaw.([]any); ok {
			for _, fRaw := range fieldsSlice {
				if fMap, ok := fRaw.(map[string]any); ok {
					f := Field{
						Fieldname:   getStringAttr(fMap, ":fieldname", ""),
						Fieldtype:   getStringAttr(fMap, ":type", "Data"),
						Reqd:        getBoolAttr(fMap, ":required", false),
						Default:     getStringAttr(fMap, ":default", ""),
						Unique:      getBoolAttr(fMap, ":unique", false),
						SearchIndex: getBoolAttr(fMap, ":search_index", false),
					}
					if f.Fieldtype == "Table" {
						f.Options = getStringAttr(fMap, ":table", "")
					}
					dt.Fields = append(dt.Fields, f)
				}
			}
		}
	}
	return dt
}

// ---------------------------------------------------------------------------
// MySQL DDL functions
// ---------------------------------------------------------------------------

func mysqlCreateTable(c Change, d DDLDialect) []string {
	dt := buildDocTypeFromAttrs(c)
	if len(dt.Fields) == 0 {
		return nil
	}
	return d.CreateTable(dt)
}

func mysqlAddColumn(c Change, d DDLDialect) []string {
	f := fieldFromChange(c, c.Field)
	tableName := "tab" + c.Entity
	ddl := d.AddColumn(tableName, f)
	result := []string{ddl}

	// Create separate indexes if needed (AddColumn only adds the column).
	if f.Unique {
		result = append(result, d.CreateIndex(tableName, f.Fieldname, true))
	}
	if f.SearchIndex {
		result = append(result, d.CreateIndex(tableName, f.Fieldname, false))
	}

	return result
}

func mysqlDropColumn(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	ddl := d.DropColumn(tableName, c.Field)
	return []string{ddl + " /* quarantine: data preserved for rollback */"}
}

func mysqlRenameColumn(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	oldName := getStringAttr(c.Attrs, ":renamed-from", "")
	if oldName == "" {
		return nil
	}
	ddl := d.RenameColumn(tableName, oldName, c.Field)
	return []string{ddl}
}

func mysqlAlterColumnDDL(c Change, d DDLDialect) []string {
	f := fieldFromChange(c, c.Field)
	tableName := "tab" + c.Entity
	ddl := d.AlterColumn(tableName, f)
	return []string{ddl}
}

func mysqlCreateIndex(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	unique := getBoolAttr(c.Attrs, ":unique", false)
	ddl := d.CreateIndex(tableName, c.Field, unique)
	return []string{ddl}
}

func mysqlDropIndex(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	indexName := getStringAttr(c.Attrs, ":index_name", fmt.Sprintf("idx_%s_%s", tableName, c.Field))
	ddl := d.DropIndex(tableName, indexName)
	return []string{ddl}
}

// ---------------------------------------------------------------------------
// LibSQL DDL functions
// ---------------------------------------------------------------------------

func libsqlCreateTable(c Change, d DDLDialect) []string {
	dt := buildDocTypeFromAttrs(c)
	if len(dt.Fields) == 0 {
		return nil
	}
	return d.CreateTable(dt)
}

func libsqlAddColumn(c Change, d DDLDialect) []string {
	f := fieldFromChange(c, c.Field)
	tableName := "tab" + c.Entity
	ddl := d.AddColumn(tableName, f)
	result := []string{ddl}

	// Create separate indexes if needed.
	if f.Unique {
		result = append(result, d.CreateIndex(tableName, f.Fieldname, true))
	}
	if f.SearchIndex {
		result = append(result, d.CreateIndex(tableName, f.Fieldname, false))
	}

	return result
}

func libsqlDropColumn(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	ddl := d.DropColumn(tableName, c.Field)
	return []string{ddl}
}

func libsqlRenameColumn(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	oldName := getStringAttr(c.Attrs, ":renamed-from", "")
	if oldName == "" {
		return nil
	}
	ddl := d.RenameColumn(tableName, oldName, c.Field)
	return []string{ddl}
}

func libsqlAlterColumnDDL(c Change, d DDLDialect) []string {
	f := fieldFromChange(c, c.Field)
	tableName := "tab" + c.Entity
	ddl := d.AlterColumn(tableName, f)
	return []string{ddl}
}

func libsqlCreateIndex(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	unique := getBoolAttr(c.Attrs, ":unique", false)
	ddl := d.CreateIndex(tableName, c.Field, unique)
	return []string{ddl}
}

func libsqlDropIndex(c Change, d DDLDialect) []string {
	tableName := "tab" + c.Entity
	indexName := getStringAttr(c.Attrs, ":index_name", fmt.Sprintf("idx_%s_%s", tableName, c.Field))
	ddl := d.DropIndex(tableName, indexName)
	return []string{ddl}
}

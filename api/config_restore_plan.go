package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
)

type restoreJournalData struct {
	TargetVersionID     string `json:"target_version_id"`
	BaseActiveVersionID string `json:"base_active_version_id"`
	ResultVersionID     string `json:"result_version_id,omitempty"`
}

func rollbackConfigOperationTx(tx *sql.Tx, database *sql.DB, dialect db.Dialect, site, operationID, kind, phase, ddlPlan, recoveryPlan string, cause error) error {
	if tx != nil {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			cause = errors.Join(cause, fmt.Errorf("rollback config operation transaction: %w", err))
		}
	}
	if err := configstore.UpdateActivationOperation(database, dialect, site, operationID, kind, phase, ddlPlan, recoveryPlan, cause.Error()); err != nil {
		return fmt.Errorf("%w; recording config operation failure: %v", cause, err)
	}
	return cause
}

// restoreOperationID scopes retries to the explicit client key and target
// version. The stored operation key stays within the existing 36-byte column.
func restoreOperationID(site, targetVersionID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(site + "\x00" + targetVersionID + "\x00" + idempotencyKey))
	return hex.EncodeToString(sum[:18])
}

// buildConfigRestoreDDL replans against live schema so a retry can resume
// after any independently committed MySQL/LibSQL DDL statement. Quarantined
// values are restored when there is exactly one matching retained column or
// table; ambiguous history fails closed for operator resolution.
func buildConfigRestoreDDL(current []*doctype.DocType, target *doctype.ConfigSnapshot, live map[string]*schema.TableInfo, dialect db.Dialect, revision string) ([]string, error) {
	if target == nil {
		return nil, fmt.Errorf("restore snapshot is required")
	}
	adjusted := cloneLiveSchema(live)
	currentByName := make(map[string]*doctype.DocType, len(current))
	targetByName := make(map[string]*doctype.DocType, len(target.DocTypes))
	for _, dt := range current {
		if dt != nil {
			currentByName[dt.Name] = dt
		}
	}
	for _, dt := range target.DocTypes {
		if dt != nil {
			targetByName[dt.Name] = dt
		}
	}
	var ddl []string

	// Restore a previously quarantined table before planning its columns.
	for _, dt := range target.DocTypes {
		if dt == nil || adjusted[dt.RawTableName()] != nil {
			continue
		}
		candidates := matchingQuarantineTables(adjusted, dt.RawTableName())
		if len(candidates) > 1 {
			return nil, fmt.Errorf("multiple quarantined tables match %q; manual recovery selection is required", dt.Name)
		}
		if len(candidates) == 1 {
			from := candidates[0]
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME TO %s", dialect.QuoteIdent(from), dialect.QuoteIdent(dt.RawTableName())))
			table := adjusted[from]
			delete(adjusted, from)
			table.Name = dt.RawTableName()
			adjusted[dt.RawTableName()] = table
		}
	}

	// Restore unambiguous quarantined columns. This prevents a rollback from
	// silently creating an empty replacement while the old values remain parked.
	for _, dt := range target.DocTypes {
		if dt == nil {
			continue
		}
		table := adjusted[dt.RawTableName()]
		if table == nil {
			continue
		}
		for _, field := range dt.DataFields() {
			if field.Fieldtype == "Table" || table.Columns[field.Fieldname] != nil {
				continue
			}
			candidates := matchingQuarantineColumns(table, field.Fieldname)
			if len(candidates) > 1 {
				return nil, fmt.Errorf("multiple quarantined columns match %q.%q; manual recovery selection is required", dt.Name, field.Fieldname)
			}
			if len(candidates) == 1 {
				from := candidates[0]
				ddl = append(ddl, dialect.RenameColumn(dt.RawTableName(), from, field.Fieldname))
				column := table.Columns[from]
				delete(table.Columns, from)
				column.Name = field.Fieldname
				table.Columns[field.Fieldname] = column
			}
		}
	}

	getTarget := func(name string) *doctype.DocType { return targetByName[name] }
	diff := schema.ComputeDiff(target.DocTypes, getTarget, adjusted, dialect)
	ddl = append(ddl, diff.GenerateDDL(target.DocTypes, getTarget, dialect)...)

	// Preserve the active-only field/type changes from the live registry. Schema
	// ComputeDiff handles additions and renames, while these changes need their
	// full target field definitions.
	var fieldChanges []doctype.Change
	for name, oldDT := range currentByName {
		targetDT := targetByName[name]
		if targetDT == nil {
			continue
		}
		liveTable := adjusted[targetDT.RawTableName()]
		if liveTable == nil {
			continue
		}
		newFields := make(map[string]doctype.Field, len(targetDT.Fields))
		for _, field := range targetDT.DataFields() {
			newFields[field.Fieldname] = field
		}
		for _, oldField := range oldDT.DataFields() {
			if oldField.Fieldtype == "Table" {
				continue
			}
			newField, exists := newFields[oldField.Fieldname]
			if !exists {
				if liveTable.Columns[oldField.Fieldname] != nil {
					fieldChanges = append(fieldChanges, doctype.Change{
						Type: "remove-field", Section: "doctypes", Entity: name, Field: oldField.Fieldname,
						Attrs: map[string]any{":quarantine-name": doctype.QuarantineNameForRevision(oldField.Fieldname, revision)},
					})
				}
				continue
			}
			if oldField.Fieldtype != newField.Fieldtype && liveTable.Columns[newField.Fieldname] != nil {
				fieldChanges = append(fieldChanges, doctype.Change{
					Type: "change-field-type", Section: "doctypes", Entity: name, Field: newField.Fieldname,
					Attrs: map[string]any{":type": newField.Fieldtype, ":required": newField.Reqd, ":default": newField.Default, ":unique": newField.Unique, ":search_index": newField.SearchIndex},
				})
			}
		}
	}
	fieldDDL, err := doctype.GenerateDDLFromDiff(fieldChanges, dialect)
	if err != nil {
		return nil, fmt.Errorf("generate restore field DDL: %w", err)
	}
	ddl = append(ddl, fieldDDL...)

	// Quarantine physical tables removed by this restore. Live introspection
	// omits a table after a previous successful rename, making retries safe.
	for name := range currentByName {
		if targetByName[name] != nil {
			continue
		}
		raw := currentByName[name].RawTableName()
		if adjusted[raw] == nil {
			continue
		}
		quarantined := doctype.QuarantineTableNameForRevision(raw, revision)
		if adjusted[quarantined] != nil {
			return nil, fmt.Errorf("quarantine table %q already exists while source table is present", quarantined)
		}
		ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME TO %s", dialect.QuoteIdent(raw), dialect.QuoteIdent(quarantined)))
	}
	return ddl, nil
}

func cloneLiveSchema(source map[string]*schema.TableInfo) map[string]*schema.TableInfo {
	result := make(map[string]*schema.TableInfo, len(source))
	for name, table := range source {
		if table == nil {
			continue
		}
		clone := &schema.TableInfo{Name: table.Name, Columns: make(map[string]*schema.ColumnInfo, len(table.Columns))}
		for columnName, column := range table.Columns {
			if column == nil {
				continue
			}
			copy := *column
			copy.IndexNames = append([]string(nil), column.IndexNames...)
			clone.Columns[columnName] = &copy
		}
		result[name] = clone
	}
	return result
}

func matchingQuarantineTables(live map[string]*schema.TableInfo, rawTable string) []string {
	prefix := "_dropquar_" + rawTable
	var matches []string
	for name := range live {
		if name == prefix || strings.HasPrefix(name, prefix+"_") {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches
}

func matchingQuarantineColumns(table *schema.TableInfo, field string) []string {
	field = strings.Trim(field, "_ ")
	for len(field) > 45 {
		_, size := utf8.DecodeLastRuneInString(field)
		field = field[:len(field)-size]
	}
	prefix := "_dropquar_" + field + "_"
	var matches []string
	for name := range table.Columns {
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches
}

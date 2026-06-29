package doctype

import (
	"fmt"
	"strings"
)

// GenerateRollbackPlan produces quarantine-aware DDL from a reversed change list.
// Instead of DROP COLUMN/TABLE, it renames them to quarantine names.
// The quarantine map records old -> new names for quarantined objects.
//
// changes should be a reversed change list (produced by ReverseDiff).
func GenerateRollbackPlan(changes []Change, dialectName string) (ddl []string, quarantine map[string]string) {
	quarantine = make(map[string]string)
	q := func(name string) string {
		if dialectName == "libsql" {
			return `"` + name + `"`
		}
		return "`" + name + "`"
	}

	for _, ch := range changes {
		switch ch.Type {
		case "remove-field":
			// A field was added in the forward direction; rollback removes it.
			// Instead of DROP COLUMN, rename it to quarantine.
			quarantineName := "_dropquar_" + ch.Field
			tableName := "tab" + ch.Entity
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
				q(tableName), q(ch.Field), q(quarantineName)))
			quarantine[ch.Entity+"."+ch.Field] = quarantineName

		case "add-field":
			// A field was removed in the forward direction; rollback adds it back.
			// We can't reconstruct the column type from a Change alone; emit a
			// quarantine note indicating the caller should use the snapshot.
			quarantine[ch.Entity+"."+ch.Field+"_replay_needed"] = ch.Field

		case "remove-doctypes":
			// A doctype was added in the forward direction; rollback removes it.
			// Instead of DROP TABLE, rename it to quarantine.
			quarantineTable := "_dropquar_tab" + ch.Entity
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME TO %s",
				q("tab"+ch.Entity), q(quarantineTable)))
			quarantine["tab"+ch.Entity] = quarantineTable

		case "add-doctypes":
			// A doctype was removed in the forward direction; rollback adds it back.
			// Emit a quarantine note indicating the snapshot should be used.
			quarantine["tab"+ch.Entity+"_replay_needed"] = ch.Entity

		case "modify-doctypes", "modify-roles", "modify-permissions":
			// Non-structural changes; no DDL needed.
		}
	}

	return ddl, quarantine
}

// PrepareRollbackDDLFromSnapshot generates full rollback DDL by comparing the
// current registry state against a target snapshot. This produces correct
// ADD COLUMN and CREATE TABLE statements because it has the full field and
// doctype definitions from the snapshot.
func PrepareRollbackDDLFromSnapshot(currentReg *Registry, snapshot *ConfigSnapshot, dialect interface {
	AddColumn(tableName string, f *Field) string
	CreateTable(dt *DocType) []string
	QuoteIdent(name string) string
}) []string {
	var ddl []string
	q := dialect.QuoteIdent

	// Build maps for quick lookup.
	snapshotDTs := make(map[string]*DocType)
	for _, dt := range snapshot.DocTypes {
		snapshotDTs[dt.Name] = dt
	}

	currentDTs := make(map[string]*DocType)
	for _, dt := range currentReg.All() {
		currentDTs[dt.Name] = dt
	}

	// Doctypes in current but not in snapshot -> quarantine them.
	for name := range currentDTs {
		if _, exists := snapshotDTs[name]; !exists {
			oldTable := q("tab" + name)
			newTable := q("_dropquar_tab" + name)
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME TO %s", oldTable, newTable))
		}
	}

	// Doctypes in snapshot but not in current -> CREATE TABLE.
	for name, dt := range snapshotDTs {
		if _, exists := currentDTs[name]; !exists {
			ddl = append(ddl, dialect.CreateTable(dt)...)
		}
	}

	// For doctypes in both, diff the fields.
	for name, dt := range snapshotDTs {
		currentDT, exists := currentDTs[name]
		if !exists {
			continue
		}
		tableName := q("tab" + name)

		snapshotFields := make(map[string]*Field)
		for i := range dt.Fields {
			snapshotFields[dt.Fields[i].Fieldname] = &dt.Fields[i]
		}
		currentFields := make(map[string]*Field)
		for i := range currentDT.Fields {
			currentFields[currentDT.Fields[i].Fieldname] = &currentDT.Fields[i]
		}

		// Fields in snapshot but not in current -> ADD COLUMN.
		for name, f := range snapshotFields {
			if _, exists := currentFields[name]; !exists {
				ddl = append(ddl, dialect.AddColumn(tableName, f))
			}
		}

		// Fields in current but not in snapshot -> RENAME COLUMN to quarantine.
		for name := range currentFields {
			if _, exists := snapshotFields[name]; !exists {
				qName := q("_dropquar_" + name)
				ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
					tableName, q(name), qName))
			}
		}

		// Handle renamed fields via renamed_from.
		for name, f := range snapshotFields {
			if f.RenamedFrom != "" {
				if _, currentHasNew := currentFields[name]; !currentHasNew {
					if _, currentHasOld := currentFields[f.RenamedFrom]; currentHasOld {
						ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
							tableName, q(f.RenamedFrom), q(name)))
					}
				}
			}
		}
	}

	return ddl
}

// RollbackDDLForVersion generates rollback DDL to revert the schema from its
// current state back to the state described in the target snapshot.
func RollbackDDLForVersion(reg *Registry, snapshot *ConfigSnapshot, dialect interface {
	AddColumn(tableName string, f *Field) string
	CreateTable(dt *DocType) []string
	QuoteIdent(name string) string
}) []string {
	return PrepareRollbackDDLFromSnapshot(reg, snapshot, dialect)
}

// QuarantineName returns the quarantine name for a field or table entity.
func QuarantineName(name string) string {
	return "_dropquar_" + strings.TrimPrefix(name, "tab")
}

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

// QuarantineName returns the quarantine name for a field or table entity.
func QuarantineName(name string) string {
	return "_dropquar_" + strings.TrimPrefix(name, "tab")
}

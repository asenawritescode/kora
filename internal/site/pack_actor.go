package site

import (
	"maps"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

// actorValue is the reference a command writes to store the acting user.
const actorValue = "$actor"

// packCommand is a parsed command file of a pack, kept with its path for findings.
type packCommand struct {
	path string
	def  *kernel.CommandResource
}

// actorWrite is one field a command stores the acting user into.
type actorWrite struct {
	record, field string
}

// actorWriteWarnings reports every command value that writes $actor into a field of a DocType declared in this
// pack when that field is not a User field. A User field is the only place an internal user is referenced; a Data
// field would hold the login as free text. Targets outside the pack are skipped, because the composition may
// supply them. The findings are warnings, which do not make a pack invalid: releases published before this rule
// store the cashier as Data and must still apply. The repository's own packs are held to it by tests.
func actorWriteWarnings(commands []packCommand, docTypes map[string]*doctype.DocType) []PackFinding {
	var out []PackFinding
	for _, cmd := range commands {
		for _, w := range actorWrites(cmd.def) {
			target := docTypes[strings.ToLower(strings.TrimSpace(w.record))]
			if target == nil {
				continue
			}
			field := target.GetField(w.field)
			if field == nil || field.Fieldtype == "User" {
				continue
			}
			out = append(out, PackFinding{Code: "actor_field_not_user", Severity: "warning", Path: cmd.path,
				Message: cmd.def.FullName() + " writes $actor to " + target.Name + "." + w.field + ", which is a " + field.Fieldtype + " field; declare it fieldtype: User so it stores a login reference"})
		}
	}
	return out
}

// actorWrites lists the fields of a command that a write step fills with $actor.
// Where clauses on update and the guards are reads, so they are not listed.
func actorWrites(def *kernel.CommandResource) []actorWrite {
	var out []actorWrite
	for _, s := range def.Steps {
		if s.Create != nil {
			out = appendActorWrites(out, s.Create.Record, s.Create.Values)
		}
		if s.Update != nil {
			out = appendActorWrites(out, s.Update.Record, s.Update.Values)
		}
		if s.Upsert != nil {
			out = appendActorWrites(out, s.Upsert.Record, s.Upsert.Values)
		}
		if s.Ensure != nil {
			// Ensure seeds the record it creates from both where and values.
			out = appendActorWrites(out, s.Ensure.Record, s.Ensure.Where)
			out = appendActorWrites(out, s.Ensure.Record, s.Ensure.Values)
		}
	}
	return out
}

func appendActorWrites(out []actorWrite, record string, values map[string]string) []actorWrite {
	for _, field := range slices.Sorted(maps.Keys(values)) {
		if strings.TrimSpace(values[field]) == actorValue {
			out = append(out, actorWrite{record: record, field: field})
		}
	}
	return out
}

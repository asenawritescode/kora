package doctype

import (
	"fmt"
)

// ChangeType categorizes a config change.
type ChangeType string

const (
	ChangeFieldAdded      ChangeType = "field_added"
	ChangeFieldRemoved    ChangeType = "field_removed"
	ChangeFieldTypeChanged ChangeType = "field_type_changed"
	ChangeFieldRenamed    ChangeType = "field_renamed"
	ChangeConstraintAdded   ChangeType = "constraint_added"
	ChangeConstraintRemoved ChangeType = "constraint_removed"
	ChangeDocTypeAdded      ChangeType = "doctype_added"
	ChangeDocTypeRemoved    ChangeType = "doctype_removed"
	ChangeFieldRequired     ChangeType = "field_required_changed"
	ChangeFieldLength       ChangeType = "field_length_changed"
	ChangeFieldDefault      ChangeType = "field_default_changed"
)

// ConfigChange represents a single change between two config versions.
type ConfigChange struct {
	Type     ChangeType `json:"type"     yaml:"type"`
	DocType  string     `json:"doctype"  yaml:"doctype"`
	Field    string     `json:"field,omitempty"    yaml:"field,omitempty"`
	OldValue string     `json:"old_value,omitempty" yaml:"old_value,omitempty"`
	NewValue string     `json:"new_value,omitempty" yaml:"new_value,omitempty"`
	Breaking bool       `json:"breaking" yaml:"breaking"`
	Message  string     `json:"message"  yaml:"message"`
}

// ConfigDiff holds the full set of changes between two versions.
type ConfigDiff struct {
	FromVersion int            `json:"from_version" yaml:"from_version"`
	ToVersion   int            `json:"to_version"   yaml:"to_version"`
	Changes     []ConfigChange `json:"changes"      yaml:"changes"`
	IsBreaking  bool           `json:"is_breaking"  yaml:"is_breaking"`
}

// DiffConfigs compares two sets of DocTypes and produces a structured diff.
func DiffConfigs(old, new []*DocType) *ConfigDiff {
	diff := &ConfigDiff{}

	oldMap := make(map[string]*DocType)
	newMap := make(map[string]*DocType)
	for _, dt := range old {
		oldMap[dt.Name] = dt
	}
	for _, dt := range new {
		newMap[dt.Name] = dt
	}

	// Detect added/removed doctypes.
	for name := range newMap {
		if _, ok := oldMap[name]; !ok {
			diff.Changes = append(diff.Changes, ConfigChange{
				Type:     ChangeDocTypeAdded,
				DocType:  name,
				Breaking: false,
				Message:  fmt.Sprintf("DocType %q added", name),
			})
		}
	}
	for name := range oldMap {
		if _, ok := newMap[name]; !ok {
			diff.Changes = append(diff.Changes, ConfigChange{
				Type:     ChangeDocTypeRemoved,
				DocType:  name,
				Breaking: true,
				Message:  fmt.Sprintf("DocType %q removed", name),
			})
		}
	}

	// Compare fields within each common doctype.
	for name, oldDT := range oldMap {
		newDT, ok := newMap[name]
		if !ok {
			continue
		}
		changes := diffFields(oldDT, newDT)
		diff.Changes = append(diff.Changes, changes...)
	}

	// Check if any change is breaking.
	for _, c := range diff.Changes {
		if c.Breaking {
			diff.IsBreaking = true
			break
		}
	}

	return diff
}

func diffFields(oldDT, newDT *DocType) []ConfigChange {
	var changes []ConfigChange

	oldFields := make(map[string]*Field)
	newFields := make(map[string]*Field)
	for i := range oldDT.Fields {
		oldFields[oldDT.Fields[i].Fieldname] = &oldDT.Fields[i]
	}
	for i := range newDT.Fields {
		newFields[newDT.Fields[i].Fieldname] = &newDT.Fields[i]
	}

	// Detect cross-name renames: new field has RenamedFrom pointing to an old field name
	// that no longer exists in the new config (meaning the field was renamed in the DocType).
	renamedFields := make(map[string]string) // old_fieldname → new_fieldname
	matchedOld := make(map[string]bool)      // old field names that are renames (not removals)

	for newName, newF := range newFields {
		if newF.RenamedFrom == "" {
			continue
		}
		if _, hasNew := oldFields[newName]; hasNew {
			// Same-name rename — the fieldname didn't change, just the column was renamed in DB.
			// Handled below in the "changed fields" loop.
			continue
		}
		if _, hasOld := oldFields[newF.RenamedFrom]; hasOld {
			// Cross-name rename: old field "status" → new field "state" with renamed_from="status".
			renamedFields[newF.RenamedFrom] = newName
			matchedOld[newF.RenamedFrom] = true
		}
	}

	// Added fields (skip those that are actually renames).
	for name, f := range newFields {
		if _, ok := oldFields[name]; ok {
			continue // It's a matched same-name field, handle in "changed fields".
		}
		// Check if this new field is a rename target.
		isRename := false
		for _, renamedTo := range renamedFields {
			if renamedTo == name {
				isRename = true
				break
			}
		}
		if isRename {
			continue // Handled as a rename below.
		}
		c := ConfigChange{
			Type:    ChangeFieldAdded,
			DocType: oldDT.Name,
			Field:   name,
			Message: fmt.Sprintf("Field %q added to %s", name, oldDT.Name),
		}
		// Adding optional field = non-breaking; adding required field without default = breaking.
		if f.Reqd && f.Default == "" {
			c.Breaking = true
			c.Message += " (required, no default — BREAKING)"
		}
		changes = append(changes, c)
	}

	// Removed fields (skip those that are actually renames).
	for name := range oldFields {
		if matchedOld[name] {
			continue // This old field was renamed, not removed.
		}
		if _, ok := newFields[name]; ok {
			continue // Same-name field exists, handle in "changed fields".
		}
		changes = append(changes, ConfigChange{
			Type:     ChangeFieldRemoved,
			DocType:  oldDT.Name,
			Field:    name,
			Breaking: true,
			Message:  fmt.Sprintf("Field %q removed from %s (BREAKING)", name, oldDT.Name),
		})
	}

	// Emit renames detected above.
	for oldName, newName := range renamedFields {
		changes = append(changes, ConfigChange{
			Type:     ChangeFieldRenamed,
			DocType:  oldDT.Name,
			Field:    newName,
			OldValue: oldName,
			NewValue: newName,
			Breaking: false,
			Message:  fmt.Sprintf("Field renamed from %q to %q", oldName, newName),
		})
	}

	// Changed fields.
	for name, oldF := range oldFields {
		newF, ok := newFields[name]
		if !ok {
			continue
		}

		// Type change.
		if oldF.Fieldtype != newF.Fieldtype {
			changes = append(changes, ConfigChange{
				Type:     ChangeFieldTypeChanged,
				DocType:  oldDT.Name,
				Field:    name,
				OldValue: oldF.Fieldtype,
				NewValue: newF.Fieldtype,
				Breaking: true,
				Message:  fmt.Sprintf("Field %q type changed from %s to %s (BREAKING)", name, oldF.Fieldtype, newF.Fieldtype),
			})
		}

		// Required changed.
		if oldF.Reqd != newF.Reqd {
			c := ConfigChange{
				Type:    ChangeFieldRequired,
				DocType: oldDT.Name,
				Field:   name,
			}
			if newF.Reqd && !oldF.Reqd {
				c.Breaking = newF.Default == ""
				c.Message = fmt.Sprintf("Field %q is now required", name)
				if c.Breaking {
					c.Message += " (no default — BREAKING)"
				}
			} else {
				c.Breaking = false
				c.Message = fmt.Sprintf("Field %q is no longer required", name)
			}
			changes = append(changes, c)
		}

		// Renamed field (via renamed_from).
		if newF.RenamedFrom != "" {
			changes = append(changes, ConfigChange{
				Type:     ChangeFieldRenamed,
				DocType:  oldDT.Name,
				Field:    name,
				OldValue: newF.RenamedFrom,
				NewValue: name,
				Breaking: false,
				Message:  fmt.Sprintf("Field renamed from %q to %q", newF.RenamedFrom, name),
			})
		}

		// Constraint changes.
		oldConstr := constraintMap(oldF.Constraints)
		newConstr := constraintMap(newF.Constraints)
		for cType := range newConstr {
			if _, ok := oldConstr[cType]; !ok {
				c := ConfigChange{
					Type:     ChangeConstraintAdded,
					DocType:  oldDT.Name,
					Field:    name,
					NewValue: cType,
					Message:  fmt.Sprintf("Constraint %q added to field %q", cType, name),
				}
				if isTighteningConstraint(cType) {
					c.Breaking = true
					c.Message += " (tightening — BREAKING)"
				}
				changes = append(changes, c)
			}
		}
		for cType := range oldConstr {
			if _, ok := newConstr[cType]; !ok {
				changes = append(changes, ConfigChange{
					Type:     ChangeConstraintRemoved,
					DocType:  oldDT.Name,
					Field:    name,
					OldValue: cType,
					Breaking: false,
					Message:  fmt.Sprintf("Constraint %q removed from field %q", cType, name),
				})
			}
		}
	}

	return changes
}

func constraintMap(constraints []Constraint) map[string]bool {
	m := make(map[string]bool)
	for _, c := range constraints {
		m[c.Type] = true
	}
	return m
}

func isTighteningConstraint(cType string) bool {
	switch cType {
	case "min", "min_length", "min_date", "min_rows", "required_if", "regex":
		return true
	default:
		return false
	}
}

// BreakingChanges returns only breaking changes from a diff.
func (d *ConfigDiff) BreakingChanges() []ConfigChange {
	var result []ConfigChange
	for _, c := range d.Changes {
		if c.Breaking {
			result = append(result, c)
		}
	}
	return result
}

// Summary returns a human-readable summary of the diff.
func (d *ConfigDiff) Summary() string {
	added, removed, changed, breaking := 0, 0, 0, 0
	for _, c := range d.Changes {
		switch c.Type {
		case ChangeDocTypeAdded, ChangeFieldAdded, ChangeConstraintAdded:
			added++
		case ChangeDocTypeRemoved, ChangeFieldRemoved, ChangeConstraintRemoved:
			removed++
		default:
			changed++
		}
		if c.Breaking {
			breaking++
		}
	}
	return fmt.Sprintf("%d added, %d removed, %d changed (%d breaking)", added, removed, changed, breaking)
}

package doctype

// ---------------------------------------------------------------------------
// Change represents a single config change for impact analysis and DDL generation.
// Type uses kebab-case: add-field, remove-field, rename-field, change-field-type,
// change-field-property, add-doctype, remove-doctype, add-role, remove-role,
// add-perm, remove-perm, add-workflow, remove-workflow, add-metric, remove-metric,
// add-script-ref, remove-script-ref.
//
// The Attrs map uses keyword-style keys like ":required", ":type", ":default",
// ":unique", ":renamed-from", ":idx", ":search_index", ":fields".
type Change struct {
	Type     string         `json:"type"`
	Section  string         `json:"section"`
	Entity   string         `json:"entity"`
	Field    string         `json:"field,omitempty"`
	OldValue any            `json:"old_value,omitempty"`
	NewValue any            `json:"new_value,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
}

// ---------------------------------------------------------------------------

// configChangeTypeMap maps ConfigChange ChangeType to the kebab-case Change.Type.
var configChangeTypeMap = map[ChangeType]string{
	ChangeDocTypeAdded:      "add-doctype",
	ChangeDocTypeRemoved:    "remove-doctype",
	ChangeFieldAdded:        "add-field",
	ChangeFieldRemoved:      "remove-field",
	ChangeFieldRenamed:      "rename-field",
	ChangeFieldTypeChanged:  "change-field-type",
	ChangeFieldRequired:     "change-field-property",
	ChangeConstraintAdded:   "change-field-property",
	ChangeConstraintRemoved: "change-field-property",
	ChangeFieldLength:       "change-field-property",
	ChangeFieldDefault:      "change-field-property",
}

// sectionChangeTypeKey builds a lookup key from section and change direction.
var sectionChangeTypeMap = map[string]string{
	"roles.added":                 "add-role",
	"roles.removed":               "remove-role",
	"permissions.added":           "add-perm",
	"permissions.removed":         "remove-perm",
	"workflows.added":             "add-workflow",
	"workflows.removed":           "remove-workflow",
	"analytics_metrics.added":     "add-metric",
	"analytics_metrics.removed":   "remove-metric",
	"scripts.added":               "add-script-ref",
	"scripts.removed":             "remove-script-ref",
}

// ConvertConfigChanges converts the existing ConfigChange and SectionChange slices
// into the new Change format, enriching field-level changes with details from
// the snapshot for DDL generation and impact analysis.
func ConvertConfigChanges(dtChanges []ConfigChange, sectionChanges []SectionChange, snapshot *ConfigSnapshot) []Change {
	var changes []Change

	for _, cc := range dtChanges {
		c := Change{
			Section: "doctypes",
			Entity:  cc.DocType,
			Field:   cc.Field,
			Attrs:   make(map[string]any),
		}

		macroType, ok := configChangeTypeMap[cc.Type]
		if !ok {
			macroType = string(cc.Type)
		}
		c.Type = macroType

		// Enrich with field details from the snapshot.
		switch cc.Type {
		case ChangeDocTypeAdded:
			if dt := findDocType(snapshot.DocTypes, cc.DocType); dt != nil {
				c.Attrs[":fields"] = docTypeToFieldMaps(dt)
			}
		case ChangeFieldAdded:
			if dt := findDocType(snapshot.DocTypes, cc.DocType); dt != nil {
				if f := dt.GetField(cc.Field); f != nil {
					c.Attrs[":type"] = f.Fieldtype
					c.Attrs[":required"] = f.Reqd
					c.Attrs[":default"] = f.Default
					c.Attrs[":unique"] = f.Unique
					c.Attrs[":search_index"] = f.SearchIndex
				}
			}
		case ChangeFieldRemoved:
			if dt := findDocType(snapshot.DocTypes, cc.DocType); dt != nil {
				if f := dt.GetField(cc.Field); f != nil {
					c.Attrs[":type"] = f.Fieldtype
				}
			}
		case ChangeFieldRenamed:
			c.Attrs[":renamed-from"] = cc.OldValue
		case ChangeFieldTypeChanged:
			c.Attrs[":type"] = cc.NewValue
			c.Attrs[":old_type"] = cc.OldValue
		case ChangeFieldRequired:
			c.Attrs[":required"] = cc.NewValue == "true"
			c.Attrs[":old_value"] = cc.OldValue
		case ChangeConstraintAdded:
			c.Attrs[":constraints"] = []string{cc.NewValue}
		case ChangeConstraintRemoved:
			c.Attrs[":constraints_removed"] = []string{cc.OldValue}
		}

		changes = append(changes, c)
	}

	// Convert section changes.
	for _, sc := range sectionChanges {
		key := sc.Section + "." + sc.Change
		macroType, ok := sectionChangeTypeMap[key]
		if !ok {
			continue
		}
		changes = append(changes, Change{
			Type:    macroType,
			Section: sc.Section,
			Entity:  sc.Name,
			Attrs:   make(map[string]any),
		})
	}

	return changes
}

// findDocType finds a DocType by name in a slice.
func findDocType(doctypes []*DocType, name string) *DocType {
	for _, dt := range doctypes {
		if dt.Name == name {
			return dt
		}
	}
	return nil
}

// docTypeToFieldMaps converts a DocType's fields to the attrs format for DDL generation.
func docTypeToFieldMaps(dt *DocType) []any {
	var result []any
	for _, f := range dt.Fields {
		m := map[string]any{
			":fieldname": f.Fieldname,
			":type":      f.Fieldtype,
		}
		if f.Reqd {
			m[":required"] = true
		}
		if f.Default != "" {
			m[":default"] = f.Default
		}
		if f.Unique {
			m[":unique"] = true
		}
		if f.SearchIndex {
			m[":search_index"] = true
		}
		if f.Fieldtype == "Table" {
			m[":table"] = f.Options
		}
		result = append(result, m)
	}
	return result
}

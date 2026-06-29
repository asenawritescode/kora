// Package doctype defines the core types for Kora's config-driven data model.
package doctype

// DocType represents a data entity definition. It defines the fields,
// constraints, relationships, and UI hints for a document type.
type DocType struct {
	Name          string        `yaml:"name"          json:"name"`
	Module        string        `yaml:"module"        json:"module"`
	IsSubmittable bool          `yaml:"is_submittable" json:"is_submittable"`
	IsChildTable  bool          `yaml:"is_child_table" json:"is_child_table"`
	IsSingle      bool          `yaml:"is_single"      json:"is_single"`
	TrackChanges  bool          `yaml:"track_changes"  json:"track_changes"`
	TitleField    string        `yaml:"title_field"    json:"title_field"`
	SearchFields  string        `yaml:"search_fields"  json:"search_fields"`
	SortField     string        `yaml:"sort_field"     json:"sort_field"`
	SortOrder     string        `yaml:"sort_order"     json:"sort_order"`
	Description   string        `yaml:"description"    json:"description"`
	Fields        []Field       `yaml:"fields"         json:"fields"`
	DocConstraints []DocConstraint `yaml:"doc_constraints" json:"doc_constraints"`
}

// TableName returns the backtick-quoted database table name for SQL statements.
// All application tables are prefixed with "tab".
func (d *DocType) TableName() string {
	return "`tab" + d.Name + "`"
}

// RawTableName returns the unquoted table name (for INFORMATION_SCHEMA comparisons).
func (d *DocType) RawTableName() string {
	return "tab" + d.Name
}

// ChildTableName returns the backtick-quoted child table name for SQL statements.
func (d *DocType) ChildTableName(fieldName string) string {
	return "`tab" + d.Name + "__" + fieldName + "`"
}

// RawChildTableName returns the unquoted child table name.
func (d *DocType) RawChildTableName(fieldName string) string {
	return "tab" + d.Name + "__" + fieldName
}

// GetField returns the field definition by fieldname, or nil if not found.
func (d *DocType) GetField(fieldname string) *Field {
	for i := range d.Fields {
		if d.Fields[i].Fieldname == fieldname {
			return &d.Fields[i]
		}
	}
	return nil
}

// DataFields returns all fields that map to database columns
// (excludes layout-only fields like Section Break, Column Break, Heading).
func (d *DocType) DataFields() []Field {
	var result []Field
	for _, f := range d.Fields {
		if f.IsDataField() {
			result = append(result, f)
		}
	}
	return result
}

// TableFields returns all Table-type fields.
func (d *DocType) TableFields() []Field {
	var result []Field
	for _, f := range d.Fields {
		if f.Fieldtype == "Table" {
			result = append(result, f)
		}
	}
	return result
}

// Field represents a single field in a DocType.
type Field struct {
	Fieldname          string       `yaml:"fieldname"           json:"fieldname"`
	Fieldtype          string       `yaml:"fieldtype"           json:"fieldtype"`
	Label              string       `yaml:"label"               json:"label"`
	Options            string       `yaml:"options"             json:"options"`
	Reqd               bool         `yaml:"reqd"                json:"reqd"`
	Unique             bool         `yaml:"unique"              json:"unique"`
	Default            string       `yaml:"default"             json:"default"`
	Hidden             bool         `yaml:"hidden"              json:"hidden"`
	ReadOnly           bool         `yaml:"read_only"           json:"read_only"`
	Bold               bool         `yaml:"bold"                json:"bold"`
	InListView         bool         `yaml:"in_list_view"        json:"in_list_view"`
	InStandardFilter   bool         `yaml:"in_standard_filter"  json:"in_standard_filter"`
	SearchIndex        bool         `yaml:"search_index"        json:"search_index"`
	Description        string       `yaml:"description"         json:"description"`
	DependsOn          string       `yaml:"depends_on"          json:"depends_on"`
	MandatoryDependsOn string       `yaml:"mandatory_depends_on" json:"mandatory_depends_on"`
	Constraints        []Constraint `yaml:"constraints"         json:"constraints"`
	RenamedFrom        string       `yaml:"renamed_from"        json:"renamed_from"`
	LinkedField        string       `yaml:"linked_field"        json:"linked_field,omitempty"`
	Computed           string       `yaml:"computed"            json:"computed,omitempty"`
	DependencyScope    string       `yaml:"dependency_scope"    json:"dependency_scope,omitempty"`
}

// IsDataField returns true if this field maps to a database column.
// Layout-only fields (Section Break, Column Break, Heading) do not.
func (f *Field) IsDataField() bool {
	switch f.Fieldtype {
	case "Section Break", "Column Break", "Heading":
		return false
	default:
		return true
	}
}

// IsLayoutField returns true if this field is a layout-only field.
func (f *Field) IsLayoutField() bool {
	return !f.IsDataField()
}

// IsNumeric returns true if the field type stores a numeric value.
func (f *Field) IsNumeric() bool {
	switch f.Fieldtype {
	case "Int", "Float", "Currency", "Percent":
		return true
	default:
		return false
	}
}

// Constraint is a validation rule on a field.
type Constraint struct {
	Type      string   `yaml:"type"      json:"type"`
	Value     any      `yaml:"value"     json:"value,omitempty"`
	Values    []string `yaml:"values"    json:"values,omitempty"`
	Pattern   string   `yaml:"pattern"   json:"pattern,omitempty"`
	Message   string   `yaml:"message"   json:"message"`
	Condition string   `yaml:"condition" json:"condition,omitempty"`
	Scope     string   `yaml:"scope"     json:"scope,omitempty"` // for unique_in: "global" or "parent"
}

// DocConstraint is a document-level validation rule.
type DocConstraint struct {
	Type           string         `yaml:"type"            json:"type"`
	Description    string         `yaml:"description"     json:"description"`
	Condition      string         `yaml:"condition"       json:"condition,omitempty"`
	RequireFields  []string       `yaml:"require_fields"  json:"require_fields,omitempty"`
	Field          string         `yaml:"field"           json:"field,omitempty"`
	GroupBy        []string       `yaml:"group_by"        json:"group_by,omitempty"`
	Max            float64        `yaml:"max"             json:"max,omitempty"`
	Message        string         `yaml:"message"         json:"message"`
	LHS            string         `yaml:"lhs"             json:"lhs,omitempty"`
	Operator       string         `yaml:"operator"        json:"operator,omitempty"`
	RHS            string         `yaml:"rhs"             json:"rhs,omitempty"`
	Fields         []string       `yaml:"fields"          json:"fields,omitempty"`
	StatusField    string         `yaml:"status_field"    json:"status_field,omitempty"`
	StatusValues   []string       `yaml:"status_values"   json:"status_values,omitempty"`
	ImmutableFields []string      `yaml:"immutable_fields" json:"immutable_fields,omitempty"`
	Constraints    []Constraint   `yaml:"constraints"     json:"constraints,omitempty"`
}

// SystemColumns returns the list of system column definitions that every table has.
func SystemColumns() []struct {
	Name string
	Type string
} {
	return []struct {
		Name string
		Type string
	}{
		{"name", "VARCHAR(140)"},
		{"owner", "VARCHAR(140)"},
		{"creation", "DATETIME(6)"},
		{"modified", "DATETIME(6)"},
		{"modified_by", "VARCHAR(140)"},
		{"doc_status", "TINYINT(1)"},
		{"idx", "INT"},
	}
}

// ChildSystemColumns returns additional system columns for child tables.
func ChildSystemColumns() []struct {
	Name string
	Type string
} {
	return []struct {
		Name string
		Type string
	}{
		{"parent", "VARCHAR(140)"},
		{"parentfield", "VARCHAR(140)"},
		{"parenttype", "VARCHAR(140)"},
	}
}

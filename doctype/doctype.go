// Package doctype defines the core types for Kigae's config-driven data model.
package doctype

import "github.com/asenawritescode/kora/contract"

// DocType represents a data entity definition. It defines the fields,
// constraints, relationships, and UI hints for a document type.
type DocType struct {
	Name                  string                     `yaml:"name"          json:"name"`
	ResourceName          string                     `yaml:"resource_name" json:"resource_name,omitempty"`
	BusinessResourceType  string                     `yaml:"business_resource_type,omitempty" json:"business_resource_type,omitempty"`
	BusinessIdentityField string                     `yaml:"business_identity_field,omitempty" json:"business_identity_field,omitempty"`
	Module                string                     `yaml:"module"        json:"module"`
	ModuleOwnerID         string                     `yaml:"module_owner_id,omitempty" json:"module_owner_id,omitempty"`
	ModuleAccess          contract.ModuleAccess      `yaml:"module_access,omitempty" json:"module_access,omitempty"`
	ModuleOwnershipMode   string                     `yaml:"module_ownership_mode,omitempty" json:"module_ownership_mode,omitempty"`
	BusinessScopePolicy   contract.ResourceScopeKind `yaml:"business_scope_policy,omitempty" json:"business_scope_policy,omitempty"`
	BusinessScopeField    string                     `yaml:"business_scope_field,omitempty" json:"business_scope_field,omitempty"`
	ReferenceRule         *contract.ReferenceRule    `yaml:"reference_rule,omitempty" json:"reference_rule,omitempty"`
	IsSubmittable         bool                       `yaml:"is_submittable" json:"is_submittable"`
	IsChildTable          bool                       `yaml:"is_child_table" json:"is_child_table"`
	IsSingle              bool                       `yaml:"is_single"      json:"is_single"`
	TrackChanges          bool                       `yaml:"track_changes"  json:"track_changes"`
	AppendOnly            bool                       `yaml:"append_only"    json:"append_only,omitempty"`
	TitleField            string                     `yaml:"title_field"    json:"title_field"`
	SearchFields          string                     `yaml:"search_fields"  json:"search_fields"`
	SortField             string                     `yaml:"sort_field"     json:"sort_field"`
	SortOrder             string                     `yaml:"sort_order"     json:"sort_order"`
	Description           string                     `yaml:"description"    json:"description"`
	Fields                []Field                    `yaml:"fields"         json:"fields"`
	DocConstraints        []DocConstraint            `yaml:"doc_constraints" json:"doc_constraints"`
	PublicAccess          *PublicAccess              `yaml:"public_access" json:"public_access,omitempty"`
	QuantityEffect        *QuantityEffect            `yaml:"quantity_effect" json:"quantity_effect,omitempty"`
	// NamingSeries configures generated record names, e.g. "INV-.YYYY.-.#####".
	NamingSeries string `yaml:"naming_series,omitempty" json:"naming_series,omitempty"`

	metadataReady      bool
	dataFields         []Field
	nonTableDataFields []Field
	tableFields        []Field
	listFields         []Field
	validSortColumns   map[string]bool
}

// QuantityEffect binds a record form to a configured transactional view action.
// Clients may use the same binding to project the signed effect before sync.
type QuantityEffect struct {
	View                string `yaml:"view" json:"view"`
	Component           string `yaml:"component" json:"component"`
	Action              string `yaml:"action" json:"action"`
	TargetDocType       string `yaml:"target_doctype" json:"target_doctype"`
	TargetQuantityField string `yaml:"target_quantity_field" json:"target_quantity_field"`
	SourceRecordField   string `yaml:"source_record_field" json:"source_record_field"`
	DeltaField          string `yaml:"delta_field" json:"delta_field"`
	ReasonField         string `yaml:"reason_field" json:"reason_field"`
}

// PublicAccess defines the explicit unauthenticated read surface for a DocType.
// Public routes expose only listed fields after server-owned filters are applied.
type PublicAccess struct {
	Enabled     bool           `yaml:"enabled" json:"enabled"`
	List        bool           `yaml:"list" json:"list"`
	Read        bool           `yaml:"read" json:"read"`
	Fields      []string       `yaml:"fields" json:"fields"`
	Filters     []PublicFilter `yaml:"filters" json:"filters"`
	SortField   string         `yaml:"sort_field" json:"sort_field"`
	SortOrder   string         `yaml:"sort_order" json:"sort_order"`
	MaxLimit    int            `yaml:"max_limit" json:"max_limit"`
	CacheMaxAge int            `yaml:"cache_max_age" json:"cache_max_age"`
}

// PublicFilter is a server-owned public filter.
type PublicFilter struct {
	Field string `yaml:"field" json:"field"`
	Op    string `yaml:"op" json:"op"`
	Value any    `yaml:"value" json:"value"`
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
	d.ensureDerivedMetadata()
	return d.dataFields
}

// NonTableDataFields returns data fields stored on the parent document table.
func (d *DocType) NonTableDataFields() []Field {
	d.ensureDerivedMetadata()
	return d.nonTableDataFields
}

// TableFields returns all Table-type fields.
func (d *DocType) TableFields() []Field {
	d.ensureDerivedMetadata()
	return d.tableFields
}

// ListFields returns all fields marked for list views.
func (d *DocType) ListFields() []Field {
	d.ensureDerivedMetadata()
	return d.listFields
}

// ValidSortColumns returns the data/system columns allowed in ORDER BY clauses.
func (d *DocType) ValidSortColumns() map[string]bool {
	d.ensureDerivedMetadata()
	return d.validSortColumns
}

// RebuildDerivedMetadata refreshes cached field groups derived from Fields.
func (d *DocType) RebuildDerivedMetadata() {
	dataFields := make([]Field, 0, len(d.Fields))
	nonTableDataFields := make([]Field, 0, len(d.Fields))
	tableFields := make([]Field, 0)
	listFields := make([]Field, 0)
	validSortColumns := map[string]bool{
		"name":        true,
		"owner":       true,
		"creation":    true,
		"modified":    true,
		"modified_by": true,
		"doc_status":  true,
		"idx":         true,
	}

	for _, f := range d.Fields {
		if !f.IsDataField() {
			continue
		}
		dataFields = append(dataFields, f)
		if f.Fieldtype == "Table" {
			tableFields = append(tableFields, f)
			continue
		}
		nonTableDataFields = append(nonTableDataFields, f)
		validSortColumns[f.Fieldname] = true
		if f.InListView {
			listFields = append(listFields, f)
		}
	}

	d.dataFields = dataFields
	d.nonTableDataFields = nonTableDataFields
	d.tableFields = tableFields
	d.listFields = listFields
	d.validSortColumns = validSortColumns
	d.metadataReady = true
}

func (d *DocType) ensureDerivedMetadata() {
	if !d.metadataReady {
		d.RebuildDerivedMetadata()
	}
}

// Field represents a single field in a DocType.
type Field struct {
	Fieldname                string                  `yaml:"fieldname"           json:"fieldname"`
	Fieldtype                string                  `yaml:"fieldtype"           json:"fieldtype"`
	Label                    string                  `yaml:"label"               json:"label"`
	Options                  string                  `yaml:"options"             json:"options"`
	Reqd                     bool                    `yaml:"reqd"                json:"reqd"`
	Unique                   bool                    `yaml:"unique"              json:"unique"`
	Default                  string                  `yaml:"default"             json:"default"`
	Hidden                   bool                    `yaml:"hidden"              json:"hidden"`
	ReadOnly                 bool                    `yaml:"read_only"           json:"read_only"`
	Bold                     bool                    `yaml:"bold"                json:"bold"`
	InListView               bool                    `yaml:"in_list_view"        json:"in_list_view"`
	InStandardFilter         bool                    `yaml:"in_standard_filter"  json:"in_standard_filter"`
	SearchIndex              bool                    `yaml:"search_index"        json:"search_index"`
	Description              string                  `yaml:"description"         json:"description"`
	DependsOn                string                  `yaml:"depends_on"          json:"depends_on"`
	MandatoryDependsOn       string                  `yaml:"mandatory_depends_on" json:"mandatory_depends_on"`
	Constraints              []Constraint            `yaml:"constraints"         json:"constraints"`
	RenamedFrom              string                  `yaml:"renamed_from"        json:"renamed_from"`
	LinkedField              string                  `yaml:"linked_field"        json:"linked_field,omitempty"`
	Computed                 string                  `yaml:"computed"            json:"computed,omitempty"`
	DependencyScope          string                  `yaml:"dependency_scope"    json:"dependency_scope,omitempty"`
	Accept                   string                  `yaml:"accept"              json:"accept,omitempty"`
	Precision                *int                    `yaml:"precision,omitempty" json:"precision,omitempty"`
	ReferenceRule            *contract.ReferenceRule `yaml:"reference_rule,omitempty" json:"reference_rule,omitempty"`
	HistoricalSnapshotSchema *contract.SchemaRef     `yaml:"historical_snapshot_schema,omitempty" json:"historical_snapshot_schema,omitempty"`
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
	Type            string       `yaml:"type"            json:"type"`
	Description     string       `yaml:"description"     json:"description"`
	Predicate       string       `yaml:"predicate"       json:"predicate,omitempty"` // s-expression predicate, e.g. "(> end_date start_date)"
	Condition       string       `yaml:"condition"       json:"condition,omitempty"`
	RequireFields   []string     `yaml:"require_fields"  json:"require_fields,omitempty"`
	Field           string       `yaml:"field"           json:"field,omitempty"`
	GroupBy         []string     `yaml:"group_by"        json:"group_by,omitempty"`
	Max             float64      `yaml:"max"             json:"max,omitempty"`
	Message         string       `yaml:"message"         json:"message"`
	LHS             string       `yaml:"lhs"             json:"lhs,omitempty"`
	Operator        string       `yaml:"operator"        json:"operator,omitempty"`
	RHS             string       `yaml:"rhs"             json:"rhs,omitempty"`
	Fields          []string     `yaml:"fields"          json:"fields,omitempty"`
	StatusField     string       `yaml:"status_field"    json:"status_field,omitempty"`
	StatusValues    []string     `yaml:"status_values"   json:"status_values,omitempty"`
	ImmutableFields []string     `yaml:"immutable_fields" json:"immutable_fields,omitempty"`
	Constraints     []Constraint `yaml:"constraints"     json:"constraints,omitempty"`
	// LinkField and RelatedField are used by linked_cross_field constraints.
	// They allow a declarative rule on this document to compare a field with a
	// field on the linked document without embedding executable code.
	LinkField    string `yaml:"link_field"       json:"link_field,omitempty"`
	RelatedField string `yaml:"related_field"    json:"related_field,omitempty"`
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
		{"revision", "BIGINT"},
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

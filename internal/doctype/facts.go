package doctype

import (
	"fmt"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// FactModel declares what a record type contributes to analytics: one fact row per
// record (or one per child row when Lines is set), keyed by Time, with exact-decimal
// measures and categorical dimensions. Config only; the fact tables are built elsewhere.
// Legal entity and owner come from the record's business scope and owner, not from here.
type FactModel struct {
	Time        string          `yaml:"time" json:"time"`
	Lines       string          `yaml:"lines,omitempty" json:"lines,omitempty"`
	Measures    []FactMeasure   `yaml:"measures" json:"measures"`
	Dimensions  []FactDimension `yaml:"dimensions,omitempty" json:"dimensions,omitempty"`
	StatusField string          `yaml:"status_field,omitempty" json:"status_field,omitempty"`
	// CurrencyField names a header dimension whose value is a Currency link. Query results then list the
	// currencies their rows carry, so a caller can show a mixed-currency note. Empty means the base currency.
	CurrencyField string `yaml:"currency_field,omitempty" json:"currency_field,omitempty"`
	// ExcludeStatus lists statuses whose records have no fact rows: they are tombstoned until they
	// move to a status outside this list. Each value must be an option of StatusField.
	ExcludeStatus []string `yaml:"exclude_status,omitempty" json:"exclude_status,omitempty"`
	// Generated marks a model built from the record type's fields because its pack declares none.
	// It lives in memory only: the JSON, YAML and s-expression forms of the record type omit it.
	Generated bool `yaml:"-" json:"-"`
}

// FactMeasure is a number summed by metrics. Field is "name" on the record, or
// "lines.name" on the child table named by FactModel.Lines.
type FactMeasure struct {
	Field string `yaml:"field" json:"field"`
	Name  string `yaml:"name,omitempty" json:"name,omitempty"`
}

// FactDimension is a category a fact row can be grouped by. Field uses the same
// record or "lines.name" form as FactMeasure.
type FactDimension struct {
	Field string `yaml:"field" json:"field"`
}

// FactColumnType is the logical type of a fact column; the builder maps it to SQL.
type FactColumnType string

// Logical column types of a fact table.
const (
	FactColumnText    FactColumnType = "text"
	FactColumnDecimal FactColumnType = "decimal"
)

// FactColumn is one column of a fact table, in the order the builder creates them.
type FactColumn struct {
	Name string         `json:"name"`
	Type FactColumnType `json:"type"`
}

var (
	knownFactModelFields     = buildFieldSet(FactModel{})
	knownFactMeasureFields   = buildFieldSet(FactMeasure{})
	knownFactDimensionFields = buildFieldSet(FactDimension{})
)

// factPath is a measure or dimension path: "name" on the record, or "lines.name" on the child.
type factPath string

// leaf is the field name after any "lines." prefix.
func (p factPath) leaf() string {
	return string(p)[strings.LastIndex(string(p), ".")+1:]
}

// ColumnName is the measure's output column: Name, or the leaf of Field.
func (m FactMeasure) ColumnName() string {
	if m.Name != "" {
		return m.Name
	}
	return factPath(m.Field).leaf()
}

// ColumnName is the dimension's output column: the leaf of Field.
func (d FactDimension) ColumnName() string { return factPath(d.Field).leaf() }

// StatusColumn is the fact column that holds the value of the status field. On a model with a status field
// it is also a dimension every query can group by, without a declared dimension.
const StatusColumn = "status"

// GroupsByStatus reports whether column names the status dimension: the status field's value, on a model
// that has a status field.
func (f *FactModel) GroupsByStatus(column string) bool {
	return f != nil && f.StatusField != "" && column == StatusColumn
}

// TableName is the fact table of a record type.
func (f *FactModel) TableName(dt *DocType) string {
	return "_kora_fact_" + strings.ToLower(strings.ReplaceAll(dt.Name, " ", "_"))
}

// LineTableName is the line-level fact table of a record type with Lines set.
func (f *FactModel) LineTableName(dt *DocType) string {
	return f.TableName(dt) + "_line"
}

// Columns lists the per-dimension (text) and per-measure (decimal) columns in order.
// The fixed columns (record, line, business date, status, sequence) belong to the facts package.
func (f *FactModel) Columns() []FactColumn {
	if f == nil {
		return nil
	}
	var cols []FactColumn
	for _, d := range f.Dimensions {
		cols = append(cols, FactColumn{Name: d.ColumnName(), Type: FactColumnText})
	}
	for _, m := range f.Measures {
		cols = append(cols, FactColumn{Name: m.ColumnName(), Type: FactColumnDecimal})
	}
	return cols
}

// factChecks collects the problems found in one record type's fact model.
type factChecks struct {
	dt    *DocType
	lines *DocType
	names map[string]bool
	errs  []CrossFileError
}

func (c *factChecks) bad(field, message string) {
	c.errs = append(c.errs, CrossFileError{Code: "InvalidFacts", DocType: c.dt.Name, Field: field, Message: message})
}

// reservedFactColumns are the fixed columns of every fact table.
var reservedFactColumns = map[string]bool{
	"record_id": true, "line_id": true, "legal_entity": true, "owner": true, "business_date": true,
	"occurred_at": true, "status": true, "source_sequence": true, "deleted": true,
}

// claim records a measure or dimension column name. A reserved name or a second claim is a problem.
func (c *factChecks) claim(field, name string) {
	switch {
	case reservedFactColumns[name]:
		c.bad(field, fmt.Sprintf("facts column %q is reserved for the fact table", name))
	case c.names[name]:
		c.bad(field, fmt.Sprintf("facts column %q is declared more than once", name))
	}
	c.names[name] = true
}

// Validate checks the fact model of dt against its record types. reg maps every record
// type name to its DocType, so the lines table can be resolved.
func (f *FactModel) Validate(dt *DocType, reg map[string]*DocType) []CrossFileError {
	if f == nil {
		return nil
	}
	c := &factChecks{dt: dt, names: map[string]bool{}}
	f.checkTime(c)
	f.checkLines(c, reg)
	f.checkMeasures(c)
	f.checkDimensions(c)
	f.checkStatus(c)
	f.checkCurrency(c)
	return c.errs
}

func (f *FactModel) checkCurrency(c *factChecks) {
	if f.CurrencyField == "" {
		return
	}
	for _, d := range f.Dimensions {
		if d.ColumnName() == f.CurrencyField {
			if fld, _ := f.column(c.dt, c.lines, factPath(d.Field)); fld != nil && fld.Fieldtype == "Link" && fld.Options == "Currency" {
				return
			}
			c.bad("facts.currency_field", fmt.Sprintf("facts currency_field %q must be a Link to Currency", f.CurrencyField))
			return
		}
	}
	c.bad("facts.currency_field", fmt.Sprintf("facts currency_field %q must name a dimension", f.CurrencyField))
}

func (f *FactModel) checkTime(c *factChecks) {
	if f.Time != "creation" {
		if tf := c.dt.GetField(f.Time); tf == nil || (tf.Fieldtype != "Date" && tf.Fieldtype != "Datetime") {
			c.bad("facts.time", fmt.Sprintf("facts time %q must be a Date or Datetime field on %s, or creation", f.Time, c.dt.Name))
		}
	}
}

func (f *FactModel) checkLines(c *factChecks, reg map[string]*DocType) {
	if f.Lines == "" {
		return
	}
	lf := c.dt.GetField(f.Lines)
	if lf == nil || lf.Fieldtype != "Table" {
		c.bad("facts.lines", fmt.Sprintf("facts lines %q must be a Table field", f.Lines))
		return
	}
	if c.lines = reg[lf.Options]; c.lines == nil {
		c.bad("facts.lines", fmt.Sprintf("facts lines table %q has no record type %q", f.Lines, lf.Options))
	}
}

func (f *FactModel) checkMeasures(c *factChecks) {
	for _, m := range f.Measures {
		fld, why := f.column(c.dt, c.lines, factPath(m.Field))
		if why != "" {
			c.bad("facts.measures", why)
			continue
		}
		if fld == nil || !(fld.Fieldtype == "Int" || fld.Fieldtype == "Float" || fld.Fieldtype == "Currency" || fld.Fieldtype == "Percent") {
			c.bad("facts.measures", fmt.Sprintf("facts measure %q must be an Int, Float, Currency or Percent field", m.Field))
		}
		c.claim("facts.measures", m.ColumnName())
	}
}

func (f *FactModel) checkDimensions(c *factChecks) {
	for _, d := range f.Dimensions {
		fld, why := f.column(c.dt, c.lines, factPath(d.Field))
		if why != "" {
			c.bad("facts.dimensions", why)
			continue
		}
		if fld == nil || !(fld.Fieldtype == "Link" || fld.Fieldtype == "User" || fld.Fieldtype == "Select" || fld.Fieldtype == "Data" || fld.Fieldtype == "Check") {
			c.bad("facts.dimensions", fmt.Sprintf("facts dimension %q must be a Link, User, Select, Data or Check field", d.Field))
		}
		c.claim("facts.dimensions", d.ColumnName())
	}
}

func (f *FactModel) checkStatus(c *factChecks) {
	if f.StatusField == "" {
		if len(f.ExcludeStatus) > 0 {
			c.bad("facts.exclude_status", "facts exclude_status needs a status_field")
		}
		return
	}
	sf := c.dt.GetField(f.StatusField)
	if sf == nil || sf.Fieldtype != "Select" {
		c.bad("facts.status_field", fmt.Sprintf("facts status_field %q must be a Select field on %s", f.StatusField, c.dt.Name))
		return
	}
	for _, status := range f.ExcludeStatus {
		if !selectHasOption(sf, status) {
			c.bad("facts.exclude_status", fmt.Sprintf("facts exclude_status %q is not an option of %s", status, f.StatusField))
		}
	}
}

// column resolves a record path or a "lines.name" path. It returns a non-empty reason
// when the path cannot name a column at all (a child path without lines, or the wrong prefix).
func (f *FactModel) column(dt, lines *DocType, path factPath) (*Field, string) {
	head, leaf, dotted := strings.Cut(string(path), ".")
	if !dotted {
		return dt.GetField(string(path)), ""
	}
	if f.Lines == "" {
		return nil, fmt.Sprintf("facts field %q is a child field, but facts has no lines", path)
	}
	if head != f.Lines {
		return nil, fmt.Sprintf("facts field %q must be a field on %s or on the %s table", path, dt.Name, f.Lines)
	}
	if lines == nil {
		return nil, "" // the lines problem is already reported
	}
	return lines.GetField(leaf), ""
}

// findFactUnknownKeys walks the facts block of a DocType YAML document and returns keys
// that FactModel, FactMeasure or FactDimension do not define, with line numbers.
func findFactUnknownKeys(data []byte) []unknownKey {
	var root yamlv3.Node
	if yamlv3.Unmarshal(data, &root) != nil || len(root.Content) == 0 || root.Content[0].Kind != yamlv3.MappingNode {
		return nil
	}
	top := root.Content[0]
	var unknown []unknownKey
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "facts" {
			unknown = append(unknown, factModelUnknownKeys(top.Content[i+1])...)
		}
	}
	return unknown
}

// factModelUnknownKeys checks the facts mapping and the items of its measures and dimensions lists.
func factModelUnknownKeys(node *yamlv3.Node) []unknownKey {
	if node.Kind != yamlv3.MappingNode {
		return nil
	}
	unknown := unknownKeysIn(node, knownFactModelFields)
	for j := 0; j+1 < len(node.Content); j += 2 {
		known := factItemFields(node.Content[j].Value)
		if known == nil || node.Content[j+1].Kind != yamlv3.SequenceNode {
			continue
		}
		for _, item := range node.Content[j+1].Content {
			if item.Kind == yamlv3.MappingNode {
				unknown = append(unknown, unknownKeysIn(item, known)...)
			}
		}
	}
	return unknown
}

// factItemFields returns the keys an item of the named facts list may hold, or nil.
func factItemFields(list string) map[string]bool {
	switch list {
	case "measures":
		return knownFactMeasureFields
	case "dimensions":
		return knownFactDimensionFields
	}
	return nil
}

// unknownKeysIn lists the keys of a mapping node that known does not contain.
func unknownKeysIn(node *yamlv3.Node, known map[string]bool) []unknownKey {
	var unknown []unknownKey
	for j := 0; j+1 < len(node.Content); j += 2 {
		k := node.Content[j]
		if !known[k.Value] {
			unknown = append(unknown, unknownKey{Key: k.Value, Line: k.Line, Column: k.Column, Known: known})
		}
	}
	return unknown
}

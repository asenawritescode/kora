// Package query parses and validates declarative published queries (D2).
package query

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"gopkg.in/yaml.v3"
)

// ErrInvalidDefinition is the code reported for load-time failures.
const ErrInvalidDefinition = "query.invalid_definition"

// Filter is one entry of the AND list: a leaf (Field/Op/Value) or an OR group (Any).
type Filter struct {
	Field    string   `yaml:"field" json:"field,omitempty"`
	Op       string   `yaml:"op" json:"op,omitempty"`
	Value    any      `yaml:"value" json:"value,omitempty"`
	Any      []Filter `yaml:"any" json:"any,omitempty"`
	Optional bool     `yaml:"optional" json:"optional,omitempty"`
}

// InputSpec describes one call input.
type InputSpec struct {
	Type      string   `yaml:"type" json:"type,omitempty"`
	MaxLength int      `yaml:"max_length" json:"max_length,omitempty"`
	Enum      []string `yaml:"enum" json:"enum,omitempty"`
	// Default stands in when the caller omits the input: a literal of the
	// input's type, or $today, $now or $actor.
	Default any `yaml:"default" json:"default,omitempty"`
}

// checkDefault reports a default that is not a literal of the input's type or
// a supported reference.
func (s InputSpec) checkDefault() error {
	var ok bool
	switch v := s.Default.(type) {
	case nil:
		return nil
	case string:
		if strings.HasPrefix(v, "$") && v != "$today" && v != "$now" && v != "$actor" {
			return fmt.Errorf("default %q is not a supported reference", v)
		}
		ok = s.Type == "string"
	case int, int64:
		ok = s.Type == "int" || s.Type == "number"
	case float64:
		ok = s.Type == "number" || s.Type == "int" && v == math.Trunc(v)
	case bool:
		ok = s.Type == "bool"
	}
	if !ok {
		return fmt.Errorf("default %v does not match type %s", s.Default, s.Type)
	}
	return nil
}

// RequiredInputs lists the inputs a call must supply: those a non-optional
// filter reads and that declare no default. It mirrors what the executor
// rejects, so the published schema and the runtime agree.
func (d *Definition) RequiredInputs() []string {
	seen := map[string]bool{}
	var walk func(Filter)
	walk = func(f Filter) {
		for _, c := range f.Any {
			walk(c)
		}
		if name, ok := strings.CutPrefix(fmt.Sprint(f.Value), "$input."); ok && !f.Optional && f.Op != "is_null" && d.Input[name].Default == nil {
			seen[name] = true
		}
	}
	for _, f := range d.Filters {
		walk(f)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Sort is one ordering key.
type Sort struct {
	Field string `yaml:"field" json:"field,omitempty"`
	Dir   string `yaml:"dir" json:"dir,omitempty"`
}

// Aggregate is one output column computed over the grouped rows.
type Aggregate struct {
	Name  string `yaml:"name" json:"name,omitempty"`
	Fn    string `yaml:"fn" json:"fn,omitempty"`
	Field string `yaml:"field" json:"field,omitempty"`
}

// Definition is a query shipped in queries/*.yaml.
type Definition struct {
	Name            string               `yaml:"name" json:"name,omitempty"`
	ModuleOwnerID   string               `yaml:"module_owner_id" json:"module_owner_id,omitempty"`
	Capability      string               `yaml:"capability" json:"capability,omitempty"`
	Operation       string               `yaml:"operation" json:"operation,omitempty"`
	Source          string               `yaml:"source" json:"source,omitempty"`
	Via             string               `yaml:"via" json:"via,omitempty"`
	Permission      string               `yaml:"permission" json:"permission,omitempty"`
	Scope           string               `yaml:"scope" json:"scope,omitempty"`
	OwnerField      string               `yaml:"owner_field" json:"owner_field,omitempty"`
	Input           map[string]InputSpec `yaml:"input" json:"input,omitempty"`
	Filters         []Filter             `yaml:"filters" json:"filters,omitempty"`
	Fields          []string             `yaml:"fields" json:"fields,omitempty"`
	GroupBy         []string             `yaml:"group_by" json:"group_by,omitempty"`
	Aggregates      []Aggregate          `yaml:"aggregates" json:"aggregates,omitempty"`
	Sort            []Sort               `yaml:"sort" json:"sort,omitempty"`
	Limit           int                  `yaml:"limit" json:"limit,omitempty"`
	OfflineEligible bool                 `yaml:"offline_eligible" json:"offline_eligible,omitempty"`
	Presentation    map[string]string    `yaml:"presentation" json:"presentation,omitempty"`
}

// ParentDocType returns the parent DocType of a child source ("" otherwise).
func (d *Definition) ParentDocType() string {
	parent, _, _ := strings.Cut(d.Via, ".")
	return parent
}

// OutputFields lists the row keys a run returns: fields, or group_by then aggregate names.
func (d *Definition) OutputFields() []string {
	if len(d.Fields) > 0 {
		return d.Fields
	}
	out := append([]string(nil), d.GroupBy...)
	for _, a := range d.Aggregates {
		out = append(out, a.Name)
	}
	return out
}

// CatalogRef is owner~capability~operation.
func (d *Definition) CatalogRef() string {
	return d.ModuleOwnerID + "~" + d.Capability + "~" + d.Operation
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

var (
	operators   = map[string]bool{"eq": true, "ne": true, "lt": true, "lte": true, "gt": true, "gte": true, "in": true, "contains": true, "starts_with": true, "is_null": true}
	inputTypes  = map[string]bool{"string": true, "int": true, "number": true, "bool": true}
	systemCols  = map[string]bool{"name": true, "owner": true, "creation": true, "modified": true, "modified_by": true, "doc_status": true, "idx": true}
	bannedTypes = map[string]bool{"Password": true, "JSON": true, "Table": true}
)

// Parse decodes strictly (unknown keys rejected) and validates against reg.
func Parse(raw []byte, reg *doctype.Registry) (*Definition, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var d Definition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrInvalidDefinition, err)
	}
	if err := d.validate(reg); err != nil {
		return nil, fmt.Errorf("%s: query %q: %w", ErrInvalidDefinition, d.Name, err)
	}
	return &d, nil
}

// LoadDir parses every yaml file in dir; a missing dir yields none.
func LoadDir(dir string, reg *doctype.Registry) ([]*Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && (strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var defs []*Definition
	seen := map[string]string{}
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n, err)
		}
		d, err := Parse(raw, reg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if prev, dup := seen[d.CatalogRef()]; dup {
			return nil, fmt.Errorf("%s: %s: duplicate catalog ref %s (also %s)", n, ErrInvalidDefinition, d.CatalogRef(), prev)
		}
		seen[d.CatalogRef()] = n
		defs = append(defs, d)
	}
	return defs, nil
}

func (d *Definition) validate(reg *doctype.Registry) error {
	for k, v := range map[string]string{"name": d.Name, "module_owner_id": d.ModuleOwnerID, "capability": d.Capability, "operation": d.Operation, "source": d.Source, "permission": d.Permission} {
		if v == "" {
			return fmt.Errorf("missing %s", k)
		}
	}
	if d.Scope != "tenant" && d.Scope != "legal_entity" {
		return fmt.Errorf("scope must be tenant or legal_entity, got %q", d.Scope)
	}
	if d.Limit == 0 {
		d.Limit = defaultLimit
	}
	if d.Limit < 0 || d.Limit > maxLimit {
		return fmt.Errorf("limit must be 1..%d, got %d", maxLimit, d.Limit)
	}
	dt := reg.Get(d.Source)
	if dt == nil {
		return fmt.Errorf("source doctype %q not found", d.Source)
	}
	if err := d.validateVia(dt, reg); err != nil {
		return err
	}
	check := func(ctx, f string) error {
		if systemCols[f] || (d.Via != "" && f == "parent") {
			return nil
		}
		if pf, ok := strings.CutPrefix(f, "parent."); ok && d.Via != "" && ctx == "filters" {
			return checkField(reg.Get(d.ParentDocType()), ctx, f, pf)
		}
		return checkField(dt, ctx, f, f)
	}
	if err := d.validateAggregates(dt, check); err != nil {
		return err
	}
	return d.validateRest(dt, check)
}

// checkField validates column name against dt for a selectable field.
func checkField(dt *doctype.DocType, ctx, shown, name string) error {
	if systemCols[name] {
		return nil
	}
	fld := dt.GetField(name)
	if fld == nil {
		return fmt.Errorf("%s: field %q not on %s", ctx, shown, dt.Name)
	}
	if bannedTypes[fld.Fieldtype] {
		return fmt.Errorf("%s: field %q has unselectable type %s", ctx, shown, fld.Fieldtype)
	}
	return nil
}

// validVia checks the via/is_child_table pairing and the parent Table field.
func (d *Definition) validateVia(dt *doctype.DocType, reg *doctype.Registry) error {
	if !dt.IsChildTable {
		if d.Via != "" {
			return fmt.Errorf("via is only allowed on a child-table source")
		}
		return nil
	}
	parent, field, ok := strings.Cut(d.Via, ".")
	if !ok || parent == "" || field == "" {
		return fmt.Errorf("child source %q needs via: Parent.table_field", d.Source)
	}
	pdt := reg.Get(parent)
	if pdt == nil {
		return fmt.Errorf("via: parent doctype %q not found", parent)
	}
	if f := pdt.GetField(field); f == nil || f.Fieldtype != "Table" || f.Options != d.Source {
		return fmt.Errorf("via: %s is not a Table field with options %q", d.Via, d.Source)
	}
	if d.OwnerField != "" {
		return fmt.Errorf("owner_field is not allowed on child sources")
	}
	return nil
}

func (d *Definition) validateRest(dt *doctype.DocType, check func(string, string) error) error {
	if len(d.Fields) == 0 {
		return fmt.Errorf("fields must not be empty")
	}
	for _, f := range d.Fields {
		if len(d.Aggregates) > 0 {
			break
		}
		if err := check("fields", f); err != nil {
			return err
		}
	}
	if d.OwnerField != "" {
		if err := check("owner_field", d.OwnerField); err != nil {
			return err
		}
	}
	for _, s := range d.Sort {
		if err := check("sort", s.Field); err != nil {
			return err
		}
		if len(d.Aggregates) > 0 && !contains(d.GroupBy, s.Field) {
			return fmt.Errorf("sort: %q is not a group_by field", s.Field)
		}
		if s.Dir != "" && s.Dir != "asc" && s.Dir != "desc" {
			return fmt.Errorf("sort: dir must be asc or desc, got %q", s.Dir)
		}
	}
	for name, in := range d.Input {
		if !inputTypes[in.Type] {
			return fmt.Errorf("input %q: unsupported type %q", name, in.Type)
		}
		if err := in.checkDefault(); err != nil {
			return fmt.Errorf("input %q: %w", name, err)
		}
	}
	for _, f := range d.Filters {
		if err := d.validateFilter(f, check, false); err != nil {
			return err
		}
	}
	return nil
}

var (
	aggNameRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	numericType = map[string]bool{"Int": true, "Float": true, "Currency": true, "Percent": true}
)

func contains(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}

// validateAggregates checks group_by/aggregates and fills Fields when omitted.
func (d *Definition) validateAggregates(dt *doctype.DocType, check func(string, string) error) error {
	if len(d.Aggregates) == 0 {
		if len(d.GroupBy) > 0 {
			return fmt.Errorf("group_by requires aggregates")
		}
		return nil
	}
	if len(d.Aggregates) > 8 || len(d.GroupBy) > 3 {
		return fmt.Errorf("aggregates allow at most 8 entries and group_by 3")
	}
	out := append([]string{}, d.GroupBy...)
	for _, g := range d.GroupBy {
		if err := check("group_by", g); err != nil {
			return err
		}
	}
	for _, a := range d.Aggregates {
		if !aggNameRe.MatchString(a.Name) || contains(out, a.Name) {
			return fmt.Errorf("aggregates: name %q is invalid or not unique", a.Name)
		}
		switch a.Fn {
		case "count":
			if a.Field != "" {
				return fmt.Errorf("aggregates: count %q takes no field", a.Name)
			}
		case "sum":
			f := dt.GetField(a.Field)
			if f == nil || !numericType[f.Fieldtype] {
				return fmt.Errorf("aggregates: sum %q needs a numeric field, got %q", a.Name, a.Field)
			}
		default:
			return fmt.Errorf("aggregates: unknown fn %q", a.Fn)
		}
		out = append(out, a.Name)
	}
	if len(d.Fields) == 0 {
		d.Fields = out
	} else if strings.Join(d.Fields, ",") != strings.Join(out, ",") {
		return fmt.Errorf("fields must equal group_by + aggregate names %v", out)
	}
	return nil
}

func (d *Definition) validateFilter(f Filter, check func(string, string) error, nested bool) error {
	if len(f.Any) > 0 {
		if nested {
			return fmt.Errorf("filters: nested any groups are not supported")
		}
		if f.Field != "" || f.Op != "" {
			return fmt.Errorf("filters: any group cannot also set field/op")
		}
		for _, c := range f.Any {
			if err := d.validateFilter(c, check, true); err != nil {
				return err
			}
		}
		return nil
	}
	if !operators[f.Op] {
		return fmt.Errorf("filters: unknown operator %q", f.Op)
	}
	if err := check("filters", f.Field); err != nil {
		return err
	}
	if s, ok := f.Value.(string); ok && strings.HasPrefix(s, "$field.") {
		switch f.Op {
		case "eq", "ne", "lt", "lte", "gt", "gte":
		default:
			return fmt.Errorf("filters: %s not allowed with operator %q", s, f.Op)
		}
		if f.Optional {
			return fmt.Errorf("filters: %s cannot be optional", s)
		}
		if err := check("filters value", strings.TrimPrefix(s, "$field.")); err != nil {
			return err
		}
	}
	if s, ok := f.Value.(string); ok && strings.HasPrefix(s, "$input.") {
		if _, ok := d.Input[strings.TrimPrefix(s, "$input.")]; !ok {
			return fmt.Errorf("filters: %s references undeclared input", s)
		}
	}
	return nil
}

// Encode serialises definitions for the tenant config snapshot.
func Encode(defs []*Definition) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(defs))
	for _, d := range defs {
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("encode query %q: %w", d.Name, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// Decode parses and validates snapshot queries against reg (JSON is valid YAML).
func Decode(raws []json.RawMessage, reg *doctype.Registry) ([]*Definition, error) {
	defs := make([]*Definition, 0, len(raws))
	seen := map[string]bool{}
	for _, raw := range raws {
		d, err := Parse(raw, reg)
		if err != nil {
			return nil, err
		}
		if seen[d.CatalogRef()] {
			return nil, fmt.Errorf("%s: duplicate catalog ref %s", ErrInvalidDefinition, d.CatalogRef())
		}
		seen[d.CatalogRef()] = true
		defs = append(defs, d)
	}
	return defs, nil
}
